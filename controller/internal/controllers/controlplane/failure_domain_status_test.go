// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func fdNode(name, domain string, provisioned, ready bool) db.Node {
	return db.Node{
		ID:            uuid.New(),
		Name:          name,
		ControlPlane:  true,
		FailureDomain: domain,
		Conditions: db.NodeConditions{
			Provisioned:     db.NewCondition(provisioned, ""),
			KubernetesReady: db.NewCondition(ready, ""),
		},
	}
}

// reportedNode is a provisioned, Ready node whose domain only the provider reports.
func reportedNode(name, domain string) db.Node {
	node := fdNode(name, "", true, true)
	node.ObservedFailureDomain = domain
	return node
}

func TestControlPlaneFailureDomainStatus(t *testing.T) {
	deleting := fdNode("cp-3", "rack-a", true, true)
	deleting.Conditions.Deleting = db.NewCondition(true, "")

	tests := []struct {
		name     string
		eligible []string
		nodes    []db.Node
		status   db.ConditionStatus
		message  string
	}{
		{
			name:    "no published or reported domains",
			nodes:   []db.Node{fdNode("cp-1", "", true, true)},
			status:  db.ConditionStatusUnknown,
			message: "No failure domains are published or reported.",
		},
		{
			name:    "provider places each node in its own domain",
			nodes:   []db.Node{reportedNode("cp-1", "rack-a"), reportedNode("cp-2", "rack-b"), fdNode("cp-3", "", false, false)},
			status:  db.ConditionStatusTrue,
			message: "Each control-plane node is in its own failure domain.",
		},
		{
			name:    "provider places nodes in a shared domain",
			nodes:   []db.Node{reportedNode("cp-1", "rack-a"), reportedNode("cp-2", "rack-a"), fdNode("cp-3", "", true, true)},
			status:  db.ConditionStatusFalse,
			message: "no failure domain for cp-3; nodes share rack-a.",
		},
		{
			name:     "one node per domain",
			eligible: []string{"rack-c", "rack-a", "rack-b"},
			nodes:    []db.Node{fdNode("cp-1", "rack-a", true, true), fdNode("cp-2", "rack-b", true, true), fdNode("cp-3", "rack-c", true, true)},
			status:   db.ConditionStatusTrue,
			message:  "Each control-plane node is in its own failure domain.",
		},
		{
			name:     "nodes share a domain",
			eligible: []string{"rack-a", "rack-b"},
			nodes:    []db.Node{fdNode("cp-1", "rack-a", true, true), fdNode("cp-2", "rack-a", true, true), fdNode("cp-3", "rack-b", true, true)},
			status:   db.ConditionStatusFalse,
			message:  "nodes share rack-a.",
		},
		{
			name:     "domain with no Ready node",
			eligible: []string{"rack-a", "rack-b"},
			nodes:    []db.Node{fdNode("cp-1", "rack-a", true, false), fdNode("cp-2", "rack-b", true, true)},
			status:   db.ConditionStatusFalse,
			message:  "no Ready node in rack-a.",
		},
		{
			name:     "unprovisioned node is not degraded",
			eligible: []string{"rack-a", "rack-b"},
			nodes:    []db.Node{fdNode("cp-1", "rack-a", false, false), fdNode("cp-2", "rack-b", true, true)},
			status:   db.ConditionStatusTrue,
			message:  "Each control-plane node is in its own failure domain.",
		},
		{
			name:     "deleting node is ignored",
			eligible: []string{"rack-a", "rack-b"},
			nodes:    []db.Node{fdNode("cp-1", "rack-a", true, true), fdNode("cp-2", "rack-b", true, true), deleting},
			status:   db.ConditionStatusTrue,
			message:  "Each control-plane node is in its own failure domain.",
		},
		{
			name:     "unplaced node",
			eligible: []string{"rack-a"},
			nodes:    []db.Node{fdNode("cp-1", "rack-a", true, true), fdNode("cp-2", "", true, true)},
			status:   db.ConditionStatusFalse,
			message:  "no failure domain for cp-2.",
		},
		{
			name:     "unprovisioned node without a domain is not unplaced",
			eligible: []string{"rack-a"},
			nodes:    []db.Node{fdNode("cp-1", "rack-a", true, true), fdNode("cp-2", "", false, false)},
			status:   db.ConditionStatusTrue,
			message:  "Each control-plane node is in its own failure domain.",
		},
		{
			name:     "observed domain wins over requested",
			eligible: []string{"rack-a", "rack-b"},
			nodes: func() []db.Node {
				moved := fdNode("cp-2", "rack-b", true, true)
				moved.ObservedFailureDomain = "rack-a"
				return []db.Node{fdNode("cp-1", "rack-a", true, true), moved}
			}(),
			status:  db.ConditionStatusFalse,
			message: "nodes share rack-a.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := controlPlaneFailureDomainStatus(tc.eligible, tc.nodes)
			assert.Equal(t, tc.status, got.Spread.Status)
			assert.Equal(t, tc.message, got.Spread.Message)
			assert.IsNonDecreasing(t, got.Eligible)
		})
	}
}

func TestReconcileFailureDomainStatusRecordsObservedDomainAndStatus(t *testing.T) {
	node := fdNode("cp-1", "rack-a", true, true)
	cluster := db.Cluster{ID: uuid.New()}
	mockTx := &testutil.MockTx{}
	mockTx.On("UpdateNodeObservedFailureDomain", mock.Anything, node.ID, "rack-b").Return(nil)
	mockTx.On("UpdateClusterControlPlaneFailureDomains", mock.Anything, cluster.ID, mock.MatchedBy(func(d *db.ControlPlaneFailureDomains) bool {
		return assert.ObjectsAreEqual([]string{"rack-a", "rack-b"}, d.Eligible) && d.Spread.Status == db.ConditionStatusTrue &&
			!d.Spread.LastTransitionTime.IsZero()
	})).Return(nil)

	r := &reconciler{db: mockTx}
	cp := &controlPlane{nodes: []db.Node{node}}
	fds := &failureDomains{eligible: []string{"rack-b", "rack-a"}, reportedByNodeName: map[string]string{"cp-1": "rack-b"}}

	require.NoError(t, r.reconcileFailureDomainStatus(context.Background(), cluster, cp, fds))
	mockTx.AssertExpectations(t)
	assert.Equal(t, "rack-b", cp.nodes[0].ObservedFailureDomain)
}

func TestReconcileFailureDomainStatusSkipsUnchangedStatus(t *testing.T) {
	node := fdNode("cp-1", "rack-a", true, true)
	node.ObservedFailureDomain = "rack-a"
	cluster := db.Cluster{ID: uuid.New(), ControlPlaneFailureDomains: &db.ControlPlaneFailureDomains{
		Eligible: []string{"rack-a"},
		Spread: db.Condition{
			Status:             db.ConditionStatusTrue,
			Message:            "Each control-plane node is in its own failure domain.",
			LastTransitionTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		},
	}}
	mockTx := &testutil.MockTx{}

	r := &reconciler{db: mockTx}
	cp := &controlPlane{nodes: []db.Node{node}}
	fds := &failureDomains{eligible: []string{"rack-a"}, reportedByNodeName: map[string]string{"cp-1": "rack-a"}}

	require.NoError(t, r.reconcileFailureDomainStatus(context.Background(), cluster, cp, fds))
	mockTx.AssertNotCalled(t, "UpdateNodeObservedFailureDomain", mock.Anything, mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "UpdateClusterControlPlaneFailureDomains", mock.Anything, mock.Anything, mock.Anything)
}

func TestReconcileFailureDomainStatusClearsOnlyAnEmptyReport(t *testing.T) {
	cleared, missing := fdNode("cp-1", "rack-a", true, true), fdNode("cp-2", "rack-b", true, true)
	cleared.ObservedFailureDomain, missing.ObservedFailureDomain = "rack-c", "rack-d"
	mockTx := &testutil.MockTx{}
	mockTx.On("UpdateNodeObservedFailureDomain", mock.Anything, cleared.ID, "").Return(nil)
	mockTx.On("UpdateClusterControlPlaneFailureDomains", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	r := &reconciler{db: mockTx}
	cp := &controlPlane{nodes: []db.Node{cleared, missing}}
	fds := &failureDomains{eligible: []string{"rack-a", "rack-b"}, reportedByNodeName: map[string]string{"cp-1": ""}}

	require.NoError(t, r.reconcileFailureDomainStatus(context.Background(), db.Cluster{ID: uuid.New()}, cp, fds))
	mockTx.AssertExpectations(t)
	assert.Equal(t, "rack-a", cp.nodes[0].EffectiveFailureDomain())
	assert.Equal(t, "rack-d", cp.nodes[1].EffectiveFailureDomain())
}
