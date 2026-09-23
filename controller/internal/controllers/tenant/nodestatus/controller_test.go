// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodestatus

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestUnit_NodeRequestsForDBNodeUsesNodeName(t *testing.T) {
	got, err := nodeRequestsForDBNode(context.Background(), db.Node{Name: "node-a"})
	require.NoError(t, err)
	require.Empty(t, got[0].Namespace)
	require.Equal(t, "node-a", got[0].Name)

	got, err = nodeRequestsForDBNode(context.Background(), db.Node{})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestUnit_ReconcileUpdatesKubernetesConditionsOnly(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.New()
	nodeID := uuid.New()
	nodeName := "worker-1"
	existing := db.Node{
		ID:        nodeID,
		ClusterID: clusterID,
		Name:      nodeName,
		Conditions: db.NodeConditions{
			NodeAgentReady: db.Condition{Status: db.ConditionStatusTrue, Message: "already checked"},
		},
		AgentVersion: "v1.2.3",
	}
	kubeNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:    corev1.NodeReady,
			Status:  corev1.ConditionTrue,
			Message: "kubelet ready",
		}}},
	}

	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.ClusterID != nil && *filters.ClusterID == clusterID &&
			filters.Name != nil && *filters.Name == nodeName
	})).Return([]db.Node{existing}, nil).Once()
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()

	tx := &testutil.MockTx{}
	database.MockTx = tx
	tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(existing, nil).Once()
	tx.On("GetNode", mock.Anything, nodeID).Return(existing, nil).Once()
	tx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.Conditions.KubernetesReady.Status == db.ConditionStatusTrue &&
			node.Conditions.KubernetesReady.Message == "kubelet ready" &&
			node.Conditions.KubernetesUnschedulable.Status == db.ConditionStatusFalse &&
			node.Conditions.NodeAgentReady.Status == db.ConditionStatusTrue &&
			node.Conditions.NodeAgentReady.Message == "already checked" &&
			node.AgentVersion == "v1.2.3"
	})).Return(existing, nil).Once()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kubeNode).Build()
	reconciler := &Reconciler{
		clusterID: clusterID,
		db:        dbwrapper.New(database, slog.New(slog.DiscardHandler)),
		client:    k8sClient,
		logger:    slog.New(slog.DiscardHandler),
	}

	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
	require.NoError(t, err)
	require.Empty(t, result)
	database.AssertExpectations(t)
	tx.AssertExpectations(t)
}

// TestUnit_ReconcileSkipsUpdateWhenHealthUnchanged guards against a
// self-retriggering UPDATE loop. The reconciler is woken by a database source
// watching the very rows it writes, so a change check that reports a
// difference between two identical condition sets never settles. Comparing
// conditions with == did exactly that: the tenant Kubernetes Node supplies
// LastTransitionTime through metav1.Time, which decodes into the local zone,
// while the stored condition comes back from JSONB in UTC, and == compares the
// *Location pointer as well as the instant.
func TestUnit_ReconcileSkipsUpdateWhenHealthUnchanged(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.New()
	nodeID := uuid.New()
	nodeName := "worker-1"
	readySince := time.Date(2026, 8, 17, 21, 21, 14, 0, time.UTC)

	existing := db.Node{
		ID:        nodeID,
		ClusterID: clusterID,
		Name:      nodeName,
		Conditions: db.NodeConditions{
			KubernetesReady: db.Condition{
				Status:             db.ConditionStatusTrue,
				Message:            "kubelet is posting ready status",
				LastTransitionTime: readySince,
			},
			KubernetesUnschedulable: db.Condition{
				Status:             db.ConditionStatusFalse,
				Message:            "Node is schedulable",
				LastTransitionTime: readySince,
			},
		},
	}
	kubeNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:    corev1.NodeReady,
			Status:  corev1.ConditionTrue,
			Message: "kubelet is posting ready status",
			// Same instant as the stored condition, in the representation an
			// apimachinery decode produces.
			LastTransitionTime: metav1.Time{Time: readySince.In(time.FixedZone("MDT", -6*60*60))},
		}}},
	}

	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{existing}, nil).Once()
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()

	tx := &testutil.MockTx{}
	database.MockTx = tx
	tx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(existing, nil).Once()
	tx.On("GetNode", mock.Anything, nodeID).Return(existing, nil).Maybe()
	// Registered so that a regression fails the AssertNotCalled below rather
	// than panicking inside the mock on an unexpected call.
	tx.On("UpdateNode", mock.Anything, mock.Anything).Return(existing, nil).Maybe()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kubeNode).Build()
	reconciler := &Reconciler{
		clusterID: clusterID,
		db:        dbwrapper.New(database, slog.New(slog.DiscardHandler)),
		client:    k8sClient,
		logger:    slog.New(slog.DiscardHandler),
	}

	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
	require.NoError(t, err)
	require.Empty(t, result)
	tx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	database.AssertExpectations(t)
	tx.AssertExpectations(t)
}

func TestUnit_DescribeConditionChanges(t *testing.T) {
	transition := time.Date(2026, 8, 17, 21, 21, 14, 0, time.UTC)
	previous := db.NodeConditions{
		KubernetesReady:         db.Condition{Status: db.ConditionStatusTrue, Message: "kubelet is posting ready status", LastTransitionTime: transition},
		KubernetesUnschedulable: db.Condition{Status: db.ConditionStatusFalse, Message: "Node is schedulable", LastTransitionTime: transition},
		NodeAgentReady:          db.Condition{Status: db.ConditionStatusTrue, Message: "Node agent is reachable", LastTransitionTime: transition},
	}

	require.Empty(t, describeConditionChanges(previous, previous))

	statusMoved := previous
	statusMoved.KubernetesReady.Status = db.ConditionStatusUnknown
	require.Equal(t, []string{"kubernetesReady: True -> Unknown"}, describeConditionChanges(previous, statusMoved))

	messageMoved := previous
	messageMoved.KubernetesUnschedulable.Message = "Node is unschedulable"
	require.Equal(t,
		[]string{"kubernetesUnschedulable: False unchanged, message or transition time updated"},
		describeConditionChanges(previous, messageMoved),
	)

	// Untouched conditions stay out of the line — that is the whole point of
	// reporting a delta rather than both condition sets.
	require.NotContains(t, describeConditionChanges(previous, statusMoved), "nodeAgentReady")
}
