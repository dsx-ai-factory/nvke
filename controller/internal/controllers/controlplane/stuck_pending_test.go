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

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
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

func TestPlaceableDomains(t *testing.T) {
	t.Parallel()

	now := time.Now()
	eligible := []string{"A", "B", "C"}

	tests := []struct {
		name        string
		unplaceable map[string]time.Time
		want        []string
	}{
		{"none marked", nil, []string{"A", "B", "C"}},
		{"recent mark drops the domain", map[string]time.Time{"A": now.Add(-time.Hour)}, []string{"B", "C"}},
		{"expired mark is ignored", map[string]time.Time{"A": now.Add(-unplaceableDomainTTL - time.Minute)}, []string{"A", "B", "C"}},
		{"every domain marked falls back to all", map[string]time.Time{"A": now, "B": now, "C": now}, []string{"A", "B", "C"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, placeableDomains(eligible, tt.unplaceable, now))
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

	r := &reconciler{db: mockTx, logger: slog.Default()}
	cp := &controlPlane{nodes: []db.Node{stuck, healthyB, healthyC}, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	acted, err := r.reconcileStuckPendingNodes(context.Background(), cluster, cp)
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

	t.Run("nothing stuck", func(t *testing.T) {
		t.Parallel()
		fresh := stuckPendingNode(clusterID, "cp-fresh", time.Minute)
		r := &reconciler{db: &testutil.MockTx{}, logger: slog.Default()}
		acted, err := r.reconcileStuckPendingNodes(context.Background(), cluster, &controlPlane{nodes: []db.Node{fresh}})
		require.NoError(t, err)
		assert.False(t, acted)
	})

	t.Run("a delete is already in flight", func(t *testing.T) {
		t.Parallel()
		stuck := stuckPendingNode(clusterID, "cp-stuck", 2*time.Hour)
		deleting := healthyCPNode(clusterID, "cp-deleting", time.Now())
		deleting.Conditions.Deleting = db.NewCondition(true, "Deleting")
		r := &reconciler{db: &testutil.MockTx{}, logger: slog.Default()}
		acted, err := r.reconcileStuckPendingNodes(context.Background(), cluster, &controlPlane{nodes: []db.Node{stuck, deleting}})
		require.NoError(t, err)
		assert.False(t, acted)
	})

	t.Run("controlPlaneVIP is not configured", func(t *testing.T) {
		t.Parallel()
		stuck := stuckPendingNode(clusterID, "cp-stuck", 2*time.Hour)
		noVIP := db.Cluster{ID: clusterID, Name: "test-cluster"}
		r := &reconciler{db: &testutil.MockTx{}, logger: slog.Default()}
		acted, err := r.reconcileStuckPendingNodes(context.Background(), noVIP, &controlPlane{nodes: []db.Node{stuck}})
		require.NoError(t, err)
		assert.False(t, acted)
	})
}
