// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestApplyNodeMetadataPoolNode(t *testing.T) {
	tests := []struct {
		name         string
		controlPlane bool
		poolLabels   map[string]string
		poolTaints   []db.NodeTaint
		existing     corev1.Node
		wantLabels   map[string]string
		wantTaints   []corev1.Taint
		wantPatches  int
		wantOutput   string
	}{
		{
			name:       "worker gets pool labels and taints alongside existing ones",
			poolLabels: map[string]string{"example.com/team": "ml", "example.com/empty": ""},
			poolTaints: []db.NodeTaint{
				{Key: "example.com/gpu", Value: "true", Effect: "NoSchedule"},
				{Key: "example.com/dedicated", Effect: "NoExecute"},
			},
			existing: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"example.com/existing": "kept"}},
				Spec: corev1.NodeSpec{Taints: []corev1.Taint{
					{Key: "example.com/existing", Value: "kept", Effect: corev1.TaintEffectNoSchedule},
				}},
			},
			wantLabels: map[string]string{
				"example.com/existing": "kept",
				"example.com/team":     "ml",
				"example.com/empty":    "",
			},
			wantTaints: []corev1.Taint{
				{Key: "example.com/existing", Value: "kept", Effect: corev1.TaintEffectNoSchedule},
				{Key: "example.com/gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule},
				{Key: "example.com/dedicated", Effect: corev1.TaintEffectNoExecute},
			},
			wantPatches: 2,
			wantOutput: "node/worker-0: labels added [example.com/empty=,example.com/team=ml], " +
				"taints added [example.com/gpu=true:NoSchedule,example.com/dedicated:NoExecute]",
		},
		{
			name:       "pool values replace existing values for the same label key and taint key and effect",
			poolLabels: map[string]string{"nodeGroup": "gpu"},
			poolTaints: []db.NodeTaint{{Key: "example.com/gpu", Value: "new", Effect: "NoSchedule"}},
			existing: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"nodeGroup": "worker"}},
				Spec: corev1.NodeSpec{Taints: []corev1.Taint{
					{Key: "example.com/gpu", Value: "old", Effect: corev1.TaintEffectNoSchedule},
				}},
			},
			wantLabels:  map[string]string{"nodeGroup": "gpu"},
			wantTaints:  []corev1.Taint{{Key: "example.com/gpu", Value: "new", Effect: corev1.TaintEffectNoSchedule}},
			wantPatches: 2,
			wantOutput:  "node/worker-0: labels updated [nodeGroup=worker->gpu], taints updated [example.com/gpu=old->new:NoSchedule]",
		},
		{
			name:       "node that already matches is not patched",
			poolLabels: map[string]string{"nodeGroup": "gpu"},
			poolTaints: []db.NodeTaint{{Key: "example.com/gpu", Value: "true", Effect: "NoSchedule"}},
			existing: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"nodeGroup": "gpu"}},
				Spec: corev1.NodeSpec{Taints: []corev1.Taint{
					{Key: "example.com/gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule},
				}},
			},
			wantLabels:  map[string]string{"nodeGroup": "gpu"},
			wantTaints:  []corev1.Taint{{Key: "example.com/gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule}},
			wantPatches: 0,
			wantOutput:  "node/worker-0: labels unchanged [nodeGroup=gpu], taints unchanged [example.com/gpu=true:NoSchedule]",
		},
		{
			name:       "node with some labels and taints already present gets only the missing ones",
			poolLabels: map[string]string{"nodeGroup": "gpu", "example.com/team": "ml"},
			poolTaints: []db.NodeTaint{
				{Key: "example.com/gpu", Value: "true", Effect: "NoSchedule"},
				{Key: "example.com/dedicated", Effect: "NoExecute"},
			},
			existing: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"nodeGroup": "gpu"}},
				Spec: corev1.NodeSpec{Taints: []corev1.Taint{
					{Key: "example.com/gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule},
				}},
			},
			wantLabels: map[string]string{"nodeGroup": "gpu", "example.com/team": "ml"},
			wantTaints: []corev1.Taint{
				{Key: "example.com/gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule},
				{Key: "example.com/dedicated", Effect: corev1.TaintEffectNoExecute},
			},
			wantPatches: 2,
			wantOutput: "node/worker-0: labels added [example.com/team=ml], labels unchanged [nodeGroup=gpu], " +
				"taints added [example.com/dedicated:NoExecute], taints unchanged [example.com/gpu=true:NoSchedule]",
		},
		{
			name:       "taint with the same key and a different effect is added alongside the existing one",
			poolTaints: []db.NodeTaint{{Key: "example.com/gpu", Value: "new", Effect: "NoExecute"}},
			existing: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"example.com/existing": "kept"}},
				Spec: corev1.NodeSpec{Taints: []corev1.Taint{
					{Key: "example.com/gpu", Value: "old", Effect: corev1.TaintEffectNoSchedule},
				}},
			},
			wantLabels: map[string]string{"example.com/existing": "kept"},
			wantTaints: []corev1.Taint{
				{Key: "example.com/gpu", Value: "old", Effect: corev1.TaintEffectNoSchedule},
				{Key: "example.com/gpu", Value: "new", Effect: corev1.TaintEffectNoExecute},
			},
			wantPatches: 1,
			wantOutput:  "node/worker-0: taints added [example.com/gpu=new:NoExecute]",
		},
		{
			name:         "control-plane pool gets labels but not taints",
			controlPlane: true,
			poolLabels:   map[string]string{"nodeGroup": "control-plane"},
			poolTaints:   []db.NodeTaint{{Key: "example.com/gpu", Value: "true", Effect: "NoSchedule"}},
			existing: corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{
				{Key: "example.com/existing", Effect: corev1.TaintEffectNoSchedule},
			}}},
			wantLabels:  map[string]string{"nodeGroup": "control-plane"},
			wantTaints:  []corev1.Taint{{Key: "example.com/existing", Effect: corev1.TaintEffectNoSchedule}},
			wantPatches: 1,
			wantOutput:  "node/worker-0: labels added [nodeGroup=control-plane]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeID := uuid.New()
			clusterID := uuid.New()
			nodePoolID := uuid.New()
			nodeSetID := uuid.New()
			node := db.Node{
				ID: nodeID, ClusterID: clusterID, NodePoolID: nodePoolID, NodeSetID: nodeSetID,
				Name: "worker-0", ControlPlane: tt.controlPlane,
			}
			nodePool := db.NodePool{ID: nodePoolID, ClusterID: clusterID, NodeLabels: tt.poolLabels, NodeTaints: tt.poolTaints}
			cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}

			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
			database.On("GetNodeSet", mock.Anything, nodeSetID).Return(db.NodeSet{ID: nodeSetID, NodePoolID: nodePoolID}, nil)
			database.On("GetNodePool", mock.Anything, nodePoolID).Return(nodePool, nil)
			database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()

			existing := tt.existing.DeepCopy()
			existing.Name = node.Name
			tenantClient, patches := newNodeMetadataTenantClient(t, existing)
			provider := &fakeTenantClientProvider{tenantClient: tenantClient}
			a := &Activities{db: database, tenantClientProvider: provider}

			result, err := executeApplyNodeMetadataActivity(t, a, ApplyNodeMetadataInput{NodeID: nodeID})
			require.NoError(t, err)
			require.Equal(t, tt.wantOutput, result.Output)

			var updated corev1.Node
			require.NoError(t, tenantClient.Get(context.Background(), client.ObjectKey{Name: node.Name}, &updated))
			require.Equal(t, tt.wantLabels, updated.Labels)
			require.ElementsMatch(t, tt.wantTaints, updated.Spec.Taints)
			require.Len(t, *patches, tt.wantPatches)
			database.AssertExpectations(t)
		})
	}
}

func TestApplyNodeMetadataLocksOnlyTaintPatch(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	nodePoolID := uuid.New()
	nodeSetID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, NodePoolID: nodePoolID, NodeSetID: nodeSetID, Name: "worker-0"}
	nodePool := db.NodePool{
		ID: nodePoolID, ClusterID: clusterID,
		NodeLabels: map[string]string{"example.com/team": "ml"},
		NodeTaints: []db.NodeTaint{{Key: "example.com/gpu", Value: "true", Effect: "NoSchedule"}},
	}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetNodeSet", mock.Anything, nodeSetID).Return(db.NodeSet{ID: nodeSetID, NodePoolID: nodePoolID}, nil)
	database.On("GetNodePool", mock.Anything, nodePoolID).Return(nodePool, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-a"}, nil).Once()

	tenantClient, patches := newNodeMetadataTenantClient(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}})
	a := &Activities{db: database, tenantClientProvider: &fakeTenantClientProvider{tenantClient: tenantClient}}

	_, err := executeApplyNodeMetadataActivity(t, a, ApplyNodeMetadataInput{NodeID: nodeID})
	require.NoError(t, err)

	require.Len(t, *patches, 2)
	labelPatch, taintPatch := (*patches)[0], (*patches)[1]
	require.Contains(t, labelPatch, `"labels"`)
	require.NotContains(t, labelPatch, `"taints"`)
	require.NotContains(t, labelPatch, `"resourceVersion"`)
	require.Contains(t, taintPatch, `"taints"`)
	require.NotContains(t, taintPatch, `"labels"`)
	require.Contains(t, taintPatch, `"resourceVersion"`)
	database.AssertExpectations(t)
}

func TestApplyNodeMetadataRetriesTaintPatchOnConflict(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	nodePoolID := uuid.New()
	nodeSetID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, NodePoolID: nodePoolID, NodeSetID: nodeSetID, Name: "worker-0"}
	nodePool := db.NodePool{
		ID: nodePoolID, ClusterID: clusterID,
		NodeTaints: []db.NodeTaint{{Key: "example.com/gpu", Value: "true", Effect: "NoSchedule"}},
	}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetNodeSet", mock.Anything, nodeSetID).Return(db.NodeSet{ID: nodeSetID, NodePoolID: nodePoolID}, nil)
	database.On("GetNodePool", mock.Anything, nodePoolID).Return(nodePool, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-a"}, nil).Once()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}}).Build()
	var patchErrs []error
	concurrentTaint := corev1.Taint{Key: "example.com/other", Effect: corev1.TaintEffectNoSchedule}
	// Before the first patch reaches the fake API server, another writer adds concurrentTaint and bumps the
	// node's resourceVersion. The activity's patch still carries the old resourceVersion, so it fails with a
	// conflict, and the retry must re-read the node and keep concurrentTaint.
	tenantClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		Patch: func(
			ctx context.Context,
			underlying client.WithWatch,
			obj client.Object,
			patch client.Patch,
			opts ...client.PatchOption,
		) error {
			if len(patchErrs) == 0 {
				var current corev1.Node
				require.NoError(t, underlying.Get(ctx, client.ObjectKey{Name: node.Name}, &current))
				current.Spec.Taints = append(current.Spec.Taints, concurrentTaint)
				require.NoError(t, underlying.Update(ctx, &current))
			}
			err := underlying.Patch(ctx, obj, patch, opts...)
			patchErrs = append(patchErrs, err)
			return err
		},
	})
	a := &Activities{db: database, tenantClientProvider: &fakeTenantClientProvider{tenantClient: tenantClient}}

	result, err := executeApplyNodeMetadataActivity(t, a, ApplyNodeMetadataInput{NodeID: nodeID})
	require.NoError(t, err)
	require.Equal(t, "node/worker-0: taints added [example.com/gpu=true:NoSchedule]", result.Output)
	require.Len(t, patchErrs, 2)
	require.True(t, apierrors.IsConflict(patchErrs[0]), "first patch should conflict, got %v", patchErrs[0])
	require.NoError(t, patchErrs[1])

	var updated corev1.Node
	require.NoError(t, baseClient.Get(context.Background(), client.ObjectKey{Name: node.Name}, &updated))
	require.ElementsMatch(t, []corev1.Taint{
		concurrentTaint,
		{Key: "example.com/gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule},
	}, updated.Spec.Taints)
	database.AssertExpectations(t)
}

func TestApplyNodeMetadataControlPlaneConfigNode(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "cp-0", ControlPlane: true}
	cluster := db.Cluster{ID: clusterID, Name: "cluster-a"}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "test-org", SiteID: uuid.New()})
	cluster.ControlPlaneConfig.NodeLabels = map[string]string{"nodeGroup": "control-plane"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Twice()

	tenantClient, _ := newNodeMetadataTenantClient(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   node.Name,
		Labels: map[string]string{"example.com/existing": "kept"},
	}})
	provider := &fakeTenantClientProvider{tenantClient: tenantClient}
	a := &Activities{db: database, tenantClientProvider: provider}

	result, err := executeApplyNodeMetadataActivity(t, a, ApplyNodeMetadataInput{NodeID: nodeID})
	require.NoError(t, err)
	require.Equal(t, "node/cp-0: labels added [nodeGroup=control-plane]", result.Output)

	var updated corev1.Node
	require.NoError(t, tenantClient.Get(context.Background(), client.ObjectKey{Name: node.Name}, &updated))
	require.Equal(t, map[string]string{
		"example.com/existing": "kept",
		"nodeGroup":            "control-plane",
	}, updated.Labels)
	database.AssertExpectations(t)
}

func TestApplyNodeMetadataSkipsTenantAccessWithNothingToApply(t *testing.T) {
	nodeID := uuid.New()
	clusterID := uuid.New()
	nodePoolID := uuid.New()
	nodeSetID := uuid.New()
	node := db.Node{ID: nodeID, ClusterID: clusterID, NodePoolID: nodePoolID, NodeSetID: nodeSetID, Name: "worker-0"}

	database := &testutil.MockDB{}
	database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
	database.On("GetNodeSet", mock.Anything, nodeSetID).Return(db.NodeSet{ID: nodeSetID, NodePoolID: nodePoolID}, nil)
	database.On("GetNodePool", mock.Anything, nodePoolID).Return(db.NodePool{ID: nodePoolID, ClusterID: clusterID}, nil)
	provider := &fakeTenantClientProvider{}
	a := &Activities{db: database, tenantClientProvider: provider}

	result, err := executeApplyNodeMetadataActivity(t, a, ApplyNodeMetadataInput{NodeID: nodeID})
	require.NoError(t, err)
	require.Equal(t, "node/worker-0 has no labels or taints to apply", result.Output)
	require.Equal(t, 0, provider.calls)
	database.AssertExpectations(t)
}

func TestApplyNodeMetadataPatchErrors(t *testing.T) {
	invalidLabels := apierrors.NewInvalid(schema.GroupKind{Kind: "Node"}, "worker-0", field.ErrorList{
		field.Invalid(field.NewPath("metadata", "labels"), "bad key", "name part must consist of alphanumeric characters"),
	})
	invalidTaints := apierrors.NewInvalid(schema.GroupKind{Kind: "Node"}, "worker-0", field.ErrorList{
		field.NotSupported(field.NewPath("spec", "taints").Index(0).Child("effect"), "Sometimes", []string{"NoSchedule", "PreferNoSchedule", "NoExecute"}),
	})
	tests := []struct {
		name         string
		poolLabels   map[string]string
		poolTaints   []db.NodeTaint
		patchErr     error
		wantErr      string
		wantNonRetry bool
	}{
		{
			name:         "labels rejected as invalid fail without retry",
			poolLabels:   map[string]string{"bad key": "value"},
			patchErr:     invalidLabels,
			wantErr:      "failed to apply labels to Kubernetes node worker-0",
			wantNonRetry: true,
		},
		{
			name:         "taints rejected as invalid fail without retry",
			poolTaints:   []db.NodeTaint{{Key: "example.com/gpu", Value: "true", Effect: "Sometimes"}},
			patchErr:     invalidTaints,
			wantErr:      "failed to apply taints to Kubernetes node worker-0",
			wantNonRetry: true,
		},
		{
			name:       "other patch errors are retried",
			poolLabels: map[string]string{"example.com/team": "ml"},
			patchErr:   apierrors.NewServiceUnavailable("apiserver unavailable"),
			wantErr:    "failed to apply labels to Kubernetes node worker-0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeID := uuid.New()
			clusterID := uuid.New()
			nodePoolID := uuid.New()
			nodeSetID := uuid.New()
			node := db.Node{ID: nodeID, ClusterID: clusterID, NodePoolID: nodePoolID, NodeSetID: nodeSetID, Name: "worker-0"}
			nodePool := db.NodePool{ID: nodePoolID, ClusterID: clusterID, NodeLabels: tt.poolLabels, NodeTaints: tt.poolTaints}

			database := &testutil.MockDB{}
			database.On("GetNode", mock.Anything, nodeID).Return(node, nil)
			database.On("GetNodeSet", mock.Anything, nodeSetID).Return(db.NodeSet{ID: nodeSetID, NodePoolID: nodePoolID}, nil)
			database.On("GetNodePool", mock.Anything, nodePoolID).Return(nodePool, nil)
			database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: "cluster-a"}, nil)

			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}}).Build()
			tenantClient := interceptor.NewClient(baseClient, interceptor.Funcs{
				Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					return tt.patchErr
				},
			})
			a := &Activities{db: database, tenantClientProvider: &fakeTenantClientProvider{tenantClient: tenantClient}}

			_, err := executeApplyNodeMetadataActivity(t, a, ApplyNodeMetadataInput{NodeID: nodeID})
			require.ErrorContains(t, err, tt.wantErr)
			// The node's provisioning condition shows this message, so it must carry the Kubernetes error once.
			conditionMessage := messageForTemporalError(err)
			require.Contains(t, conditionMessage, tt.wantErr)
			require.Equal(t, 1, strings.Count(conditionMessage, tt.patchErr.Error()), conditionMessage)
			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			require.Equal(t, tt.wantNonRetry, appErr.NonRetryable())
			if tt.wantNonRetry {
				require.Equal(t, "InvalidNodeMetadata", appErr.Type())
			}
		})
	}
}

func newNodeMetadataTenantClient(t *testing.T, node *corev1.Node) (client.Client, *[]string) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	var patches []string
	tenantClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		Patch: func(
			ctx context.Context,
			underlying client.WithWatch,
			obj client.Object,
			patch client.Patch,
			opts ...client.PatchOption,
		) error {
			data, err := patch.Data(obj)
			require.NoError(t, err)
			patches = append(patches, string(data))
			return underlying.Patch(ctx, obj, patch, opts...)
		},
	})
	return tenantClient, &patches
}

func executeApplyNodeMetadataActivity(t *testing.T, a *Activities, input ApplyNodeMetadataInput) (ApplyNodeMetadataResult, error) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.ApplyNodeMetadata)

	value, err := env.ExecuteActivity(a.ApplyNodeMetadata, input)
	if err != nil {
		return ApplyNodeMetadataResult{}, err
	}

	var result ApplyNodeMetadataResult
	require.NoError(t, value.Get(&result))
	return result, nil
}
