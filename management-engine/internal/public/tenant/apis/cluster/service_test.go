// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Service tests verify public validation, projection, pagination and safe failure handling.
package cluster

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	kubetesting "k8s.io/client-go/testing"

	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/shared/datastore"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/shared/limits"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/apis/cluster/mapping"
	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

// testConfig supplies a deterministic storage scope so token isolation is reproducible.
func testConfig() Config {
	return Config{Namespace: "default", DatastoreID: "test-storage", PageTokenKey: make([]byte, limits.PageTokenKeyMinBytes)}
}

// newFakeService exposes datastore calls for assertions about validation and write boundaries.
func newFakeService(t *testing.T) (*Service, *fake.FakeDynamicClient) {
	t.Helper()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{clusterResource: "NVKEClusterList"})
	svc, err := New(client, testConfig())
	require.NoError(t, err)
	return svc, client
}

// createRequest builds valid public input so each case changes only the behavior under test.
func createRequest(t *testing.T) *clusterv1.CreateClusterRequest {
	t.Helper()
	request := new(clusterv1.CreateClusterRequest)
	require.NoError(t, protojson.Unmarshal([]byte(`{
		"name":"cluster","spec":{
			"control_plane_class":"standard","release":{"channel":{"name":"stable"}},
			"cluster_network":{"pods":{"cidr_blocks":["10.244.0.0/16"]},"services":{"cidr_blocks":["10.96.0.0/12"]}}
		}}`), request))
	return request
}

// TestInvalidRequestsNeverAccessStorage proves invalid public input is rejected before any datastore call.
func TestInvalidRequestsNeverAccessStorage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*clusterv1.CreateClusterRequest)
		field  string
	}{
		{name: "invalid name", mutate: func(r *clusterv1.CreateClusterRequest) { r.Name = "../other" }, field: "name"},
		{name: "missing spec", mutate: func(r *clusterv1.CreateClusterRequest) { r.Spec = nil }, field: "spec"},
		{name: "unknown binary input", mutate: func(r *clusterv1.CreateClusterRequest) {
			r.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 999, protowire.VarintType), 1))
		}},
		{name: "nested unknown binary input", mutate: func(r *clusterv1.CreateClusterRequest) {
			r.Spec.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 999, protowire.VarintType), 1))
		}},
		{name: "oversized request", mutate: func(r *clusterv1.CreateClusterRequest) { r.Name = strings.Repeat("a", limits.RequestBytes) }},
		{name: "invalid class before network policy", field: "spec.control_plane_class", mutate: func(r *clusterv1.CreateClusterRequest) {
			r.Spec.ControlPlaneClass = new("PRIVATE/invalid")
			r.Spec.ClusterNetwork.Pods.CidrBlocks = []string{"PRIVATE"}
		}},
		{name: "invalid CIDR", field: "spec.cluster_network.pods.cidr_blocks", mutate: func(r *clusterv1.CreateClusterRequest) {
			r.Spec.ClusterNetwork.Pods.CidrBlocks = []string{"PRIVATE"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, client := newFakeService(t)
			request := createRequest(t)
			tc.mutate(request)
			_, err := svc.CreateCluster(t.Context(), request)
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Empty(t, client.Actions())
			assert.NotContains(t, err.Error(), "PRIVATE")
			if tc.field == "" {
				assert.Equal(t, "invalid cluster request", status.Convert(err).Message())
				assert.Empty(t, status.Convert(err).Details())
			} else {
				assertFieldViolation(t, err, tc.field)
			}
		})
	}
	t.Run("nil create request", func(t *testing.T) {
		svc, client := newFakeService(t)
		_, err := svc.CreateCluster(t.Context(), nil)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Empty(t, client.Actions())
	})
	validSpec := &clusterv1.ReplaceClusterSpec{
		ControlPlaneClass: new("standard"),
		Release:           &clusterv1.ReplaceRelease{Channel: &clusterv1.ReplaceReleaseChannel{Name: new("stable")}},
	}
	for name, version := range map[string]string{
		"missing": "", "zero": "0", "multiple zeros": "00", "leading whitespace": " 0", "trailing whitespace": "x ",
	} {
		t.Run("invalid replacement version/"+name, func(t *testing.T) {
			svc, client := newFakeService(t)
			_, err := svc.ReplaceCluster(t.Context(), &clusterv1.ReplaceClusterRequest{Name: "cluster", ResourceVersion: version, Spec: validSpec})
			assertFieldViolation(t, err, "resource_version")
			assert.Empty(t, client.Actions())
		})
	}
	for name, spec := range map[string]*clusterv1.ReplaceClusterSpec{
		"missing spec":  nil,
		"missing class": {Release: &clusterv1.ReplaceRelease{Channel: &clusterv1.ReplaceReleaseChannel{Name: new("stable")}}},
	} {
		t.Run("invalid replacement spec/"+name, func(t *testing.T) {
			svc, client := newFakeService(t)
			_, err := svc.ReplaceCluster(t.Context(), &clusterv1.ReplaceClusterRequest{
				Name: "cluster", ResourceVersion: "42", Spec: spec,
			})
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Empty(t, client.Actions())
		})
	}
}

// assertFieldViolation checks that clients receive a structured, actionable input error.
func assertFieldViolation(t *testing.T, err error, field string) {
	t.Helper()
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	details := status.Convert(err).Details()
	require.Len(t, details, 1)
	badRequest, ok := details[0].(*errdetails.BadRequest)
	require.True(t, ok)
	require.Len(t, badRequest.FieldViolations, 1)
	assert.Equal(t, field, badRequest.FieldViolations[0].Field)
	assert.NotEmpty(t, badRequest.FieldViolations[0].Description)
}

// TestSupportedNetworks covers address-family, canonical-form and overlap restrictions on creation.
func TestSupportedNetworks(t *testing.T) {
	for _, tc := range []struct {
		name           string
		pods, services []string
		valid          bool
	}{
		{name: "supported", pods: []string{"10.244.0.0/16"}, services: []string{"10.96.0.0/12"}, valid: true},
		{name: "IPv6", pods: []string{"fd00::/64"}, services: []string{"10.96.0.0/12"}},
		{name: "noncanonical", pods: []string{"10.244.1.1/16"}, services: []string{"10.96.0.0/12"}},
		{name: "overlap", pods: []string{"10.96.0.0/16"}, services: []string{"10.96.0.0/12"}},
		{name: "multiple", pods: []string{"10.244.0.0/16", "10.245.0.0/16"}, services: []string{"10.96.0.0/12"}},
		{name: "missing", services: []string{"10.96.0.0/12"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &clusterv1.CreateClusterSpec{ClusterNetwork: &clusterv1.CreateClusterNetwork{
				Pods:     &clusterv1.CreateClusterNetworkPods{CidrBlocks: tc.pods},
				Services: &clusterv1.CreateClusterNetworkServices{CidrBlocks: tc.services},
			}}
			if tc.valid {
				assert.NoError(t, validateNetworks(spec))
			} else {
				assert.Equal(t, codes.InvalidArgument, status.Code(validateNetworks(spec)))
			}
		})
	}
}

// TestPageTokens verifies tenant page-size policy and binds continuations to configured storage.
func TestPageTokens(t *testing.T) {
	svc, client := newFakeService(t)
	const continuation = "private-storage-namespace-and-key"
	token, err := svc.pages.Encode(continuation, 2)
	require.NoError(t, err)
	_, err = svc.ListClusters(t.Context(), &clusterv1.ListClustersRequest{})
	require.NoError(t, err)
	require.Len(t, client.Actions(), 1)
	assert.Equal(t, int64(defaultPageSize), client.Actions()[0].(kubetesting.ListActionImpl).GetListOptions().Limit)
	client.ClearActions()
	_, err = svc.ListClusters(t.Context(), &clusterv1.ListClustersRequest{PageSize: MaxPageSize + 1})
	assertFieldViolation(t, err, "page_size")
	assert.Empty(t, client.Actions())
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "namespace", mutate: func(c *Config) { c.Namespace = "other" }},
		{name: "datastore", mutate: func(c *Config) { c.DatastoreID = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := testConfig()
			tc.mutate(&config)
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			other, err := New(client, config)
			require.NoError(t, err)
			_, err = other.ListClusters(t.Context(), &clusterv1.ListClustersRequest{PageToken: token})
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Empty(t, client.Actions())
		})
	}
	resumed, resumedClient := newFakeService(t)
	_, err = resumed.ListClusters(t.Context(), &clusterv1.ListClustersRequest{PageToken: token})
	require.NoError(t, err)
	require.Len(t, resumedClient.Actions(), 1)
	options := resumedClient.Actions()[0].(kubetesting.ListActionImpl).GetListOptions()
	assert.Equal(t, continuation, options.Continue, "tokens survive service restarts with the same configured key")
	assert.Equal(t, int64(2), options.Limit)
}

// storedCluster includes private data so projection and replacement tests detect accidental exposure or loss.
func storedCluster(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	spec, err := mapping.CreateSpecToInternal(createRequest(t).Spec)
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(spec, "v1", "release", "channel", "version"))
	require.NoError(t, unstructured.SetNestedField(spec, map[string]any{
		"nil": nil, "list": []any{"PRIVATE", map[string]any{"value": "PRIVATE"}},
	}, "release", "channel", "private"))
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": internalv1.SchemeGroupVersion.String(), "kind": "NVKECluster",
		"metadata": map[string]any{
			"name": "cluster", "namespace": "default", "resourceVersion": "42", "generation": int64(2),
			"annotations": map[string]any{"example.com/private": "PRIVATE"},
		},
		"spec": spec,
		"status": map[string]any{"conditions": []any{map[string]any{
			"type": "Ready", "status": "True", "observedGeneration": int64(2), "message": "PRIVATE",
		}}},
	}}
}

// TestProjectionDoesNotMutateOrAliasStorage verifies public responses cannot mutate their stored source.
func TestProjectionDoesNotMutateOrAliasStorage(t *testing.T) {
	svc, _ := newFakeService(t)
	stored := storedCluster(t)
	before := stored.DeepCopy()
	projected, err := svc.result(t.Context(), "get", stored.GetName(), stored, nil)
	require.NoError(t, err)
	assert.Equal(t, before, stored)
	assert.Equal(t, clusterv1.ClusterStatus_STATE_READY, projected.Status.State)
	*projected.Spec.Release.Channel.Name = "changed"
	projected.Spec.ClusterNetwork.Pods.CidrBlocks[0] = "changed"
	projected.Status.ObservedGeneration = 100
	assert.Equal(t, before, stored, "public output must not retain writable aliases into storage")
}

// TestReplacementChangesOnlyWritableSpec proves a guarded merge preserves private and create-only fields.
func TestReplacementChangesOnlyWritableSpec(t *testing.T) {
	svc, client := newFakeService(t)
	stored := storedCluster(t)
	before := stored.DeepCopy()
	client.PrependReactor("get", "nvkeclusters", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, stored, nil
	})
	var persisted unstructured.Unstructured
	client.PrependReactor("patch", "nvkeclusters", func(action kubetesting.Action) (bool, runtime.Object, error) {
		patch := action.(kubetesting.PatchAction).GetPatch()
		require.JSONEq(t, `{"metadata":{"resourceVersion":"42"},"spec":{
			"controlPlaneClass":"alternate","release":{"channel":{"version":null}}}}`, string(patch))
		data, err := json.Marshal(stored.Object)
		require.NoError(t, err)
		data, err = jsonpatch.MergePatch(data, patch)
		require.NoError(t, err)
		require.NoError(t, persisted.UnmarshalJSON(data))
		return true, &persisted, nil
	})
	response, err := svc.ReplaceCluster(t.Context(), &clusterv1.ReplaceClusterRequest{
		Name: "cluster", ResourceVersion: "42", Spec: &clusterv1.ReplaceClusterSpec{
			ControlPlaneClass: new("alternate"), Release: &clusterv1.ReplaceRelease{
				Channel: &clusterv1.ReplaceReleaseChannel{Name: new("stable")},
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, before, stored, "replacement must not mutate the object returned by the datastore")
	assert.Equal(t, "alternate", response.Spec.GetControlPlaneClass())
	assert.Nil(t, response.Spec.Release.Channel.Version)
	want := before.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(want.Object, "alternate", "spec", "controlPlaneClass"))
	unstructured.RemoveNestedField(want.Object, "spec", "release", "channel", "version")
	assert.Equal(t, want, &persisted, "private maps, nulls, lists, metadata and status must survive")
}

// TestMalformedStoredReplacementDoesNotWrite ensures invalid stored shapes fail without partial writes.
func TestMalformedStoredReplacementDoesNotWrite(t *testing.T) {
	for name, spec := range map[string]any{
		"null spec": nil, "scalar spec": "PRIVATE", "scalar release": map[string]any{"release": "PRIVATE"},
	} {
		t.Run(name, func(t *testing.T) {
			svc, client := newFakeService(t)
			stored := storedCluster(t)
			stored.Object["spec"] = spec
			before := stored.DeepCopy()
			client.PrependReactor("get", "nvkeclusters", func(kubetesting.Action) (bool, runtime.Object, error) {
				return true, stored, nil
			})
			_, err := svc.ReplaceCluster(t.Context(), &clusterv1.ReplaceClusterRequest{
				Name: "cluster", ResourceVersion: "42", Spec: &clusterv1.ReplaceClusterSpec{
					ControlPlaneClass: new("alternate"), Release: &clusterv1.ReplaceRelease{
						Channel: &clusterv1.ReplaceReleaseChannel{Name: new("stable")},
					},
				},
			})
			require.Equal(t, codes.Internal, status.Code(err))
			assert.NotContains(t, err.Error(), "PRIVATE")
			require.Len(t, client.Actions(), 1)
			assert.Equal(t, "get", client.Actions()[0].GetVerb())
			assert.Equal(t, before, stored)
		})
	}
}

// TestServiceConfigurationBounds verifies invalid pagination configuration prevents service startup.
func TestServiceConfigurationBounds(t *testing.T) {
	_, client := newFakeService(t)
	config := testConfig()
	config.PageTokenKey = nil
	_, err := New(client, config)
	require.Error(t, err)
}

// TestStorageFailureDiagnostics checks error classification without exposing private storage details.
func TestStorageFailureDiagnostics(t *testing.T) {
	t.Run("public resource label", func(t *testing.T) {
		private := apierrors.NewNotFound(schema.GroupResource{Resource: "PRIVATE"}, "PRIVATE")
		public := datastore.PublicError(private, "release channel")
		assert.Equal(t, status.New(codes.NotFound, "release channel not found").Proto(), status.Convert(public).Proto())
	})
	for _, tc := range []struct {
		name, operation string
		err             error
		code            codes.Code
		class           string
	}{
		{name: "unexpected datastore failure", operation: "get", err: errors.New("PRIVATE token or upstream endpoint"), code: codes.Unavailable, class: "unexpected_failure"},
		{name: "unauthorized datastore", operation: "get", err: apierrors.NewUnauthorized("PRIVATE"), code: codes.Internal, class: "storage_unauthorized"},
		{name: "forbidden datastore", operation: "get", err: apierrors.NewForbidden(clusterResource.GroupResource(), "PRIVATE", errors.New("PRIVATE")), code: codes.Internal, class: "storage_forbidden"},
		{name: "datastore internal error", operation: "get", err: apierrors.NewInternalError(errors.New("PRIVATE")), code: codes.Internal, class: "storage_internal_error"},
		{name: "TLS failure", operation: "get", err: fmt.Errorf("PRIVATE: %w", x509.HostnameError{Host: "PRIVATE"}), code: codes.Unavailable, class: "tls_hostname_mismatch"},
		{name: "network failure", operation: "get", err: fmt.Errorf("PRIVATE: %w", syscall.ECONNREFUSED), code: codes.Unavailable, class: "connection_refused"},
		{name: "deadline", operation: "get", err: context.DeadlineExceeded, code: codes.DeadlineExceeded, class: "storage_timeout"},
		{name: "readiness identity failure", operation: "ready", err: apierrors.NewUnauthorized("PRIVATE"), code: codes.Internal, class: "storage_unauthorized"},
		{name: "expired continuation", operation: "list", err: apierrors.NewResourceExpired("PRIVATE"), code: codes.InvalidArgument},
		{name: "gone continuation", operation: "list", err: &apierrors.StatusError{ErrStatus: metav1.Status{Reason: metav1.StatusReasonGone, Code: 410, Message: "PRIVATE"}}, code: codes.InvalidArgument},
		{name: "readiness missing CRD", operation: "ready", err: apierrors.NewNotFound(clusterResource.GroupResource(), "PRIVATE"), code: codes.Internal, class: "storage_not_found"},
		{name: "list missing CRD", operation: "list", err: apierrors.NewNotFound(clusterResource.GroupResource(), "PRIVATE"), code: codes.Internal, class: "storage_not_found"},
		{name: "create missing namespace", operation: "create", err: apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "PRIVATE"), code: codes.Internal, class: "storage_not_found"},
		{name: "not found", operation: "get", err: apierrors.NewNotFound(clusterResource.GroupResource(), "PRIVATE"), code: codes.NotFound},
		{name: "canceled", operation: "get", err: context.Canceled, code: codes.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, client := newFakeService(t)
			var logs bytes.Buffer
			svc.config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			client.PrependReactor("*", "nvkeclusters", func(kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			var err error
			switch tc.operation {
			case "create":
				_, err = svc.CreateCluster(t.Context(), createRequest(t))
			case "get":
				_, err = svc.GetCluster(t.Context(), &clusterv1.GetClusterRequest{Name: "cluster"})
			case "list":
				_, err = svc.ListClusters(t.Context(), &clusterv1.ListClustersRequest{})
			case "ready":
				err = svc.Ready(t.Context())
			}
			require.Equal(t, tc.code, status.Code(err))
			assert.NotContains(t, err.Error(), "PRIVATE")
			assert.NotContains(t, logs.String(), "PRIVATE")
			if tc.class == "" {
				assert.Empty(t, logs.String(), "expected client outcomes must not be logged as internal failures")
				if tc.operation == "list" {
					assert.Contains(t, err.Error(), "restart listing")
				}
				return
			}
			assert.Equal(t, 1, strings.Count(logs.String(), "\n"), "one diagnostic per failure")
			var record map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
			assert.Equal(t, tc.operation, record["operation"])
			assert.Equal(t, "nvkeclusters", record["resource"])
			assert.Equal(t, "default", record["namespace"])
			assert.Equal(t, tc.class, record["error_class"])
			assert.NotEmpty(t, record["error_type"])
		})
	}
}

// TestStatusProjection verifies lifecycle states use current observations and reviewed public reasons.
func TestStatusProjection(t *testing.T) {
	for _, tc := range []struct {
		name                string
		state               clusterv1.ClusterStatus_State
		reason              clusterv1.ClusterStatus_Reason
		deleted, checkpoint bool
		readyStatus         string
		readyReason         string
		lifecycleReason     string
		lifecycleKind       string
		generation          int64
	}{
		{name: "stale ready", readyStatus: "True", readyReason: "PRIVATE", state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_AWAITING_STATUS, generation: 1},
		{name: "current ready", readyStatus: "True", readyReason: "PRIVATE", state: clusterv1.ClusterStatus_STATE_READY, reason: clusterv1.ClusterStatus_REASON_UNSPECIFIED, generation: 2},
		{name: "deleting ready", readyStatus: "True", readyReason: "PRIVATE", deleted: true, state: clusterv1.ClusterStatus_STATE_DELETING, reason: clusterv1.ClusterStatus_REASON_UNSPECIFIED, generation: 2},
		{name: "initial observation", readyReason: "ReadyUnknown", state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_AWAITING_STATUS, generation: 2},
		{name: "explicitly unreported", readyReason: "NotYetReported", lifecycleReason: "NotYetReported", state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_AWAITING_STATUS, generation: 2},
		{name: "not configured", readyReason: "ReadyUnknown", lifecycleReason: "ProvisioningNotConfigured", state: clusterv1.ClusterStatus_STATE_UNKNOWN, reason: clusterv1.ClusterStatus_REASON_STATUS_UNAVAILABLE, generation: 2},
		{name: "reconciliation failure", readyReason: "ReadyUnknown", lifecycleReason: "ReconcileFailed", state: clusterv1.ClusterStatus_STATE_UNKNOWN, reason: clusterv1.ClusterStatus_REASON_STATUS_UNAVAILABLE, generation: 2},
		{name: "control plane reconciliation failure", readyReason: "ReadyUnknown", lifecycleReason: "ReconcileFailed", lifecycleKind: internalv1.ControlPlaneReadyCondition, state: clusterv1.ClusterStatus_STATE_UNKNOWN, reason: clusterv1.ClusterStatus_REASON_STATUS_UNAVAILABLE, generation: 2},
		{name: "future reason", readyReason: "PRIVATE", state: clusterv1.ClusterStatus_STATE_UNKNOWN, reason: clusterv1.ClusterStatus_REASON_STATUS_UNAVAILABLE, generation: 2},
		{name: "checkpoint exists", readyReason: "ReadyUnknown", checkpoint: true, state: clusterv1.ClusterStatus_STATE_UNKNOWN, reason: clusterv1.ClusterStatus_REASON_STATUS_UNAVAILABLE, generation: 2},
		{name: "ready blocks deletion", readyReason: "CreateOutcomeUnknown", deleted: true, state: clusterv1.ClusterStatus_STATE_DELETING, reason: clusterv1.ClusterStatus_REASON_DELETION_BLOCKED, generation: 2},
		{name: "synced blocks deletion", readyReason: "ReadyUnknown", lifecycleReason: "CreateOutcomeUnknown", deleted: true, state: clusterv1.ClusterStatus_STATE_DELETING, reason: clusterv1.ClusterStatus_REASON_DELETION_BLOCKED, generation: 2},
		{name: "stale deletion failure", readyReason: "CreateOutcomeUnknown", lifecycleReason: "CreateOutcomeUnknown", deleted: true, state: clusterv1.ClusterStatus_STATE_DELETING, reason: clusterv1.ClusterStatus_REASON_UNSPECIFIED, generation: 1},
		{name: "create rejected", readyStatus: "False", lifecycleReason: "CreateRejected", state: clusterv1.ClusterStatus_STATE_FAILED, reason: clusterv1.ClusterStatus_REASON_CREATION_REJECTED, generation: 2},
		{name: "unsupported configuration", readyStatus: "False", lifecycleReason: "UnsupportedConfiguration", state: clusterv1.ClusterStatus_STATE_FAILED, reason: clusterv1.ClusterStatus_REASON_UNSUPPORTED_CONFIGURATION, generation: 2},
		{name: "unsupported update", readyStatus: "False", lifecycleReason: "UnsupportedUpdate", state: clusterv1.ClusterStatus_STATE_FAILED, reason: clusterv1.ClusterStatus_REASON_UNSUPPORTED_UPDATE, generation: 2},
		{name: "provisioning failed", readyStatus: "False", lifecycleReason: "ProvisioningFailed", lifecycleKind: internalv1.ControlPlaneReadyCondition, state: clusterv1.ClusterStatus_STATE_FAILED, reason: clusterv1.ClusterStatus_REASON_PROVISIONING_FAILED, generation: 2},
		{name: "provisioning", readyStatus: "False", lifecycleReason: "Provisioning", state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_PROVISIONING, generation: 2},
		{name: "provisioned but not ready", readyStatus: "False", lifecycleReason: "Provisioned", state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_UNSPECIFIED, generation: 2},
		{name: "synced but not ready", readyStatus: "False", lifecycleReason: internalv1.SyncedReason, state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_UNSPECIFIED, generation: 2},
		{name: "not synced", readyStatus: "False", lifecycleReason: internalv1.NotSyncedReason, state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_UNSPECIFIED, generation: 2},
		{name: "sync unknown", readyStatus: "False", lifecycleReason: internalv1.SyncUnknownReason, state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_UNSPECIFIED, generation: 2},
		{name: "create outcome unknown", readyStatus: "False", lifecycleReason: "CreateOutcomeUnknown", state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_UNSPECIFIED, generation: 2},
		{name: "unconfigured but not ready", readyStatus: "False", lifecycleReason: "ProvisioningNotConfigured", state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_UNSPECIFIED, generation: 2},
		{name: "reconciliation failed but not ready", readyStatus: "False", lifecycleReason: "ReconcileFailed", state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_UNSPECIFIED, generation: 2},
		{name: "future failure reason", readyStatus: "False", lifecycleReason: "PRIVATE_FUTURE_REASON", state: clusterv1.ClusterStatus_STATE_PENDING, reason: clusterv1.ClusterStatus_REASON_UNSPECIFIED, generation: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := "Unknown"
			if tc.deleted {
				state = "False"
			}
			if tc.readyStatus != "" {
				state = tc.readyStatus
			}
			conditions := []any{map[string]any{
				"type": "Ready", "status": state, "observedGeneration": tc.generation, "reason": tc.readyReason, "message": "PRIVATE",
			}}
			if tc.lifecycleReason != "" {
				kind := tc.lifecycleKind
				if kind == "" {
					kind = internalv1.SyncedCondition
				}
				conditions = append(conditions, map[string]any{
					"type": kind, "status": state, "observedGeneration": tc.generation, "reason": tc.lifecycleReason, "message": "PRIVATE",
				})
			}
			object := &unstructured.Unstructured{Object: map[string]any{
				"metadata": map[string]any{"generation": int64(2)}, "status": map[string]any{"conditions": conditions},
			}}
			if tc.checkpoint {
				require.NoError(t, unstructured.SetNestedMap(object.Object, map[string]any{}, "status", "provisioning"))
			}
			if tc.deleted {
				deleted := metav1.Now()
				object.SetDeletionTimestamp(&deleted)
			}
			projected := projectStatus(object)
			assert.Equal(t, tc.state, projected.State)
			assert.Equal(t, tc.reason, projected.Reason)
			assert.Equal(t, tc.generation, projected.ObservedGeneration)
			encoded, err := protojson.Marshal(projected)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "PRIVATE")
		})
	}
}

// TestInvalidStoredDataIsLoggedWithoutValues checks malformed responses produce safe operator diagnostics.
func TestInvalidStoredDataIsLoggedWithoutValues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		namespace string
		spec      any
	}{
		{name: "wrong namespace", namespace: "other", spec: map[string]any{}},
		{name: "wrong spec shape", namespace: "default", spec: "PRIVATE"},
		{name: "wrong field shape", namespace: "default", spec: map[string]any{"controlPlaneClass": []any{"PRIVATE"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, client := newFakeService(t)
			var logs bytes.Buffer
			svc.config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			client.PrependReactor("get", "nvkeclusters", func(kubetesting.Action) (bool, runtime.Object, error) {
				return true, &unstructured.Unstructured{Object: map[string]any{
					"metadata": map[string]any{"name": "cluster", "namespace": tc.namespace}, "spec": tc.spec,
				}}, nil
			})
			_, err := svc.GetCluster(t.Context(), &clusterv1.GetClusterRequest{Name: "cluster"})
			require.Equal(t, codes.Internal, status.Code(err))
			assert.NotContains(t, err.Error(), "PRIVATE")
			assert.NotContains(t, logs.String(), "PRIVATE")
			assert.Equal(t, 1, strings.Count(logs.String(), "\n"))
			assert.Contains(t, logs.String(), `"name":"cluster"`)
		})
	}
}
