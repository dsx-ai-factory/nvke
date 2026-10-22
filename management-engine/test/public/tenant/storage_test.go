// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Real datastore tests verify persistence, concurrency, projection and the API identity's permissions.
package tenant_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	tenantcluster "github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/apis/cluster"
	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

// TestPublicClusterPersistence exercises public operations against Kubernetes admission and RBAC.
func TestPublicClusterPersistence(t *testing.T) {
	config := startDatastore(t)
	api := newService(t, config)
	client, err := dynamic.NewForConfig(config)
	require.NoError(t, err)
	resource := client.Resource(resourceGVR()).Namespace("default")

	t.Run("duplicate create requests persist one cluster", func(t *testing.T) {
		ctx := t.Context()
		requests := []*clusterv1.CreateClusterRequest{storageCreate("create-race"), storageCreate("create-race")}
		var wait sync.WaitGroup
		errors := make(chan error, 2)
		for _, request := range requests {
			wait.Go(func() { _, err := api.CreateCluster(ctx, request); errors <- err })
		}
		wait.Wait()
		close(errors)
		results := map[codes.Code]int{}
		for err := range errors {
			results[status.Code(err)]++
		}
		assert.Equal(t, map[codes.Code]int{codes.OK: 1, codes.AlreadyExists: 1}, results)
	})

	t.Run("changed merge preserves private state and returns persisted version", func(t *testing.T) {
		ctx := t.Context()
		request := storageCreate("roundtrip")
		request.Spec.Release.Channel.Version = new("v1")
		created, err := api.CreateCluster(ctx, request)
		require.NoError(t, err)
		require.NotEmpty(t, created.Uid)
		require.Equal(t, int64(1), created.Generation)
		_, err = resource.Patch(ctx, created.Name, types.MergePatchType,
			[]byte(`{"metadata":{"annotations":{"example.com/private":"PRIVATE"}},
			"spec":{"acceleratedRuntime":{"name":"none"}}}`), metav1.PatchOptions{FieldValidation: "Strict"})
		require.NoError(t, err)
		stored, err := resource.Get(ctx, created.Name, metav1.GetOptions{})
		require.NoError(t, err)
		stored.Object["status"] = map[string]any{"conditions": []any{map[string]any{
			"type": "Ready", "status": "True", "observedGeneration": stored.GetGeneration(),
			"reason": "Ready", "message": "PRIVATE", "lastTransitionTime": "2026-10-10T00:00:00Z",
		}}}
		stored, err = resource.UpdateStatus(ctx, stored, metav1.UpdateOptions{})
		require.NoError(t, err)
		current, err := api.GetCluster(ctx, &clusterv1.GetClusterRequest{Name: created.Name})
		require.NoError(t, err)
		require.Equal(t, clusterv1.ClusterStatus_STATE_READY, current.Status.State)
		require.Equal(t, "v1", current.Spec.Release.Channel.GetVersion())
		replaced, err := api.ReplaceCluster(ctx, replacement(created.Name, current.ResourceVersion))
		require.NoError(t, err)
		assert.Equal(t, clusterv1.ClusterStatus_STATE_PENDING, replaced.Status.State,
			"old ready observation cannot describe changed desired state")
		assert.Equal(t, current.Generation+1, replaced.Generation)
		assert.NotEqual(t, current.ResourceVersion, replaced.ResourceVersion)
		persisted, err := api.GetCluster(ctx, &clusterv1.GetClusterRequest{Name: created.Name})
		require.NoError(t, err)
		assert.True(t, proto.Equal(replaced, persisted),
			"replacement differs from stored response. Want: %v. Got: %v", replaced, persisted)
		raw, err := resource.Get(ctx, created.Name, metav1.GetOptions{})
		require.NoError(t, err)
		_, found, err := unstructured.NestedString(raw.Object, "spec", "release", "channel", "version")
		require.NoError(t, err)
		assert.False(t, found, "replacement must clear the omitted writable version")
		assert.Equal(t, "PRIVATE", raw.GetAnnotations()["example.com/private"])
		runtimeName, _, err := unstructured.NestedString(raw.Object, "spec", "acceleratedRuntime", "name")
		require.NoError(t, err)
		assert.Equal(t, "none", runtimeName)
		network, _, err := unstructured.NestedMap(raw.Object, "spec", "clusterNetwork")
		require.NoError(t, err)
		originalNetwork, _, err := unstructured.NestedMap(stored.Object, "spec", "clusterNetwork")
		require.NoError(t, err)
		assert.Equal(t, originalNetwork, network, "create-only networking must survive replacement")
		encoded, err := protojson.Marshal(replaced)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "PRIVATE")
		assert.NotContains(t, string(encoded), "accelerated")
		assert.NotContains(t, string(encoded), "managedFields")
		_, err = api.ReplaceCluster(ctx, replacement(created.Name, current.ResourceVersion))
		assert.Equal(t, codes.Aborted, status.Code(err))
	})

	t.Run("complete pagination", func(t *testing.T) {
		ctx := t.Context()
		for i := range 4 {
			_, err := api.CreateCluster(ctx, storageCreate(fmt.Sprintf("page-%d", i)))
			require.NoError(t, err)
		}
		first, err := api.ListClusters(ctx, &clusterv1.ListClustersRequest{PageSize: 2})
		require.NoError(t, err)
		require.Len(t, first.Clusters, 2)
		require.NotEmpty(t, first.NextPageToken)
		names := map[string]bool{}
		page := first
		for {
			for _, cluster := range page.Clusters {
				require.False(t, names[cluster.Name], "duplicate cluster on different pages")
				names[cluster.Name] = true
			}
			if page.NextPageToken == "" {
				break
			}
			page, err = api.ListClusters(ctx, &clusterv1.ListClustersRequest{PageToken: page.NextPageToken})
			require.NoError(t, err)
		}
		all, err := resource.List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		expected := make(map[string]bool, len(all.Items))
		for _, cluster := range all.Items {
			expected[cluster.GetName()] = true
		}
		assert.Equal(t, expected, names)
	})

	t.Run("configured namespaces isolate same-name clusters", func(t *testing.T) {
		ctx := t.Context()
		_, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Create(ctx,
			&unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "other"},
			}}, metav1.CreateOptions{})
		require.NoError(t, err)
		other, err := tenantcluster.New(client, tenantcluster.Config{
			Namespace: "other", DatastoreID: "envtest", PageTokenKey: []byte(strings.Repeat("test", 8)),
		})
		require.NoError(t, err)
		original, err := api.CreateCluster(ctx, storageCreate("namespace-isolation"))
		require.NoError(t, err)
		request := &clusterv1.GetClusterRequest{Name: original.Name}
		_, err = other.GetCluster(ctx, request)
		require.Equal(t, codes.NotFound, status.Code(err))
		created, err := other.CreateCluster(ctx, storageCreate(request.Name))
		require.NoError(t, err)
		assert.NotEqual(t, original.Uid, created.Uid)
		isolated, err := other.GetCluster(ctx, request)
		require.NoError(t, err)
		assert.True(t, proto.Equal(created, isolated),
			"namespace-scoped response differs. Want: %v. Got: %v", created, isolated)
		_, err = other.ReplaceCluster(ctx, replacement(request.Name, original.ResourceVersion))
		require.Equal(t, codes.Aborted, status.Code(err))
		unchanged, err := api.GetCluster(ctx, request)
		require.NoError(t, err)
		assert.True(t, proto.Equal(original, unchanged),
			"another namespace changed the response. Want: %v. Got: %v", original, unchanged)
	})

	t.Run("deployed API role enforces datastore least privilege", func(t *testing.T) {
		ctx := t.Context()
		installAPIIdentity(t, ctx, client)
		identity := rest.CopyConfig(config)
		identity.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:default:public-api"}
		scoped, err := dynamic.NewForConfig(identity)
		require.NoError(t, err)
		access := scoped.Resource(resourceGVR()).Namespace("default")
		require.Eventually(t, func() bool {
			_, err := access.List(ctx, metav1.ListOptions{Limit: 1})
			return err == nil
		}, 5*time.Second, 20*time.Millisecond, "API RoleBinding should become effective")
		public := newService(t, identity)
		created, err := public.CreateCluster(ctx, storageCreate("least-privilege"))
		require.NoError(t, err)
		_, err = public.GetCluster(ctx, &clusterv1.GetClusterRequest{Name: created.Name})
		require.NoError(t, err)
		_, err = public.ListClusters(ctx, &clusterv1.ListClustersRequest{})
		require.NoError(t, err)
		_, err = public.ReplaceCluster(ctx, replacement(created.Name, created.ResourceVersion))
		require.NoError(t, err)
		stored, err := access.Get(ctx, created.Name, metav1.GetOptions{})
		require.NoError(t, err)
		for _, forbidden := range []struct {
			name string
			call func(context.Context) error
		}{
			{name: "update", call: func(ctx context.Context) error {
				_, err := access.Update(ctx, stored, metav1.UpdateOptions{})
				return err
			}},
			{name: "delete", call: func(ctx context.Context) error {
				return access.Delete(ctx, created.Name, metav1.DeleteOptions{})
			}},
			{name: "status update", call: func(ctx context.Context) error {
				_, err := access.UpdateStatus(ctx, stored, metav1.UpdateOptions{})
				return err
			}},
			{name: "status patch", call: func(ctx context.Context) error {
				_, err := access.Patch(ctx, created.Name, types.MergePatchType,
					[]byte(`{"status":{}}`), metav1.PatchOptions{}, "status")
				return err
			}},
			{name: "other namespace", call: func(ctx context.Context) error {
				_, err := scoped.Resource(resourceGVR()).Namespace("other").Get(ctx, created.Name, metav1.GetOptions{})
				return err
			}},
			{name: "cluster-wide list", call: func(ctx context.Context) error {
				_, err := scoped.Resource(resourceGVR()).List(ctx, metav1.ListOptions{})
				return err
			}},
		} {
			t.Run(forbidden.name, func(t *testing.T) {
				err := forbidden.call(t.Context())
				assert.True(t, apierrors.IsForbidden(err), "expected datastore access to be forbidden, got %v", err)
			})
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := api.GetCluster(canceled, &clusterv1.GetClusterRequest{Name: "roundtrip"})
		assert.Equal(t, codes.Canceled, status.Code(err))
	})
}

// startDatastore isolates each test in a fresh Kubernetes API server with automatic cleanup.
func startDatastore(t *testing.T) *rest.Config {
	t.Helper()
	environment := &envtest.Environment{
		UseExistingCluster:    new(false),
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	return config
}

// storageCreate builds valid public input with a unique name for independent persistence scenarios.
func storageCreate(name string) *clusterv1.CreateClusterRequest {
	return &clusterv1.CreateClusterRequest{
		Name: name,
		Spec: &clusterv1.CreateClusterSpec{
			ControlPlaneClass: new("standard"),
			Release:           &clusterv1.CreateRelease{Channel: &clusterv1.CreateReleaseChannel{Name: new("stable")}},
			ClusterNetwork: &clusterv1.CreateClusterNetwork{
				Pods:     &clusterv1.CreateClusterNetworkPods{CidrBlocks: []string{"10.244.0.0/16"}},
				Services: &clusterv1.CreateClusterNetworkServices{CidrBlocks: []string{"10.96.0.0/12"}},
			},
		},
	}
}

// resourceGVR locates the internal resource so tests can compare public responses with persisted state.
func resourceGVR() schema.GroupVersionResource {
	return internalv1.SchemeGroupVersion.WithResource("nvkeclusters")
}

// installAPIIdentity loads the shipped permissions so tests detect accidental privilege expansion.
func installAPIIdentity(t *testing.T, ctx context.Context, client dynamic.Interface) {
	t.Helper()
	manifest, err := os.Open(filepath.Join("..", "..", "..", "config", "api", "state", "identity.yaml"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manifest.Close()) })
	decoder := yaml.NewYAMLOrJSONDecoder(manifest, 4096)
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		object.SetNamespace("default")
		if object.GetKind() == "RoleBinding" {
			subjects, _, err := unstructured.NestedSlice(object.Object, "subjects")
			require.NoError(t, err)
			for _, subject := range subjects {
				subject.(map[string]any)["namespace"] = "default"
			}
			require.NoError(t, unstructured.SetNestedSlice(object.Object, subjects, "subjects"))
		}
		gvr, _ := meta.UnsafeGuessKindToResource(object.GroupVersionKind())
		_, err = client.Resource(gvr).Namespace("default").Create(ctx, &object, metav1.CreateOptions{})
		require.NoError(t, err)
	}
}
