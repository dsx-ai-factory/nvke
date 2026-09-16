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
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

type listCountingClient struct {
	client.Client
	listCalls int
}

// List records the call before delegating to the wrapped client.
func (c *listCountingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.listCalls++
	return c.Client.List(ctx, list, opts...)
}

func TestReadFailureDomains(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	clusterName := "test-cluster"
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))

	t.Run("ready cluster filters eligible domains and active control plane machines", func(t *testing.T) {
		t.Parallel()

		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: clusterName}, nil).Once()
		now := metav1.Now()
		objects := []client.Object{
			&clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: constants.NamespaceNKESystem},
				Status: clusterv1.ClusterStatus{
					Initialization: clusterv1.ClusterInitializationStatus{InfrastructureProvisioned: ptr.To(true)},
					FailureDomains: []clusterv1.FailureDomain{
						{Name: "fd-a", ControlPlane: ptr.To(true)},
						{Name: "worker-only", ControlPlane: ptr.To(false)},
					},
				},
			},
			failureDomainMachine(clusterName, "cp-status", "fd-spec", "fd-status", true, nil),
			failureDomainMachine(clusterName, "cp-spec", "fd-spec", "", true, nil),
			failureDomainMachine(clusterName, "worker", "fd-worker", "", false, nil),
			failureDomainMachine(clusterName, "deleting", "fd-deleting", "", true, &now),
		}
		k8sClient := &listCountingClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
		controller := NewController(
			config.Config{ProvisionControlPlaneWithControlPlaneController: true},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			k8sClient,
		)

		snapshot := controller.readFailureDomains(context.Background(), clusterID)

		require.NoError(t, snapshot.discoveryErr)
		assert.Equal(t, []string{"fd-a"}, snapshot.eligible)
		assert.Equal(t, map[string]string{
			"cp-status": "fd-status",
			"cp-spec":   "fd-spec",
		}, snapshot.observedByNodeName)
		assert.Equal(t, 1, k8sClient.listCalls)
		mockDB.AssertExpectations(t)
	})

	t.Run("ready cluster without eligible domains skips machine listing", func(t *testing.T) {
		t.Parallel()

		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: clusterName}, nil).Once()
		k8sClient := &listCountingClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			&clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: constants.NamespaceNKESystem},
				Status: clusterv1.ClusterStatus{
					Initialization: infrastructureProvisionedStatus(),
				},
			},
		).Build()}
		controller := NewController(
			config.Config{ProvisionControlPlaneWithControlPlaneController: true},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			k8sClient,
		)

		snapshot := controller.readFailureDomains(context.Background(), clusterID)

		require.NoError(t, snapshot.discoveryErr)
		assert.Empty(t, snapshot.eligible)
		assert.Zero(t, k8sClient.listCalls)
		mockDB.AssertExpectations(t)
	})
}

func TestControllerRequeuesWhileClusterInfrastructureIsNotReady(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cluster := db.Cluster{
		ID:                clusterID,
		Name:              "test-cluster",
		KubernetesVersion: "1.34.2",
		ControlPlaneConfig: db.ControlPlaneConfig{
			DesiredCount: 1,
		},
	}
	node := db.Node{
		ID:           uuid.New(),
		ClusterID:    clusterID,
		ControlPlane: true,
		Name:         "cp-existing",
		Conditions: db.NodeConditions{
			Provisioned:     db.NewCondition(true, "provisioned"),
			KubernetesReady: db.NewCondition(true, "ready"),
		},
	}
	mockTx := &testutil.MockTx{}
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil).Once()
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{node}, nil).Once()
	mockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Twice()
	mockDB := &testutil.MockDB{MockTx: mockTx}
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: constants.NamespaceNKESystem},
	}).Build()
	controller := NewController(
		config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		slog.Default(),
		dbwrapper.New(mockDB, slog.Default()),
		nil,
		k8sClient,
	)

	result, err := controller.Reconcile(context.Background(), dbreconcile.ClusterKey(clusterID))

	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, result.RequeueAfter)
	mockDB.AssertExpectations(t)
	mockTx.AssertExpectations(t)
}

// infrastructureProvisionedStatus returns a status with infrastructure marked ready.
func infrastructureProvisionedStatus() clusterv1.ClusterInitializationStatus {
	return clusterv1.ClusterInitializationStatus{InfrastructureProvisioned: ptr.To(true)}
}

// failureDomainMachine builds a CAPI Machine for failure-domain discovery tests.
func failureDomainMachine(
	clusterName,
	name,
	specDomain,
	statusDomain string,
	controlPlane bool,
	deletionTimestamp *metav1.Time,
) *clusterv1.Machine {
	labels := map[string]string{clusterv1.ClusterNameLabel: clusterName}
	if controlPlane {
		labels[clusterv1.MachineControlPlaneLabel] = ""
	}
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         constants.NamespaceNKESystem,
			Labels:            labels,
			DeletionTimestamp: deletionTimestamp,
			Finalizers:        []string{"test.finalizer"},
		},
		Spec:   clusterv1.MachineSpec{FailureDomain: specDomain},
		Status: clusterv1.MachineStatus{FailureDomain: statusDomain},
	}
}

func TestPrepareFailureDomainAssignmentsBackfillsAndSpreadsPendingNodes(t *testing.T) {
	t.Parallel()

	activeID := uuid.New()
	pendingBID := uuid.New()
	pendingCID := uuid.New()
	cp := &controlPlane{nodes: []db.Node{
		{
			ID:           activeID,
			Name:         "cp-a",
			ControlPlane: true,
			Conditions: db.NodeConditions{
				Provisioned: db.NewCondition(true, "provisioned"),
			},
		},
		{
			ID:           pendingBID,
			Name:         "cp-b",
			ControlPlane: true,
			Conditions: db.NodeConditions{
				Pending: db.NewCondition(true, "pending"),
			},
		},
		{
			ID:           pendingCID,
			Name:         "cp-c",
			ControlPlane: true,
			Conditions: db.NodeConditions{
				Pending: db.NewCondition(true, "pending"),
			},
		},
	}}

	mockTx := &testutil.MockTx{}
	for nodeName, domain := range map[string]string{
		"cp-a": "fd-a",
		"cp-b": "fd-b",
		"cp-c": "fd-c",
	} {
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
			return node.Name == nodeName && ptr.Deref(node.FailureDomain, "") == domain
		})).Return(db.Node{}, nil).Once()
	}

	r := &reconciler{
		cfg:    config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:     mockTx,
		logger: slog.Default(),
		failureDomainSnapshot: failureDomainSnapshot{
			eligible:           []string{"fd-c", "fd-b", "fd-a"},
			observedByNodeName: map[string]string{"cp-a": "fd-a"},
		},
	}
	require.NoError(t, r.prepareFailureDomainAssignments(context.Background(), cp))

	assert.Equal(t, "fd-a", ptr.Deref(cp.nodes[0].FailureDomain, ""))
	assert.Equal(t, "fd-b", ptr.Deref(cp.nodes[1].FailureDomain, ""))
	assert.Equal(t, "fd-c", ptr.Deref(cp.nodes[2].FailureDomain, ""))
	assert.False(t, r.retryFailureDomains)
	mockTx.AssertExpectations(t)
}

func TestPrepareFailureDomainAssignmentsWaitsForExistingPlacement(t *testing.T) {
	t.Parallel()

	cp := &controlPlane{nodes: []db.Node{{
		ID:           uuid.New(),
		Name:         "cp-existing",
		ControlPlane: true,
		Conditions: db.NodeConditions{
			Provisioned: db.NewCondition(true, "provisioned"),
		},
	}}}
	r := &reconciler{
		cfg:                   config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:                    &testutil.MockTx{},
		logger:                slog.Default(),
		failureDomainSnapshot: failureDomainSnapshot{eligible: []string{"fd-a", "fd-b"}},
	}

	require.NoError(t, r.prepareFailureDomainAssignments(context.Background(), cp))
	assert.True(t, r.retryFailureDomains)
	assert.Equal(t, []string{"cp-existing"}, r.unknownDomains)
	_, err := r.selectFailureDomain()
	require.ErrorContains(t, err, "waiting for observed failure domains")
}

func TestPrepareFailureDomainAssignmentsMarksUnsupportedProviderComplete(t *testing.T) {
	t.Parallel()

	cp := &controlPlane{nodes: []db.Node{{
		ID:           uuid.New(),
		Name:         "cp-pending",
		ControlPlane: true,
		Conditions: db.NodeConditions{
			Pending: db.NewCondition(true, "pending"),
		},
	}}}
	mockTx := &testutil.MockTx{}
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.Name == "cp-pending" && node.FailureDomain != nil && *node.FailureDomain == ""
	})).Return(db.Node{}, nil).Once()
	r := &reconciler{
		cfg:                   config.Config{ProvisionControlPlaneWithControlPlaneController: true},
		db:                    mockTx,
		logger:                slog.Default(),
		failureDomainSnapshot: failureDomainSnapshot{},
	}

	require.NoError(t, r.prepareFailureDomainAssignments(context.Background(), cp))
	require.NotNil(t, cp.nodes[0].FailureDomain)
	assert.Empty(t, *cp.nodes[0].FailureDomain)
	assert.False(t, r.retryFailureDomains)
	mockTx.AssertExpectations(t)
}
