// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// TestReconcileEndsThePassWhenRemediationRecordsADelete pins the ordering fix for
// the #784 follow-on. remediateMachines records delete intent through the DB, but
// the in-memory controlPlane the reconcile pass carries forward still holds the
// pre-remediation snapshot. Without ending the pass, reconcileNodesNeedingRollout
// runs on that stale view: its cp.nodeWithDeleteInFlight() guard does not see the
// just-recorded delete, so it records a second one in the same pass -- two
// control-plane deletes queued at once, which the serialized delete queue exists
// to prevent.
//
// The cluster here is set up so the rollout WOULD record a delete (a drifted node
// with surge headroom and healthy joined survivors) and a separate node is marked
// for recycling. With the fix, remediation's delete is the only one recorded and
// the rollout never runs; without it, the rollout queues a second delete.
func TestReconcileEndsThePassWhenRemediationRecordsADelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clusterID := uuid.New()
	currentConfigID := uuid.New()
	staleConfigID := uuid.New()
	currentSpec := nicoCPSpec("nvke-1.34.2", "", "1.34.2")
	staleSpec := staleCPSpec(t, currentSpec)

	cluster := db.Cluster{
		ID:                    clusterID,
		Name:                  "cp-remediation-ordering",
		KubernetesVersion:     currentSpec.KubernetesVersion,
		ControlPlaneVIP:       "10.0.0.100",
		EnableCPNodeRecycling: true,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID:        currentConfigID,
			DesiredCount:           3,
			MaxSurge:               1,
			ControlPlaneConfigSpec: currentSpec,
		},
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "test-org", SiteID: uuid.New(), VpcID: uuid.New()})
	// Already recycling, so the rollout path skips the cluster-condition write and
	// the only DB writes under test are node deletes.
	cluster.Conditions.CPNodesRecycling = db.NewCondition(true, "Control plane nodes recycling in progress")

	// A node marked for recycling, a drifted node that needs rollout, and three
	// healthy current-config nodes that give the rollout its surge headroom. The
	// rollout-required conditions are pre-set to the real drift so that step is a
	// no-op for the delete count. (It may still relock/rewrite the condition; the
	// assertion filters writes down to deletes.)
	marked := makeCPControllerNode("cp-marked", currentConfigID)
	marked.ClusterID = clusterID
	marked.Conditions.MarkedForRecycling = db.NewCondition(true, "Marked")
	marked.Conditions.ControlPlaneRolloutRequired = db.NewCondition(false, "converged")
	stale := makeCPControllerNode("cp-stale", staleConfigID)
	stale.ClusterID = clusterID
	stale.Conditions.ControlPlaneRolloutRequired = db.NewCondition(true, "drifted")
	healthy1 := makeCPControllerNode("cp-healthy-1", currentConfigID)
	healthy1.ClusterID = clusterID
	healthy1.Conditions.ControlPlaneRolloutRequired = db.NewCondition(false, "converged")
	healthy2 := makeCPControllerNode("cp-healthy-2", currentConfigID)
	healthy2.ClusterID = clusterID
	healthy2.Conditions.ControlPlaneRolloutRequired = db.NewCondition(false, "converged")
	healthy3 := makeCPControllerNode("cp-healthy-3", currentConfigID)
	healthy3.ClusterID = clusterID
	healthy3.Conditions.ControlPlaneRolloutRequired = db.NewCondition(false, "converged")
	allNodes := []db.Node{marked, stale, healthy1, healthy2, healthy3}

	mockTx := &testutil.MockTx{}
	cpTrue := true
	mockTx.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &cpTrue}).Return(allNodes, nil)
	mockTx.On("ListControlPlaneConfigurationsByIDs", mock.Anything, mock.Anything).Return([]db.ControlPlaneConfiguration{
		{ID: currentConfigID, Config: currentSpec},
		{ID: staleConfigID, Config: staleSpec},
	}, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	// Serve GetNode/GetNodeForUpdate for every node so both the remediation delete
	// (marked) and, without the fix, the rollout delete (stale) can be recorded.
	for _, node := range allNodes {
		mockTx.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
	}
	var deleteWrites []db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		if node := *args.Get(1).(*db.Node); nodes.IsDeleteRequested(node) || nodes.IsDeleting(node) {
			deleteWrites = append(deleteWrites, node)
		}
	}).Return(marked, nil)

	provisioned := true
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: constants.LegacyManagementNamespace},
		Status: clusterv1.ClusterStatus{
			Initialization: clusterv1.ClusterInitializationStatus{InfrastructureProvisioned: &provisioned},
			FailureDomains: []clusterv1.FailureDomain{
				{Name: "fd-a", ControlPlane: &provisioned},
				{Name: "fd-b", ControlPlane: &provisioned},
				{Name: "fd-c", ControlPlane: &provisioned},
			},
		},
	}).Build()

	r := newReconciler(config.Config{}, slog.New(slog.DiscardHandler), mockTx, nil, k8sClient, k8sClient)

	require.NoError(t, r.reconcile(ctx, cluster))

	require.Len(t, deleteWrites, 1,
		"exactly one delete must be recorded -- remediation's; ending the pass stops the rollout from queuing a second on the stale snapshot")
	assert.Equal(t, marked.ID, deleteWrites[0].ID, "the recorded delete must be the remediation-marked node, not the drifted one")
	assert.True(t, nodes.IsDeleteRequested(deleteWrites[0]), "remediation records delete intent")
	assert.False(t, nodes.IsDeleting(deleteWrites[0]), "the teardown waits for the serialized delete loop")
}
