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
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

type getErrorClient struct {
	client.Client
	err error
}

func (c *getErrorClient) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return c.err
}

// discoveryFailureClients returns, per discovery failure, the Kubernetes client
// that produces it for a cluster named clusterName.
func discoveryFailureClients(t *testing.T, clusterName string) map[string]func() client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	readyCluster := func() *clusterv1.Cluster {
		return &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: constants.LegacyManagementNamespace},
			Status: clusterv1.ClusterStatus{
				Initialization: infrastructureProvisionedStatus(),
				FailureDomains: []clusterv1.FailureDomain{
					{Name: "fd-a", ControlPlane: new(true)},
					{Name: "fd-b", ControlPlane: new(true)},
					{Name: "fd-c", ControlPlane: new(true)},
				},
			},
		}
	}

	return map[string]func() client.Client{
		"kubernetes client is not configured": func() client.Client { return nil },
		"capi cluster read fails": func() client.Client {
			return &getErrorClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), err: errors.New("api server unavailable")}
		},
		"capi machine listing fails": func() client.Client {
			return &listErrorClient{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(readyCluster()).Build(),
				err:    errors.New("api server unavailable"),
			}
		},
		"capi cluster infrastructure is not provisioned": func() client.Client {
			return fake.NewClientBuilder().WithScheme(scheme).WithObjects(&clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: constants.LegacyManagementNamespace},
			}).Build()
		},
	}
}

// newDiscoveryFailureReconciler builds a reconciler whose clients are typed nil
// when k8sClient is nil, which is how the controller is wired without one.
func newDiscoveryFailureReconciler(tx db.Tx, k8sClient client.Client) *reconciler {
	if k8sClient == nil {
		return newReconciler(config.Config{}, slog.New(slog.DiscardHandler), tx, nil, nil, nil)
	}
	return newReconciler(config.Config{}, slog.New(slog.DiscardHandler), tx, nil, k8sClient, k8sClient)
}

// TestReconcileRemediatesWhenFailureDomainDiscoveryFails pins #1640: a discovery
// failure of any kind, not only infrastructure that is not yet provisioned, must
// let remediation record its delete, and must not fail the reconcile, whose error
// would roll that write back.
func TestReconcileRemediatesWhenFailureDomainDiscoveryFails(t *testing.T) {
	t.Parallel()

	const clusterName = "cp-discovery-failure-remediation"

	for name, k8sClient := range discoveryFailureClients(t, clusterName) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			clusterID := uuid.New()
			configID := uuid.New()
			spec := nicoCPSpec("nvke-1.34.2", "", "1.34.2")
			cluster := db.Cluster{
				ID:                    clusterID,
				Name:                  clusterName,
				KubernetesVersion:     spec.KubernetesVersion,
				ControlPlaneVIP:       "10.0.0.100",
				EnableCPNodeRecycling: true,
				ControlPlaneConfig: db.ControlPlaneConfig{
					ConfigurationID:        configID,
					DesiredCount:           3,
					MaxSurge:               1,
					ControlPlaneConfigSpec: spec,
				},
			}
			cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "test-org", SiteID: uuid.New(), VpcID: uuid.New()})

			marked := makeCPControllerNode("cp-marked", configID)
			marked.Conditions.MarkedForRecycling = db.NewCondition(true, "Marked")
			allNodes := []db.Node{
				marked,
				makeCPControllerNode("cp-healthy-1", configID),
				makeCPControllerNode("cp-healthy-2", configID),
				makeCPControllerNode("cp-healthy-3", configID),
			}
			for i := range allNodes {
				allNodes[i].ClusterID = clusterID
				allNodes[i].Conditions.ControlPlaneRolloutRequired = db.NewCondition(false, "converged")
			}

			mockTx := &testutil.MockTx{}
			cpTrue := true
			mockTx.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &cpTrue}).Return(allNodes, nil)
			mockTx.On("ListControlPlaneConfigurationsByIDs", mock.Anything, mock.Anything).
				Return([]db.ControlPlaneConfiguration{{ID: configID, Config: spec}}, nil)
			mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
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

			r := newDiscoveryFailureReconciler(mockTx, k8sClient())

			require.NoError(t, r.reconcile(context.Background(), cluster),
				"a discovery failure must not roll back the reconcile's remediation write")
			assert.Equal(t, failureDomainRetryInterval, r.requeue, "the cluster is requeued so discovery is retried")
			require.Len(t, deleteWrites, 1, "remediation must still record the delete of the marked node")
			assert.Equal(t, marked.ID, deleteWrites[0].ID)
		})
	}
}

// TestReconcileDoesNotScaleUpWhenFailureDomainDiscoveryFails pins the other half
// of #1640: the steps that choose a failure domain wait for discovery, so a
// control plane below its desired count gets no new node until it succeeds.
func TestReconcileDoesNotScaleUpWhenFailureDomainDiscoveryFails(t *testing.T) {
	t.Parallel()

	const clusterName = "cp-discovery-failure-scale-up"

	for name, k8sClient := range discoveryFailureClients(t, clusterName) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			clusterID := uuid.New()
			configID := uuid.New()
			spec := nicoCPSpec("nvke-1.34.2", "", "1.34.2")
			cluster := db.Cluster{
				ID:                clusterID,
				Name:              clusterName,
				KubernetesVersion: spec.KubernetesVersion,
				ControlPlaneVIP:   "10.0.0.100",
				ControlPlaneConfig: db.ControlPlaneConfig{
					ConfigurationID:        configID,
					DesiredCount:           3,
					ControlPlaneConfigSpec: spec,
				},
			}
			cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "test-org", SiteID: uuid.New(), VpcID: uuid.New()})

			healthy := makeCPControllerNode("cp-healthy-1", configID)
			healthy.ClusterID = clusterID
			healthy.Conditions.ControlPlaneRolloutRequired = db.NewCondition(false, "converged")

			mockTx := &testutil.MockTx{}
			cpTrue := true
			mockTx.On("ListNodes", mock.Anything, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &cpTrue}).Return([]db.Node{healthy}, nil)
			mockTx.On("ListControlPlaneConfigurationsByIDs", mock.Anything, mock.Anything).
				Return([]db.ControlPlaneConfiguration{{ID: configID, Config: spec}}, nil)
			// The rollout-required status step may refresh the node's condition; that
			// write is not under test.
			mockTx.On("GetNodeForUpdate", mock.Anything, healthy.ID).Return(healthy, nil).Maybe()
			mockTx.On("UpdateNode", mock.Anything, mock.Anything).Return(healthy, nil).Maybe()

			r := newDiscoveryFailureReconciler(mockTx, k8sClient())

			require.NoError(t, r.reconcile(context.Background(), cluster))
			assert.Equal(t, failureDomainRetryInterval, r.requeue)
			mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
		})
	}
}
