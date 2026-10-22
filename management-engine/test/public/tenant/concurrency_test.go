// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tenant_test exercises API writes against the actual controller checkpoint protocol.
package tenant_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvkecontrollerapi "github.com/dsx-ai-factory/nvke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/controllers"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/nvkecontroller"
	tenantcluster "github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/apis/cluster"
	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

// TestControllerCheckpointConcurrency proves spec corrections cannot bypass the provisioning checkpoint.
func TestControllerCheckpointConcurrency(t *testing.T) {
	config := startDatastore(t)
	scheme := runtime.NewScheme()
	require.NoError(t, internalv1.AddToScheme(scheme))
	kube, err := client.New(config, client.Options{Scheme: scheme})
	require.NoError(t, err)

	t.Run("API wins before checkpoint so stale controller cannot provision", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		api := newService(t, config)
		cluster := newCluster(t, ctx, kube, "api-wins")
		backend := newBackend(t, http.StatusCreated)
		var corrected *clusterv1.Cluster
		controllerConfig := intercept(config, func(request *http.Request) bool {
			return request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/status")
		}, func() {
			var updateErr error
			corrected, updateErr = api.ReplaceCluster(ctx, replacement(cluster.Name, cluster.ResourceVersion))
			require.NoError(t, updateErr)
		})
		reconciler := newReconciler(t, controllerConfig, scheme, backend)
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		require.True(t, apierrors.IsConflict(err), "expected stale status checkpoint conflict, got %v", err)
		require.NotNil(t, corrected)
		require.Equal(t, int32(0), backend.posts.Load(), "stale spec must never reach the NVKE controller")
		var stored internalv1.NVKECluster
		require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(cluster), &stored))
		require.Equal(t, internalv1.ResourceName("alternate"), stored.Spec.ControlPlaneClass)
		require.Nil(t, stored.Status.Provisioning)
	})

	for _, tc := range []struct{ name, class string }{
		{name: "changed spec", class: "alternate"},
		{name: "unchanged spec", class: "standard"},
	} {
		t.Run("checkpoint wins after API read so merge conflicts/"+tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cluster := newCluster(t, ctx, kube, "controller-wins-"+tc.class)
			backend := newBackend(t, http.StatusCreated)
			reconciler := newReconciler(t, config, scheme, backend)
			apiConfig := intercept(config, func(request *http.Request) bool {
				return request.Method == http.MethodPatch && !strings.HasSuffix(request.URL.Path, "/status")
			}, func() {
				_, reconcileErr := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
				require.NoError(t, reconcileErr)
			})
			api := newService(t, apiConfig)
			request := replacement(cluster.Name, cluster.ResourceVersion)
			request.Spec.ControlPlaneClass = new(tc.class)
			_, err := api.ReplaceCluster(ctx, request)
			require.Equal(t, codes.Aborted, status.Code(err))
			require.Equal(t, int32(1), backend.posts.Load())
			var stored internalv1.NVKECluster
			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(cluster), &stored))
			require.Equal(t, internalv1.ResourceName("standard"), stored.Spec.ControlPlaneClass)
			require.NotNil(t, stored.Status.Provisioning)
			request.ResourceVersion = stored.ResourceVersion
			_, err = api.ReplaceCluster(ctx, request)
			require.Equal(t, codes.FailedPrecondition, status.Code(err))
		})
	}

	for _, tc := range []struct {
		name     string
		response int
		editable bool
	}{
		{name: "rejected-create", response: http.StatusBadRequest, editable: true},
		{name: "unknown-outcome", response: http.StatusGatewayTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cluster := newCluster(t, ctx, kube, tc.name)
			backend := newBackend(t, tc.response)
			reconciler := newReconciler(t, config, scheme, backend)
			_, _ = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
			require.Equal(t, int32(1), backend.posts.Load())
			var stored internalv1.NVKECluster
			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(cluster), &stored))
			api := newService(t, config)
			_, err := api.ReplaceCluster(ctx, replacement(cluster.Name, stored.ResourceVersion))
			if tc.editable {
				require.Nil(t, stored.Status.Provisioning)
				require.NoError(t, err)
			} else {
				require.NotNil(t, stored.Status.Provisioning)
				require.Empty(t, stored.Status.Provisioning.ID, "an empty ID must still block changes")
				require.Equal(t, codes.FailedPrecondition, status.Code(err))
			}
		})
	}

	t.Run("deleting cluster cannot be corrected", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		cluster := newCluster(t, ctx, kube, "deleting")
		require.NoError(t, kube.Delete(ctx, cluster))
		require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(cluster), cluster))
		require.False(t, cluster.DeletionTimestamp.IsZero())
		_, err := newService(t, config).ReplaceCluster(ctx, replacement(cluster.Name, cluster.ResourceVersion))
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
	})
}

// newService binds the production API to real storage for persistence and concurrency checks.
func newService(t *testing.T, config *rest.Config) *tenantcluster.Service {
	t.Helper()
	dynamicClient, err := dynamic.NewForConfig(config)
	require.NoError(t, err)
	api, err := tenantcluster.New(dynamicClient, tenantcluster.Config{
		Namespace: "default", DatastoreID: "envtest",
		PageTokenKey: []byte(strings.Repeat("test", 8)),
	})
	require.NoError(t, err)
	return api
}

// newCluster persists supported desired state for controlled API and reconciliation races.
func newCluster(t *testing.T, ctx context.Context, kube client.Client, name string) *internalv1.NVKECluster {
	t.Helper()
	cluster := &internalv1.NVKECluster{
		Name: name, Namespace: "default", Finalizers: []string{"internal.nvke.nvidia.com/cluster-cleanup"},
		Spec: internalv1.NVKEClusterSpec{
			Release:           internalv1.ReleaseSelection{Channel: internalv1.ReleaseChannelReference{Name: "stable"}},
			ControlPlaneClass: "standard",
			ClusterNetwork: internalv1.ClusterNetwork{
				Pods:          internalv1.NetworkRanges{CIDRBlocks: []internalv1.NetworkCIDR{"10.244.0.0/16"}},
				Services:      internalv1.NetworkRanges{CIDRBlocks: []internalv1.NetworkCIDR{"10.96.0.0/12"}},
				ServiceDomain: "cluster.local",
			},
		},
	}
	require.NoError(t, kube.Create(ctx, cluster))
	return cluster
}

// replacement supplies a complete writable spec and version guard for correction attempts.
func replacement(name, version string) *clusterv1.ReplaceClusterRequest {
	return &clusterv1.ReplaceClusterRequest{
		Name: name, ResourceVersion: version, Spec: &clusterv1.ReplaceClusterSpec{
			Release:           &clusterv1.ReplaceRelease{Channel: &clusterv1.ReplaceReleaseChannel{Name: new("stable")}},
			ControlPlaneClass: new("alternate"),
		},
	}
}

type provisioningBackend struct {
	client *nvkecontroller.Client
	posts  atomic.Int32
}

// newBackend controls provisioning outcomes and counts calls through the production NVKE controller client.
func newBackend(t *testing.T, response int) *provisioningBackend {
	t.Helper()
	backend := &provisioningBackend{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodGet:
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"clusters": []any{}}))
		case http.MethodPost:
			backend.posts.Add(1)
			var input nvkecontrollerapi.CreateClusterRequest
			if !assert.NoError(t, json.NewDecoder(request.Body).Decode(&input)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(response)
			if response == http.StatusCreated {
				assert.NoError(t, json.NewEncoder(w).Encode(nvkecontrollerapi.Cluster{ID: uuid.New(), Name: input.Name}))
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	var err error
	backend.client, err = nvkecontroller.New(nvkecontroller.Config{Endpoint: server.URL})
	require.NoError(t, err)
	return backend
}

// newReconciler wires the production controller to real storage and the controlled provisioning backend.
func newReconciler(
	t *testing.T,
	config *rest.Config,
	scheme *runtime.Scheme,
	backend *provisioningBackend,
) *controllers.NVKEClusterReconciler {
	t.Helper()
	kube, err := client.New(config, client.Options{Scheme: scheme})
	require.NoError(t, err)
	fixture := filepath.Join(
		"..", "..", "..", "controllers", "testdata", "nvkecluster-create-nvke-controller-pending", "input_bindings.yaml",
	)
	bindings, err := controllers.LoadNVKEControllerBindings(fixture)
	require.NoError(t, err)
	return &controllers.NVKEClusterReconciler{
		Client: kube, NVKEController: backend.client, Bindings: bindings, PollInterval: time.Second,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip adapts the test hook to HTTP so races occur at real datastore request boundaries.
func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// intercept injects one competing operation immediately before the selected datastore request.
func intercept(config *rest.Config, matches func(*http.Request) bool, before func()) *rest.Config {
	copyConfig := rest.CopyConfig(config)
	var fired atomic.Bool
	copyConfig.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if matches(request) && fired.CompareAndSwap(false, true) {
				before()
			}
			return next.RoundTrip(request)
		})
	})
	return copyConfig
}
