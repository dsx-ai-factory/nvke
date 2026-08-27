// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodestatus

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
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
	}

	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
	require.NoError(t, err)
	require.Empty(t, result)
	database.AssertExpectations(t)
	tx.AssertExpectations(t)
}
