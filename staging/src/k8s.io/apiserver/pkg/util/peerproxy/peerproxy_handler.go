/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package peerproxy

import (
	"context"
	"errors"
	"fmt"
	"crypto/rand"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"k8s.io/api/apiserverinternal/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	epmetrics "k8s.io/apiserver/pkg/endpoints/metrics"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/endpoints/responsewriter"
	"k8s.io/apiserver/pkg/reconcilers"
	"k8s.io/apiserver/pkg/storageversion"
	"k8s.io/apiserver/pkg/util/peerproxy/metrics"
	apiserverproxyutil "k8s.io/apiserver/pkg/util/proxy"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/transport"
	"k8s.io/klog/v2"
)

const (
	PeerProxiedHeader = "x-kubernetes-peer-proxied"
)

type peerProxyHandler struct {
	name string
	// StorageVersion informer used to fetch apiserver ids than can serve a resource
	storageversionInformer cache.SharedIndexInformer

	// StorageVersion manager used to ensure it has finished updating storageversions before
	// we start handling external requests
	storageversionManager storageversion.Manager

	// proxy transport
	proxyTransport http.RoundTripper

	// identity for this server
	serverId string

	// reconciler that is used to fetch host port of peer apiserver when proxying request to a peer
	reconciler reconcilers.PeerEndpointLeaseReconciler

	serializer runtime.NegotiatedSerializer

	// SyncMap for storing an up to date copy of the storageversions and apiservers that can serve them
	// This map is populated using the StorageVersion informer
	// This map has key set to GVR and value being another SyncMap
	// The nested SyncMap has key set to apiserver id and value set to boolean
	// The nested maps are created to have a "Set" like structure to store unique apiserver ids
	// for a given GVR
	svMap sync.Map

	finishedSync atomic.Bool
}

// responder implements rest.Responder for assisting a connector in writing objects or errors.
type responder struct {
	w   http.ResponseWriter
	ctx context.Context
}

func (h *peerProxyHandler) HasFinishedSync() bool {
	return h.finishedSync.Load()
}

func (h *peerProxyHandler) WaitForCacheSync(stopCh <-chan struct{}) error {

	ok := cache.WaitForNamedCacheSync("unknown-version-proxy", stopCh, h.storageversionInformer.HasSynced, h.storageversionManager.Completed)
	if !ok {
		return fmt.Errorf("error while waiting for initial cache sync")
	}
	klog.V(3).Infof("setting finishedSync to true")
	h.finishedSync.Store(true)
	return nil
}

// WrapHandler will fetch the apiservers that can serve the request and either serve it locally
// or route it to a peer
func (h *peerProxyHandler) WrapHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		requestInfo, ok := apirequest.RequestInfoFrom(ctx)

		if !ok {
			responsewriters.InternalError(w, r, errors.New("no RequestInfo found in the context"))
			return
		}

		// Allow non-resource requests
		if !requestInfo.IsResourceRequest {
			klog.V(3).Infof("Not a resource request skipping proxying")
			handler.ServeHTTP(w, r)
			return
		}

		// Request has already been proxied once, it must be served locally
		if r.Header.Get(PeerProxiedHeader) == "true" {
			klog.V(3).Infof("Already rerouted once, skipping proxying to peer")
			handler.ServeHTTP(w, r)
			return
		}

		// StorageVersion Informers and/or StorageVersionManager is not synced yet, pass request to next handler
		// This will happen for self requests from the kube-apiserver because we have a poststarthook
		// to ensure that external requests are not served until the StorageVersion Informer and
		// StorageVersionManager has synced
		if !h.HasFinishedSync() {
			handler.ServeHTTP(w, r)
			return
		}

		gvr := schema.GroupVersionResource{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion, Resource: requestInfo.Resource}
		if requestInfo.APIGroup == "" {
			gvr.Group = "core"
		}

		apiservers, err := h.findServiceableByServers(gvr)
		if err != nil {
			// resource wasn't found in SV informer cache which means that resource is an aggregated API
			// or a CR. This situation is ok to be handled by local handler.
			handler.ServeHTTP(w, r)
			return
		}

		locallyServiceable, peerEndpoints, err := h.resolveServingLocation(apiservers)
		if err != nil {
			gv := schema.GroupVersion{Group: gvr.Group, Version: gvr.Version}
			klog.ErrorS(err, "error finding serviceable-by apiservers for the requested resource", "gvr", gvr)
			responsewriters.ErrorNegotiated(apierrors.NewServiceUnavailable("Error getting ip and port info of the remote server while proxying"), h.serializer, gv, w, r)
			return
		}

		// pass request to the next handler if found the gvr locally.
		// TODO: maintain locally serviceable GVRs somewhere so that we dont have to
		// consult the storageversion-informed map for those
		if locallyServiceable {
			handler.ServeHTTP(w, r)
			return
		}

		if len(peerEndpoints) == 0 {
			klog.Errorf("gvr %v is not served by anything in this cluster", gvr)
			handler.ServeHTTP(w, r)
			return
		}

		// otherwise, randomly select an apiserver and proxy request to it
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(peerEndpoints))))
		if err != nil {
			klog.Errorf("Failed to generate secure random index: %v", err)
			handler.ServeHTTP(w, r)
			return
		}
		destServerHostPort := peerEndpoints[idx.Int64()]
		h.proxyRequestToDestinationAPIServer(r, w, destServerHostPort)
	})
}

func (h *peerProxyHandler) findServiceableByServers(gvr schema.GroupVersionResource) (*sync.Map, error) {
	apiserversi, ok := h.svMap.Load(gvr)
	if !ok || apiserversi == nil {
		return nil, fmt.Errorf("no storageVersions found for the GVR: %v", gvr)
	}

	apiservers, _ := apiserversi.(*sync.Map)
	return apiservers, nil
}

func (h *peerProxyHandler) resolveServingLocation(apiservers *sync.Map) (bool, []string, error) {
	var peerServerEndpoints []string
	var locallyServiceable bool
	var respErr error

	apiservers.Range(func(key, value interface{}) bool {
		apiserverKey := key.(string)
		if apiserverKey == h.serverId {
			locallyServiceable = true
			// stop iteration and reset any errors encountered so far.
			respErr = nil
			return false
		}

		hostPort, err := h.hostportInfo(apiserverKey)
		if err != nil {
			respErr = err
			// continue with iteration
			return true
		}

		peerServerEndpoints = append(peerServerEndpoints, hostPort)
		return true
	})

	// reset err if there was atleast one valid peer server found.
	if len(peerServerEndpoints) > 0 {
		respErr = nil
	}

	return locallyServiceable, peerServerEndpoints, respErr
}

func (h *peerProxyHandler) hostportInfo(apiserverKey string) (string, error) {
	hostport, err := h.reconciler.GetEndpoint(apiserverKey)
	if err != nil {
		return "", err
	}
	// check ip format
	_, _, err = net.SplitHostPort(hostport)
	if err != nil {
		return "", err
	}

	return hostport, nil
}

func (h *peerProxyHandler) proxyRequestToDestinationAPIServer(req *http.Request, rw http.ResponseWriter, host string) {
	user, ok := apirequest.UserFrom(req.Context())
	if !ok {
		klog.Error("failed to get user info from request")
		return
	}

	// write a new location based on the existing request pointed at the target service
	location := &url.URL{}
	location.Scheme = "https"
	location.Host = host
	location.Path = req.URL.Path
	location.RawQuery = req.URL.Query().Encode()

	newReq, cancelFn := apiserverproxyutil.NewRequestForProxy(location, req)
	newReq.Header.Add(PeerProxiedHeader, "true")
	defer cancelFn()

	proxyRoundTripper := transport.NewAuthProxyRoundTripper(user.GetName(), user.GetUID(), user.GetGroups(), user.GetExtra(), h.proxyTransport)
	delegate := &epmetrics.ResponseWriterDelegator{ResponseWriter: rw}
	w := responsewriter.WrapForHTTP1Or2(delegate)

	handler := proxy.NewUpgradeAwareHandler(location, proxyRoundTripper, true, false, &responder{w: w, ctx: req.Context()})
	handler.ServeHTTP(w, newReq)
	metrics.IncPeerProxiedRequest(req.Context(), strconv.Itoa(delegate.Status()))
}

func (r *responder) Error(w http.ResponseWriter, req *http.Request, err error) {
	klog.ErrorS(err, "Error while proxying request to destination apiserver")
	http.Error(w, err.Error(), http.StatusServiceUnavailable)
}

// Adds a storageversion object to SVMap
func (h *peerProxyHandler) addSV(obj interface{}) {
	sv, ok := obj.(*v1alpha1.StorageVersion)
	if !ok {
		klog.Error("Invalid StorageVersion provided to addSV()")
		return
	}
	h.updateSVMap(nil, sv)
}

// Updates the SVMap to delete old storageversion and add new storageversion
func (h *peerProxyHandler) updateSV(oldObj interface{}, newObj interface{}) {
	oldSV, ok := oldObj.(*v1alpha1.StorageVersion)
	if !ok {
		klog.Error("Invalid StorageVersion provided to updateSV()")
		return
	}

	newSV, ok := newObj.(*v1alpha1.StorageVersion)
	if !ok {
		klog.Error("Invalid StorageVersion provided to updateSV()")
		return
	}

	h.updateSVMap(oldSV, newSV)
}

// Deletes a storageversion object from SVMap
func (h *peerProxyHandler) deleteSV(obj interface{}) {
	sv, ok := obj.(*v1alpha1.StorageVersion)
	if !ok {
		klog.Error("Invalid StorageVersion provided to deleteSV()")
		return
	}

	h.updateSVMap(sv, nil)
}

// Delete old storageversion, add new storagversion
func (h *peerProxyHandler) updateSVMap(oldSV *v1alpha1.StorageVersion, newSV *v1alpha1.StorageVersion) {
	if oldSV != nil {
		h.deleteSVFromMap(oldSV)
	}

	if newSV != nil {
		h.addSVToMap(newSV)
	}
}

func (h *peerProxyHandler) deleteSVFromMap(sv *v1alpha1.StorageVersion) {
	// The name of storageversion is <group>.<resource>
	splitInd := strings.LastIndex(sv.Name, ".")
	group := sv.Name[:splitInd]
	resource := sv.Name[splitInd+1:]

	gvr := schema.GroupVersionResource{Group: group, Resource: resource}
	for _, gr := range sv.Status.StorageVersions {
		for _, version := range gr.ServedVersions {
			versionSplit := strings.Split(version, "/")
			if len(versionSplit) == 2 {
				version = versionSplit[1]
			}
			gvr.Version = version
			h.svMap.Delete(gvr)
		}
	}
}

func (h *peerProxyHandler) addSVToMap(sv *v1alpha1.StorageVersion) {
	// The name of storageversion is <group>.<resource>
	splitInd := strings.LastIndex(sv.Name, ".")
	group := sv.Name[:splitInd]
	resource := sv.Name[splitInd+1:]

	gvr := schema.GroupVersionResource{Group: group, Resource: resource}
	for _, gr := range sv.Status.StorageVersions {
		for _, version := range gr.ServedVersions {

			// some versions have groups included in them, so get rid of the groups
			versionSplit := strings.Split(version, "/")
			if len(versionSplit) == 2 {
				version = versionSplit[1]
			}
			gvr.Version = version
			apiserversi, _ := h.svMap.LoadOrStore(gvr, &sync.Map{})
			apiservers := apiserversi.(*sync.Map)
			apiservers.Store(gr.APIServerID, true)
		}
	}
}
