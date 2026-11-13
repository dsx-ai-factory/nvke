// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/failuredomains"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func stuckPendingNode(clusterID uuid.UUID, name string, pendingFor time.Duration) db.Node {
	node := healthyCPNode(clusterID, name, time.Now().Add(-pendingFor))
	node.FailureDomain = "A"
	node.Conditions.Joined = db.NewCondition(false, "Not joined")
	node.Conditions.Provisioned = db.NewCondition(false, "Not provisioned")
	node.Conditions.KubernetesReady = db.NewCondition(false, "Status unknown")
	node.Conditions.Pending = db.NewCondition(true, "Pending")
	node.Conditions.Pending.LastTransitionTime = time.Now().Add(-pendingFor)
	return node
}

// placementStub reports every Machine's failure domain as unavailable or not.
type placementStub struct {
	clients.MachineMaterialization
	unavailable bool
}

func (s placementStub) FailureDomainUnavailable(context.Context, shared.MachineAdvanceDeps, *clusterv1.Machine) (bool, error) {
	return s.unavailable, nil
}

func capiMachinesClient(t *testing.T, names ...string) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, name := range names {
		builder = builder.WithObjects(&clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Config{}.ManagementNamespaceOrDefault()},
		})
	}
	return builder.Build()
}

func TestIsStuckPendingInDomain(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	tests := []struct {
		name   string
		mutate func(*db.Node)
		want   bool
	}{
		{"pending past the timeout with no instance", func(*db.Node) {}, true},
		{"within the timeout", func(n *db.Node) { n.Conditions.Pending.LastTransitionTime = time.Now() }, false},
		{"no domain pin", func(n *db.Node) { n.FailureDomain = "" }, false},
		{"instance allocated", func(n *db.Node) { n.Nico = &db.NicoNodeProvider{ForgeInstanceID: "i-1"} }, false},
		{"no longer pending", func(n *db.Node) { n.Conditions.Pending = db.NewCondition(false, "Provisioning") }, false},
		{"rebalance surge", func(n *db.Node) { n.Conditions.ControlPlanePreferredReplacement = db.NewCondition(true, "surge") }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			node := stuckPendingNode(clusterID, "cp-a", 2*time.Hour)
			tt.mutate(&node)
			assert.Equal(t, tt.want, isStuckPendingInDomain(node))
		})
	}
}

func TestReconcileStuckPendingNodes_ReplacesAndMarksDomain(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cluster := db.Cluster{ID: clusterID, Name: "test-cluster", ControlPlaneVIP: "10.0.0.1", ControlPlaneConfig: db.ControlPlaneConfig{DesiredCount: 3}}
	stuck := stuckPendingNode(clusterID, "cp-stuck", 2*time.Hour)
	healthyB := healthyCPNode(clusterID, "cp-b", time.Now().Add(-5*time.Hour))
	healthyB.FailureDomain = "B"
	healthyC := healthyCPNode(clusterID, "cp-c", time.Now().Add(-5*time.Hour))
	healthyC.FailureDomain = "C"

	mockTx := &testutil.MockTx{}
	mockTx.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)
	var marked db.Cluster
	mockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		marked = *c
		return true
	})).Return(cluster, nil)
	mockTx.On("GetNode", mock.Anything, stuck.ID).Return(stuck, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, stuck.ID).Return(stuck, nil)
	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(n *db.Node) bool {
		written = *n
		return true
	})).Return(stuck, nil)

	r := &reconciler{db: mockTx, logger: slog.Default(), k8sClient: capiMachinesClient(t, stuck.Name)}
	cp := &controlPlane{
		nodes:                     []db.Node{stuck, healthyB, healthyC},
		clusterControlPlaneConfig: cluster.ControlPlaneConfig,
		failureDomains:            &failuredomains.Snapshot{Eligible: []string{"A", "B", "C"}},
	}

	acted, err := r.reconcileStuckPendingNodes(context.Background(), cluster, cp, placementStub{unavailable: true})
	require.NoError(t, err)

	assert.True(t, acted)
	assert.Equal(t, stuck.ID, written.ID)
	assert.True(t, nodes.IsDeleteRequested(written))
	assert.Contains(t, marked.Conditions.UnplaceableFailureDomains, "A")
}

func TestReconcileStuckPendingNodes_NoOps(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cluster := db.Cluster{ID: clusterID, Name: "test-cluster", ControlPlaneVIP: "10.0.0.1"}
	unavailable := placementStub{unavailable: true}
	twoDomains := &failuredomains.Snapshot{Eligible: []string{"A", "B"}}

	t.Run("nothing stuck", func(t *testing.T) {
		t.Parallel()
		fresh := stuckPendingNode(clusterID, "cp-fresh", time.Minute)
		r := &reconciler{db: &testutil.MockTx{}, logger: slog.Default()}
		acted, err := r.reconcileStuckPendingNodes(context.Background(), cluster, &controlPlane{nodes: []db.Node{fresh}}, unavailable)
		require.NoError(t, err)
		assert.False(t, acted)
	})

	t.Run("a delete is already in flight", func(t *testing.T) {
		t.Parallel()
		stuck := stuckPendingNode(clusterID, "cp-stuck", 2*time.Hour)
		deleting := healthyCPNode(clusterID, "cp-deleting", time.Now())
		deleting.Conditions.Deleting = db.NewCondition(true, "Deleting")
		r := &reconciler{db: &testutil.MockTx{}, logger: slog.Default()}
		acted, err := r.reconcileStuckPendingNodes(context.Background(), cluster, &controlPlane{nodes: []db.Node{stuck, deleting}}, unavailable)
		require.NoError(t, err)
		assert.False(t, acted)
	})

	t.Run("no other failure domain is placeable", func(t *testing.T) {
		t.Parallel()
		stuck := stuckPendingNode(clusterID, "cp-stuck", 2*time.Hour)
		marked := cluster
		marked.Conditions.UnplaceableFailureDomains = map[string]time.Time{"B": time.Now()}
		cp := &controlPlane{nodes: []db.Node{stuck}, failureDomains: &failuredomains.Snapshot{Eligible: []string{"A", "B"}}}
		r := &reconciler{db: &testutil.MockTx{}, logger: slog.Default(), k8sClient: capiMachinesClient(t, stuck.Name)}
		acted, err := r.reconcileStuckPendingNodes(context.Background(), marked, cp, unavailable)
		require.NoError(t, err)
		assert.False(t, acted, "re-placing would land in the same domain again")
	})

	// Regression: with the fallback on, the provisioning activity moves the node; a delete here would race it.
	t.Run("failure-domain fallback is enabled", func(t *testing.T) {
		t.Parallel()
		stuck := stuckPendingNode(clusterID, "cp-stuck", 2*time.Hour)
		r := &reconciler{
			db:        &testutil.MockTx{},
			logger:    slog.Default(),
			k8sClient: capiMachinesClient(t, stuck.Name),
			cfg:       config.Config{ControlPlaneFailureDomainFallbackEnabled: true},
		}
		cp := &controlPlane{nodes: []db.Node{stuck}, failureDomains: twoDomains}
		acted, err := r.reconcileStuckPendingNodes(context.Background(), cluster, cp, unavailable)
		require.NoError(t, err)
		assert.False(t, acted)
	})

	t.Run("controlPlaneVIP is not configured", func(t *testing.T) {
		t.Parallel()
		stuck := stuckPendingNode(clusterID, "cp-stuck", 2*time.Hour)
		noVIP := db.Cluster{ID: clusterID, Name: "test-cluster"}
		r := &reconciler{db: &testutil.MockTx{}, logger: slog.Default()}
		acted, err := r.reconcileStuckPendingNodes(context.Background(), noVIP, &controlPlane{nodes: []db.Node{stuck}}, unavailable)
		require.NoError(t, err)
		assert.False(t, acted)
	})

	// Regression: a NICo outage or a non-capacity placement error must not re-place the node.
	t.Run("provider does not report the domain out of capacity", func(t *testing.T) {
		t.Parallel()
		stuck := stuckPendingNode(clusterID, "cp-stuck", 2*time.Hour)
		r := &reconciler{db: &testutil.MockTx{}, logger: slog.Default(), k8sClient: capiMachinesClient(t, stuck.Name)}
		cp := &controlPlane{nodes: []db.Node{stuck}, failureDomains: twoDomains}
		acted, err := r.reconcileStuckPendingNodes(context.Background(), cluster, cp, placementStub{})
		require.NoError(t, err)
		assert.False(t, acted)
	})

	t.Run("CAPI Machine not found", func(t *testing.T) {
		t.Parallel()
		stuck := stuckPendingNode(clusterID, "cp-stuck", 2*time.Hour)
		r := &reconciler{db: &testutil.MockTx{}, logger: slog.Default(), k8sClient: capiMachinesClient(t)}
		cp := &controlPlane{nodes: []db.Node{stuck}, failureDomains: twoDomains}
		acted, err := r.reconcileStuckPendingNodes(context.Background(), cluster, cp, unavailable)
		require.NoError(t, err)
		assert.False(t, acted)
	})
}
