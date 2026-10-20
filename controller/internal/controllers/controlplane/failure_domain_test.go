// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
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

type listErrorClient struct {
	client.Client
	err error
}

func (c *listErrorClient) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return c.err
}

func TestReadFailureDomains(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	clusterName := "test-cluster"
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))

	t.Run("ready cluster filters eligible domains and active control plane machines", func(t *testing.T) {
		t.Parallel()

		now := metav1.Now()
		objects := []client.Object{
			&clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: constants.LegacyManagementNamespace},
				Status: clusterv1.ClusterStatus{
					Initialization: clusterv1.ClusterInitializationStatus{InfrastructureProvisioned: new(true)},
					FailureDomains: []clusterv1.FailureDomain{
						{Name: "fd-a", ControlPlane: new(true)},
						{Name: "worker-only", ControlPlane: new(false)},
					},
				},
			},
			failureDomainMachine(clusterName, "cp-status", "fd-spec", "fd-status", true, nil),
			failureDomainMachine(clusterName, "cp-spec", "fd-spec", "", true, nil),
			failureDomainMachine(clusterName, "cp-unobserved", "", "", true, nil),
			failureDomainMachine(clusterName, "worker", "fd-worker", "", false, nil),
			failureDomainMachine(clusterName, "deleting", "fd-deleting", "", true, &now),
		}
		k8sClient := &listCountingClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
		r := &reconciler{
			k8sClient: k8sClient,
		}

		domains, err := r.readFailureDomains(context.Background(), db.Cluster{ID: clusterID, Name: clusterName})

		require.NoError(t, err)
		require.NotNil(t, domains)
		assert.Equal(t, []string{"fd-a"}, domains.eligible)
		assert.Equal(t, map[string]string{
			"cp-status": "fd-status",
			"cp-spec":   "fd-spec",
		}, domains.observedByNodeName)
		assert.Equal(t, map[string]string{"cp-status": "fd-status", "cp-spec": "", "cp-unobserved": ""}, domains.reportedByNodeName)
		assert.Equal(t, 1, k8sClient.listCalls)
	})

	t.Run("ready cluster without eligible domains still reads reported placement", func(t *testing.T) {
		t.Parallel()

		k8sClient := &listCountingClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			&clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: constants.LegacyManagementNamespace},
				Status: clusterv1.ClusterStatus{
					Initialization: infrastructureProvisionedStatus(),
				},
			},
			failureDomainMachine(clusterName, "cp-1", "", "rack-a", true, nil),
		).Build()}
		r := &reconciler{
			k8sClient: k8sClient,
		}

		domains, err := r.readFailureDomains(context.Background(), db.Cluster{ID: clusterID, Name: clusterName})

		require.NoError(t, err)
		require.NotNil(t, domains)
		assert.Empty(t, domains.eligible)
		assert.Equal(t, map[string]string{"cp-1": "rack-a"}, domains.reportedByNodeName)
		assert.Equal(t, 1, k8sClient.listCalls)
	})
}

func TestReadFailureDomainsWithoutAPlacementView(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	clusterName := "test-cluster"
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	readyCluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: constants.LegacyManagementNamespace},
		Status: clusterv1.ClusterStatus{
			Initialization: infrastructureProvisionedStatus(),
			FailureDomains: []clusterv1.FailureDomain{{Name: "fd-a", ControlPlane: new(true)}},
		},
	}

	tests := map[string]struct {
		k8sClient func() client.Client
		wantErr   string
	}{
		"kubernetes client is not configured": {
			k8sClient: func() client.Client { return nil },
			wantErr:   "kubernetes client is not configured",
		},
		"capi cluster is missing": {
			k8sClient: func() client.Client {
				return fake.NewClientBuilder().WithScheme(scheme).Build()
			},
			wantErr: "get CAPI cluster for failure-domain discovery",
		},
		"machine listing fails": {
			k8sClient: func() client.Client {
				return &listErrorClient{
					Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(readyCluster).Build(),
					err:    errors.New("api server unavailable"),
				}
			},
			wantErr: "list CAPI machines for failure-domain discovery",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var k8sClient client.Client
			if tc.k8sClient != nil {
				k8sClient = tc.k8sClient()
			}
			r := &reconciler{k8sClient: k8sClient}

			domains, err := r.readFailureDomains(context.Background(), db.Cluster{ID: clusterID, Name: clusterName})

			if tc.wantErr == "" {
				require.NoError(t, err)
				require.NotNil(t, domains)
				assert.Empty(t, domains.eligible)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, domains)
			}
		})
	}
}

func TestControllerReturnsErrorWhileClusterInfrastructureIsNotReady(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cluster := db.Cluster{
		ID:                clusterID,
		Name:              "test-cluster",
		KubernetesVersion: "1.34.2",
		ProviderType:      db.ProviderTypeCAPI,
		ControlPlaneConfig: db.ControlPlaneConfig{
			DesiredCount: 2,
		},
	}
	nodes := []db.Node{
		{
			ID:            uuid.New(),
			ClusterID:     clusterID,
			ControlPlane:  true,
			Name:          "cp-existing",
			FailureDomain: "fd-a",
			Conditions: db.NodeConditions{
				Provisioned:     db.NewCondition(true, "provisioned"),
				KubernetesReady: db.NewCondition(true, "ready"),
			},
		},
		{
			ID:            uuid.New(),
			ClusterID:     clusterID,
			ControlPlane:  true,
			Name:          "cp-pending",
			FailureDomain: "fd-b",
			Conditions: db.NodeConditions{
				Pending: db.NewCondition(true, "pending"),
			},
		},
	}
	mockTx := &testutil.MockTx{}
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil).Once()
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Once()
	mockDB := &testutil.MockDB{MockTx: mockTx}
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: constants.LegacyManagementNamespace},
	}).Build()
	controller := NewController(
		config.Config{},
		slog.Default(),
		dbwrapper.New(mockDB, slog.Default()),
		nil,
		k8sClient,
		k8sClient,
		nil,
	)

	result, err := controller.Reconcile(context.Background(), dbreconcile.ClusterKey(clusterID))

	require.ErrorContains(t, err, "discovering control-plane failure domains: capi cluster infrastructure is not yet provisioned")
	assert.Zero(t, result)
	mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	mockDB.AssertExpectations(t)
	mockTx.AssertExpectations(t)
}

// infrastructureProvisionedStatus returns a status with infrastructure marked ready.
func infrastructureProvisionedStatus() clusterv1.ClusterInitializationStatus {
	return clusterv1.ClusterInitializationStatus{InfrastructureProvisioned: new(true)}
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
			Namespace:         constants.LegacyManagementNamespace,
			Labels:            labels,
			DeletionTimestamp: deletionTimestamp,
			Finalizers:        []string{"test.finalizer"},
		},
		Spec:   clusterv1.MachineSpec{FailureDomain: specDomain},
		Status: clusterv1.MachineStatus{FailureDomain: statusDomain},
	}
}

// assertControlPlaneNodesPlaced asserts that the nodes created by this test's
// reconcile already have a failure-domain decision in their initial row.
func assertControlPlaneNodesPlaced(ctx context.Context, t *testing.T, database db.DB, clusterID uuid.UUID) []db.Node {
	t.Helper()

	controlPlane := true
	nodes, err := database.ListNodes(ctx, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &controlPlane})
	require.NoError(t, err)
	require.NotEmpty(t, nodes, "expected control-plane nodes")

	for _, node := range nodes {
		assert.NotEmpty(t, node.FailureDomain, "node %s was left unplaced by reconcile", node.Name)
	}
	return nodes
}

func TestReconcilePlacesTheNodesItCreatedInTheSamePass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	bunDB := testutil.SetupDB(ctx, t,
		(*db.Cluster)(nil),
		(*db.ControlPlaneConfiguration)(nil),
		(*db.NodePool)(nil),
		(*db.NodeSet)(nil),
		(*db.Node)(nil),
	)
	// reconcile runs inside RunInTx below, so the FOR UPDATE paths SQLite
	// cannot execute are never reached.
	database := db.NewDB(bunDB)

	const kubernetesVersion = "1.31.0"
	configID := uuid.New()
	cluster := db.Cluster{
		ID:                uuid.New(),
		Name:              "test-cluster",
		KubernetesVersion: kubernetesVersion,
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID:        configID,
			DesiredCount:           3,
			ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{KubernetesVersion: kubernetesVersion},
		},
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "test-org", SiteID: uuid.New(), VpcID: uuid.New()})
	_, err := database.CreateCluster(ctx, &cluster)
	require.NoError(t, err)
	_, err = database.CreateControlPlaneConfiguration(ctx, &db.ControlPlaneConfiguration{
		ID:        configID,
		ClusterID: cluster.ID,
		Config:    cluster.ControlPlaneConfig.ControlPlaneConfigSpec,
	})
	require.NoError(t, err)

	require.NoError(t, database.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		scheme := runtime.NewScheme()
		require.NoError(t, clusterv1.AddToScheme(scheme))
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: constants.LegacyManagementNamespace},
			Status: clusterv1.ClusterStatus{
				Initialization: infrastructureProvisionedStatus(),
				FailureDomains: []clusterv1.FailureDomain{
					{Name: "fd-c", ControlPlane: new(true)},
					{Name: "fd-a", ControlPlane: new(true)},
					{Name: "fd-b", ControlPlane: new(true)},
				},
			},
		}).Build()
		r := newReconciler(
			config.Config{},
			slog.New(slog.DiscardHandler),
			tx,
			nil,
			k8sClient,
			k8sClient,
		)
		require.NoError(t, r.reconcile(ctx, cluster))
		return nil
	}))

	nodes := assertControlPlaneNodesPlaced(ctx, t, database, cluster.ID)

	require.Len(t, nodes, 3)
	placed := make([]string, 0, len(nodes))
	for _, node := range nodes {
		placed = append(placed, node.FailureDomain)
	}
	assert.ElementsMatch(t, []string{"fd-a", "fd-b", "fd-c"}, placed,
		"scale-up in the same pass should spread across the eligible domains")
}
