// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
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

		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, Name: clusterName}, nil).Once()
		now := metav1.Now()
		objects := []client.Object{
			&clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: constants.NamespaceNKESystem},
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
		controller := NewController(
			config.Config{ProvisionControlPlaneWithControlPlaneController: true},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			k8sClient,
		)

		domains, err := controller.readFailureDomains(context.Background(), clusterID)

		require.NoError(t, err)
		require.NotNil(t, domains)
		assert.Equal(t, []string{"fd-a"}, domains.eligible)
		assert.Equal(t, map[string]string{
			"cp-status": "fd-status",
			"cp-spec":   "fd-spec",
		}, domains.observedByNodeName)
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

		domains, err := controller.readFailureDomains(context.Background(), clusterID)

		require.NoError(t, err)
		require.NotNil(t, domains)
		assert.Empty(t, domains.eligible)
		assert.Zero(t, k8sClient.listCalls)
		mockDB.AssertExpectations(t)
	})
}

func TestReadFailureDomainsWithoutAPlacementView(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	clusterName := "test-cluster"
	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	readyCluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: constants.NamespaceNKESystem},
		Status: clusterv1.ClusterStatus{
			Initialization: infrastructureProvisionedStatus(),
			FailureDomains: []clusterv1.FailureDomain{{Name: "fd-a", ControlPlane: new(true)}},
		},
	}

	tests := map[string]struct {
		cfg        config.Config
		cluster    db.Cluster
		clusterErr error
		k8sClient  func() client.Client
		wantErr    string
	}{
		"controller does not own the control plane": {
			cfg: config.Config{},
		},
		"cluster row is gone": {
			cfg:        config.Config{ProvisionControlPlaneWithControlPlaneController: true},
			clusterErr: db.ErrNotFound,
		},
		"cluster is being deleted": {
			cfg: config.Config{ProvisionControlPlaneWithControlPlaneController: true},
			cluster: db.Cluster{
				ID:         clusterID,
				Name:       clusterName,
				Conditions: db.ClusterConditions{Deleting: db.NewCondition(true, "deleting")},
			},
		},
		"cluster read fails": {
			cfg:        config.Config{ProvisionControlPlaneWithControlPlaneController: true},
			clusterErr: errors.New("connection refused"),
			wantErr:    "get cluster for failure-domain discovery",
		},
		"kubernetes client is not configured": {
			cfg:       config.Config{ProvisionControlPlaneWithControlPlaneController: true},
			cluster:   db.Cluster{ID: clusterID, Name: clusterName},
			k8sClient: func() client.Client { return nil },
			wantErr:   "kubernetes client is not configured",
		},
		"capi cluster is missing": {
			cfg:     config.Config{ProvisionControlPlaneWithControlPlaneController: true},
			cluster: db.Cluster{ID: clusterID, Name: clusterName},
			k8sClient: func() client.Client {
				return fake.NewClientBuilder().WithScheme(scheme).Build()
			},
			wantErr: "get CAPI cluster for failure-domain discovery",
		},
		"machine listing fails": {
			cfg:     config.Config{ProvisionControlPlaneWithControlPlaneController: true},
			cluster: db.Cluster{ID: clusterID, Name: clusterName},
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

			mockDB := &testutil.MockDB{}
			if tc.cfg.ProvisionControlPlaneWithControlPlaneController {
				mockDB.On("GetCluster", mock.Anything, clusterID).Return(tc.cluster, tc.clusterErr).Once()
			}
			var k8sClient client.Client
			if tc.k8sClient != nil {
				k8sClient = tc.k8sClient()
			}
			controller := NewController(tc.cfg, slog.Default(), dbwrapper.New(mockDB, slog.Default()), nil, k8sClient)

			domains, err := controller.readFailureDomains(context.Background(), clusterID)

			if tc.wantErr == "" {
				require.NoError(t, err)
				require.NotNil(t, domains)
				assert.Empty(t, domains.eligible)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, domains)
			}
			mockDB.AssertExpectations(t)
		})
	}
}

func TestControllerRequeuesWhileClusterInfrastructureIsNotReady(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	cluster := db.Cluster{
		ID:                clusterID,
		Name:              "test-cluster",
		KubernetesVersion: "1.34.2",
		ControlPlaneConfig: db.ControlPlaneConfig{
			DesiredCount: 2,
		},
	}
	nodes := []db.Node{
		{
			ID:           uuid.New(),
			ClusterID:    clusterID,
			ControlPlane: true,
			Name:         "cp-existing",
			Conditions: db.NodeConditions{
				Provisioned:     db.NewCondition(true, "provisioned"),
				KubernetesReady: db.NewCondition(true, "ready"),
			},
		},
		{
			ID:           uuid.New(),
			ClusterID:    clusterID,
			ControlPlane: true,
			Name:         "cp-pending",
			Conditions: db.NodeConditions{
				Pending: db.NewCondition(true, "pending"),
			},
		},
	}
	mockTx := &testutil.MockTx{}
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil).Once()
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodes, nil).Twice()
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
			Namespace:         constants.NamespaceNKESystem,
			Labels:            labels,
			DeletionTimestamp: deletionTimestamp,
			Finalizers:        []string{"test.finalizer"},
		},
		Spec:   clusterv1.MachineSpec{FailureDomain: specDomain},
		Status: clusterv1.MachineStatus{FailureDomain: statusDomain},
	}
}

// assertControlPlaneNodesPlaced asserts that the cluster's control-plane nodes
// that are still waiting for infrastructure all have a failure domain. It returns
// all of the cluster's control-plane nodes regardless of the assertion.
func assertControlPlaneNodesPlaced(ctx context.Context, t *testing.T, database db.DB, clusterID uuid.UUID) []db.Node {
	t.Helper()

	controlPlane := true
	nodes, err := database.ListNodes(ctx, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &controlPlane})
	require.NoError(t, err)
	require.NotEmpty(t, nodes, "expected control-plane nodes")

	for _, node := range nodes {
		// A node whose infrastructure already exists may legitimately be waiting
		// for CAPI to report its placement.
		if node.IsMarkedForDeletion() || node.HasInfrastructureProvisioningStarted() {
			continue
		}
		assert.NotNil(t, node.FailureDomain, "node %s was left unplaced by reconcile", node.Name)
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
		r := newReconciler(
			config.Config{ProvisionControlPlaneWithControlPlaneController: true},
			slog.New(slog.DiscardHandler),
			tx,
			nil,
			&failureDomains{eligible: []string{"fd-c", "fd-a", "fd-b"}},
		)
		result, err := r.reconcile(ctx, cluster)
		require.NoError(t, err)
		assert.Zero(t, result.RequeueAfter, "placement should not defer when discovery is healthy")
		return nil
	}))

	nodes := assertControlPlaneNodesPlaced(ctx, t, database, cluster.ID)

	require.Len(t, nodes, 3)
	placed := make([]string, 0, len(nodes))
	for _, node := range nodes {
		placed = append(placed, node.GetFailureDomain())
	}
	assert.ElementsMatch(t, []string{"fd-a", "fd-b", "fd-c"}, placed,
		"scale-up in the same pass should spread across the eligible domains")
}
