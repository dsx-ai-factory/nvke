// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodetpmekpub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"log/slog"
	"math/big"
	"os"
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

	forgeapi "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
	"github.com/NVIDIA/nke/controller/internal/tpm"
)

func TestUnit_ReconcileForgeNodeEKPub(t *testing.T) {
	pending := forgeapi.INSTANCESTATUS_PENDING

	type parameters struct {
		node           db.Node
		cluster        db.Cluster
		forgeProvider  *recordingNodeTPMForgeProvider
		wantOrgID      string
		wantSiteID     string
		wantInstanceID string
	}

	clusterID := uuid.MustParse("40000000-0000-0000-0000-000000000001")
	emptyForgeNodeID := uuid.MustParse("40000000-0000-0000-0000-000000000002")
	payloadNodeID := uuid.MustParse("40000000-0000-0000-0000-000000000003")
	siteID := uuid.MustParse("40000000-0000-0000-0000-000000000004")
	vpcID := uuid.MustParse("40000000-0000-0000-0000-000000000005")
	tests := map[string]parameters{
		"requeues without forge instance": {
			node: db.Node{
				ID:           emptyForgeNodeID,
				ClusterID:    clusterID,
				ControlPlane: true,
				Name:         "cp-no-pool",
				IP:           "10.0.0.1",
				Nico:         &db.NicoNodeProvider{},
			},
			cluster: db.Cluster{
				ID:           clusterID,
				ProviderType: db.ProviderTypeNico,
				Nico: &db.NicoClusterProvider{
					OrgID:  "org-1",
					SiteID: siteID,
					VpcID:  vpcID,
				},
			},
			forgeProvider: &recordingNodeTPMForgeProvider{
				instance: &forgeapi.Instance{Status: &pending},
			},
		},
		"uses nico node provider": {
			node: db.Node{
				ID:              payloadNodeID,
				ClusterID:       clusterID,
				Name:            "worker-0",
				ForgeInstanceID: "legacy-instance",
				Nico: &db.NicoNodeProvider{
					ForgeInstanceID: "payload-instance",
				},
			},
			cluster: db.Cluster{
				ID:           clusterID,
				ProviderType: db.ProviderTypeNico,
				Nico: &db.NicoClusterProvider{
					OrgID:  "org-1",
					SiteID: siteID,
					VpcID:  vpcID,
				},
			},
			forgeProvider: &recordingNodeTPMForgeProvider{
				instance: &forgeapi.Instance{Status: &pending},
			},
			wantOrgID:      "org-1",
			wantSiteID:     siteID.String(),
			wantInstanceID: "payload-instance",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			controller := &Controller{
				logger:        slog.New(slog.NewTextHandler(os.Stdout, nil)),
				forgeProvider: params.forgeProvider,
			}

			result, err := controller.reconcileForgeNodeEKPub(context.Background(), params.node, params.cluster)

			require.ErrorIs(t, err, errTPMDataNotReady)
			assert.Nil(t, result)
			assert.Equal(t, params.wantOrgID, params.forgeProvider.orgID)
			assert.Equal(t, params.wantSiteID, params.forgeProvider.siteID)
			assert.Equal(t, params.wantInstanceID, params.forgeProvider.instanceID)
		})
	}
}

func TestUnit_IsForgeInstanceAvailable(t *testing.T) {
	ready := forgeapi.INSTANCESTATUS_READY
	pending := forgeapi.INSTANCESTATUS_PENDING

	assert.True(t, isForgeInstanceAvailable(&forgeapi.Instance{Status: &ready}))
	assert.False(t, isForgeInstanceAvailable(&forgeapi.Instance{Status: &pending}))
	assert.False(t, isForgeInstanceAvailable(&forgeapi.Instance{}))
	assert.False(t, isForgeInstanceAvailable(nil))
}

type recordingNodeTPMForgeProvider struct {
	orgID      string
	siteID     string
	instanceID string
	instance   *forgeapi.Instance
}

func (p *recordingNodeTPMForgeProvider) GetInstanceByID(_ context.Context, orgID, siteID, instanceID string) (*forgeapi.Instance, error) {
	p.orgID = orgID
	p.siteID = siteID
	p.instanceID = instanceID
	return p.instance, nil
}

func TestUnit_IsNicoMachineAvailable(t *testing.T) {
	assert.True(t, isNicoMachineAvailable(&nicov1.NicoMachine{
		Status: nicov1.NicoMachineStatus{Ready: true},
	}))
	assert.True(t, isNicoMachineAvailable(&nicov1.NicoMachine{
		Status: nicov1.NicoMachineStatus{
			Conditions: []metav1.Condition{{
				Type:   clusterv1.ReadyCondition,
				Status: metav1.ConditionTrue,
			}},
		},
	}))
	assert.False(t, isNicoMachineAvailable(&nicov1.NicoMachine{
		Status: nicov1.NicoMachineStatus{
			Conditions: []metav1.Condition{{
				Type:   clusterv1.ReadyCondition,
				Status: metav1.ConditionFalse,
			}},
		},
	}))
}

func TestUnit_ReconcileNicoMachineEKPub_PersistsHashWhenReady(t *testing.T) {
	ctx := context.Background()

	clusterID := uuid.MustParse("41000000-0000-0000-0000-000000000001")
	nodeID := uuid.MustParse("41000000-0000-0000-0000-000000000002")
	node := db.Node{
		ID:        nodeID,
		ClusterID: clusterID,
		Name:      "worker-0",
	}
	cluster := db.Cluster{
		ID:   clusterID,
		Name: "capi-cluster",
	}

	tpmHash := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f60001"
	nicoMachine := &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: constants.NamespaceNKESystem,
		},
		Spec: nicov1.NicoMachineSpec{
			ProviderID: "nico://instance-1",
		},
		Status: nicov1.NicoMachineStatus{
			Ready:        true,
			TpmEkPubHash: tpmHash,
		},
	}

	k8sClient := newControllerTestClient(t, nicoMachine)
	controller := NewController(
		config.Config{EnableForgeCapi: true},
		slog.New(slog.NewTextHandler(os.Stdout, nil)),
		nil,
		nil,
		k8sClient,
	)

	result, err := controller.reconcileNicoMachineEKPub(ctx, node, cluster)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, tpmEKPubResult{hash: tpmHash, source: tpmSourceNicoCR}, *result)
}

func TestUnit_ReconcileNicoMachineEKPub_RequeuesWhenNotReady(t *testing.T) {
	ctx := context.Background()
	bunDB := testutil.SetupDB(ctx, t,
		(*db.Site)(nil),
		(*db.Cluster)(nil),
		(*db.Node)(nil),
	)
	database := db.NewDB(bunDB)

	siteID := uuid.MustParse("42000000-0000-0000-0000-000000000001")
	clusterID := uuid.MustParse("42000000-0000-0000-0000-000000000002")
	vpcID := uuid.MustParse("42000000-0000-0000-0000-000000000003")
	nodeID := uuid.MustParse("42000000-0000-0000-0000-000000000004")

	site := db.Site{ID: siteID, VIPRanges: []string{}}
	_, err := database.RegisterSite(ctx, &site)
	require.NoError(t, err)

	cluster := db.Cluster{
		ID:                clusterID,
		SiteID:            site.ID,
		Name:              "capi-cluster",
		OrgID:             "test-org",
		VpcID:             vpcID,
		KubernetesVersion: "1.31.0",
	}
	_, err = database.CreateCluster(ctx, &cluster)
	require.NoError(t, err)

	node := db.Node{
		ID:        nodeID,
		ClusterID: cluster.ID,
		Name:      "worker-0",
	}
	_, err = database.CreateNode(ctx, &node)
	require.NoError(t, err)

	nicoMachine := &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: constants.NamespaceNKESystem,
		},
		Spec: nicov1.NicoMachineSpec{
			ProviderID: "nico://instance-1",
		},
		Status: nicov1.NicoMachineStatus{
			TpmEkPubHash: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f60001",
			Conditions: []metav1.Condition{{
				Type:   clusterv1.ReadyCondition,
				Status: metav1.ConditionFalse,
				Reason: nicov1.InstanceNotReadyReason,
			}},
		},
	}

	k8sClient := newControllerTestClient(t, nicoMachine)
	controller := NewController(
		config.Config{EnableForgeCapi: true},
		slog.New(slog.NewTextHandler(os.Stdout, nil)),
		dbwrapper.New(database, slog.Default()),
		nil,
		k8sClient,
	)

	result, err := controller.reconcileNicoMachineEKPub(ctx, node, cluster)
	require.ErrorIs(t, err, errTPMDataNotReady)
	assert.Nil(t, result)

	updated, err := database.GetNode(ctx, node.ID)
	require.NoError(t, err)
	assert.Empty(t, updated.TpmEKPubHash)
}

func TestUnit_ControllerReconcile(t *testing.T) {
	ctx := context.Background()
	tpmHash := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f60001"

	t.Run("skips missing node", func(t *testing.T) {
		nodeID := uuid.New()
		mockDB := &testutil.MockDB{}
		mockDB.On("GetNode", mock.Anything, nodeID).Return(db.Node{}, db.ErrNotFound)
		controller := NewController(
			config.Config{},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))

		require.NoError(t, err)
		assert.Zero(t, result)
		mockDB.AssertExpectations(t)
	})

	t.Run("skips already hashed node", func(t *testing.T) {
		node := db.Node{ID: uuid.New(), TpmEKPubHash: tpmHash}
		mockDB := &testutil.MockDB{}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		controller := NewController(
			config.Config{},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Zero(t, result)
		mockDB.AssertExpectations(t)
	})

	t.Run("skips deleting node", func(t *testing.T) {
		node := db.Node{
			ID: uuid.New(),
			Conditions: db.NodeConditions{
				Deleting: db.NewCondition(true, "deleting"),
			},
		}
		mockDB := &testutil.MockDB{}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		controller := NewController(
			config.Config{},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Zero(t, result)
		mockDB.AssertExpectations(t)
	})

	t.Run("persists hash from NicoMachine", func(t *testing.T) {
		cluster, node, nodeSet, nodePool := nodeTPMCAPIContext(t, db.SpireAttestationMethodTPM, testNicoMachineTemplateManifest())
		nicoMachine := readyTestNicoMachine(node.Name, cluster.Name, tpmHash)
		mockTx := &testutil.MockTx{}
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
			return updated.ID == node.ID && updated.TpmEKPubHash == tpmHash
		})).Return(db.Node{}, nil)

		mockDB := &testutil.MockDB{MockTx: mockTx}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
		mockDB.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil)
		mockDB.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		controller := NewController(
			config.Config{EnableForgeCapi: true},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t, nicoMachine),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Zero(t, result)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("requeues missing NicoMachine", func(t *testing.T) {
		cluster, node, nodeSet, nodePool := nodeTPMCAPIContext(t, db.SpireAttestationMethodTPM, testNicoMachineTemplateManifest())
		mockDB := &testutil.MockDB{}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
		mockDB.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil)
		mockDB.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil)

		controller := NewController(
			config.Config{EnableForgeCapi: true},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Equal(t, nodeTPMEKPubRetryAfter, result.RequeueAfter)
		mockDB.AssertExpectations(t)
	})

	t.Run("errors on CAPI node with missing CAPI provider payload", func(t *testing.T) {
		cluster, node, nodeSet, nodePool := nodeTPMCAPIContext(t, db.SpireAttestationMethodTPM, testNicoMachineTemplateManifest())
		nodeSet.Capi = nil
		mockDB := &testutil.MockDB{}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
		mockDB.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil)
		mockDB.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil)

		controller := NewController(
			config.Config{EnableForgeCapi: true},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.ErrorIs(t, err, db.ErrMissingProviderPayload)
		assert.Zero(t, result)
		mockDB.AssertExpectations(t)
	})

	t.Run("skips TPM CAPI node for non-Nico infra provider", func(t *testing.T) {
		cluster, node, nodeSet, nodePool := nodeTPMCAPIContext(t, db.SpireAttestationMethodTPM, []byte(`{"apiVersion":"infra.example.com/v1","kind":"ExampleMachineTemplate"}`))
		nicoMachine := readyTestNicoMachine(node.Name, cluster.Name, tpmHash)
		mockDB := &testutil.MockDB{}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
		mockDB.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil)
		mockDB.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil)

		controller := NewController(
			config.Config{},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t, nicoMachine),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Zero(t, result)
		mockDB.AssertExpectations(t)
	})

	t.Run("skips ineligible CAPI join-token node", func(t *testing.T) {
		cluster, node, nodeSet, nodePool := nodeTPMCAPIContext(t, db.SpireAttestationMethodJoinToken, []byte(`{"apiVersion":"infra.example.com/v1","kind":"ExampleMachineTemplate"}`))
		mockDB := &testutil.MockDB{}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
		mockDB.On("GetNodeSet", mock.Anything, nodeSet.ID).Return(nodeSet, nil)
		mockDB.On("GetNodePool", mock.Anything, nodePool.ID).Return(nodePool, nil)

		controller := NewController(
			config.Config{},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Zero(t, result)
		mockDB.AssertExpectations(t)
	})

	t.Run("persists hash from NicoMachine for template-less Forge CAPI Nico cluster", func(t *testing.T) {
		clusterID := uuid.New()
		node := db.Node{
			ID:        uuid.New(),
			ClusterID: clusterID,
			Name:      "worker-forge-capi",
			Nico:      &db.NicoNodeProvider{},
		}
		cluster := db.Cluster{
			ID:           clusterID,
			Name:         "forge-capi",
			ProviderType: db.ProviderTypeNico,
			Nico: &db.NicoClusterProvider{
				OrgID:  "org-1",
				SiteID: uuid.New(),
			},
		}
		nicoMachine := readyTestNicoMachine(node.Name, cluster.Name, tpmHash)

		mockTx := &testutil.MockTx{}
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
			return updated.ID == node.ID && updated.TpmEKPubHash == tpmHash
		})).Return(db.Node{}, nil)
		mockDB := &testutil.MockDB{MockTx: mockTx}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		controller := NewController(
			config.Config{EnableForgeCapi: true},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t, nicoMachine),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Zero(t, result)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("requeues missing NicoMachine for template-less Forge CAPI Nico cluster", func(t *testing.T) {
		clusterID := uuid.New()
		node := db.Node{
			ID:        uuid.New(),
			ClusterID: clusterID,
			Name:      "worker-forge-capi",
			Nico:      &db.NicoNodeProvider{},
		}
		cluster := db.Cluster{
			ID:           clusterID,
			Name:         "forge-capi",
			ProviderType: db.ProviderTypeNico,
			Nico: &db.NicoClusterProvider{
				OrgID:  "org-1",
				SiteID: uuid.New(),
			},
		}

		mockDB := &testutil.MockDB{}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)

		controller := NewController(
			config.Config{EnableForgeCapi: true},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Equal(t, nodeTPMEKPubRetryAfter, result.RequeueAfter)
		mockDB.AssertExpectations(t)
	})

	t.Run("requeues Nico node missing Forge instance when Forge CAPI is disabled", func(t *testing.T) {
		clusterID := uuid.New()
		node := db.Node{
			ID:        uuid.New(),
			ClusterID: clusterID,
			Name:      "worker-forge",
			Nico:      &db.NicoNodeProvider{},
		}
		cluster := db.Cluster{
			ID:           clusterID,
			ProviderType: db.ProviderTypeNico,
			Nico: &db.NicoClusterProvider{
				OrgID:  "org-1",
				SiteID: uuid.New(),
			},
		}

		mockDB := &testutil.MockDB{}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)

		controller := NewController(
			config.Config{},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Equal(t, nodeTPMEKPubRetryAfter, result.RequeueAfter)
		mockDB.AssertExpectations(t)
	})

	t.Run("persists hash from Forge certificate for non-CAPI cluster when Forge CAPI is enabled", func(t *testing.T) {
		ready := forgeapi.INSTANCESTATUS_READY
		cert := testEKCertBundle(t)
		hash, err := tpm.ComputePublicKeyHash(cert)
		require.NoError(t, err)

		clusterID := uuid.New()
		node := db.Node{
			ID:        uuid.New(),
			ClusterID: clusterID,
			Name:      "worker-forge",
			Nico: &db.NicoNodeProvider{
				ForgeInstanceID: "instance-1",
			},
		}
		cluster := db.Cluster{
			ID:           clusterID,
			ProviderType: db.ProviderTypeNico,
			Nico: &db.NicoClusterProvider{
				OrgID:  "org-1",
				SiteID: uuid.New(),
			},
		}
		nicoMachine := readyTestNicoMachine(node.Name, "forge-cluster", tpmHash)

		mockTx := &testutil.MockTx{}
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
			return updated.ID == node.ID && updated.TpmEKPubHash == hash
		})).Return(db.Node{}, nil)

		mockDB := &testutil.MockDB{MockTx: mockTx}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		controller := NewController(
			config.Config{EnableForgeCapi: true},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t, nicoMachine),
		)
		controller.forgeProvider = &recordingNodeTPMForgeProvider{
			instance: &forgeapi.Instance{
				Status:           &ready,
				TpmEkCertificate: *forgeapi.NewNullableString(forgeapi.PtrString(cert)),
			},
		}

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Zero(t, result)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("skips final update when node already has hash after lock", func(t *testing.T) {
		clusterID := uuid.New()
		node := db.Node{
			ID:        uuid.New(),
			ClusterID: clusterID,
			Name:      "worker-forge-capi",
			Nico:      &db.NicoNodeProvider{},
		}
		lockedNode := node
		lockedNode.TpmEKPubHash = tpmHash
		cluster := db.Cluster{
			ID:           clusterID,
			Name:         "forge-capi",
			ProviderType: db.ProviderTypeNico,
			Nico: &db.NicoClusterProvider{
				OrgID:  "org-1",
				SiteID: uuid.New(),
			},
		}
		nicoMachine := readyTestNicoMachine(node.Name, cluster.Name, tpmHash)

		mockTx := &testutil.MockTx{}
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(lockedNode, nil)
		mockDB := &testutil.MockDB{MockTx: mockTx}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		controller := NewController(
			config.Config{EnableForgeCapi: true},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t, nicoMachine),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Zero(t, result)
		mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("skips final update when node is deleting after lock", func(t *testing.T) {
		clusterID := uuid.New()
		node := db.Node{
			ID:        uuid.New(),
			ClusterID: clusterID,
			Name:      "worker-forge-capi",
			Nico:      &db.NicoNodeProvider{},
		}
		lockedNode := node
		lockedNode.Conditions.Deleting = db.NewCondition(true, "deleting")
		cluster := db.Cluster{
			ID:           clusterID,
			Name:         "forge-capi",
			ProviderType: db.ProviderTypeNico,
			Nico: &db.NicoClusterProvider{
				OrgID:  "org-1",
				SiteID: uuid.New(),
			},
		}
		nicoMachine := readyTestNicoMachine(node.Name, cluster.Name, tpmHash)

		mockTx := &testutil.MockTx{}
		mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(lockedNode, nil)
		mockDB := &testutil.MockDB{MockTx: mockTx}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
		mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

		controller := NewController(
			config.Config{EnableForgeCapi: true},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t, nicoMachine),
		)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Zero(t, result)
		mockTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
		mockDB.AssertExpectations(t)
		mockTx.AssertExpectations(t)
	})

	t.Run("requeues Forge pending instance", func(t *testing.T) {
		pending := forgeapi.INSTANCESTATUS_PENDING
		clusterID := uuid.New()
		node := db.Node{
			ID:        uuid.New(),
			ClusterID: clusterID,
			Name:      "worker-pending",
			Nico: &db.NicoNodeProvider{
				ForgeInstanceID: "instance-1",
			},
		}
		cluster := db.Cluster{
			ID:           clusterID,
			ProviderType: db.ProviderTypeNico,
			Nico: &db.NicoClusterProvider{
				OrgID:  "org-1",
				SiteID: uuid.New(),
			},
		}
		mockDB := &testutil.MockDB{}
		mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
		mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)

		controller := NewController(
			config.Config{},
			slog.Default(),
			dbwrapper.New(mockDB, slog.Default()),
			nil,
			newControllerTestClient(t),
		)
		controller.forgeProvider = &recordingNodeTPMForgeProvider{
			instance: &forgeapi.Instance{Status: &pending},
		}

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))

		require.NoError(t, err)
		assert.Equal(t, nodeTPMEKPubRetryAfter, result.RequeueAfter)
		mockDB.AssertExpectations(t)
	})
}

func nodeTPMCAPIContext(t *testing.T, method db.SpireAttestationMethod, infraMachineTemplate []byte) (db.Cluster, db.Node, db.NodeSet, db.NodePool) {
	t.Helper()

	clusterID := uuid.New()
	nodePoolID := uuid.New()
	nodeSetID := uuid.New()
	cluster := db.Cluster{
		ID:           clusterID,
		Name:         "capi-cluster",
		ProviderType: db.ProviderTypeCAPI,
		Capi:         &db.CAPIClusterProvider{},
	}
	nodePool := db.NodePool{
		ID:        nodePoolID,
		ClusterID: clusterID,
		Name:      "workers",
	}
	nodePool.SetCapi(db.CAPINodePoolProvider{
		InfraMachineTemplate:   append([]byte(nil), infraMachineTemplate...),
		SpireAttestationMethod: method,
	})
	nodeSet := db.NodeSet{
		ID:         nodeSetID,
		NodePoolID: nodePoolID,
	}
	nodeSet.SetCapi(db.CAPINodePoolProvider{
		InfraMachineTemplate:   append([]byte(nil), infraMachineTemplate...),
		SpireAttestationMethod: method,
	})
	node := db.Node{
		ID:         uuid.New(),
		ClusterID:  clusterID,
		NodePoolID: nodePoolID,
		NodeSetID:  nodeSetID,
		Name:       "worker-0",
	}
	return cluster, node, nodeSet, nodePool
}

func testNicoMachineTemplateManifest() []byte {
	return []byte(`{"apiVersion":"` + nicov1.GroupVersion.String() + `","kind":"NicoMachineTemplate"}`)
}

func readyTestNicoMachine(nodeName, _, tpmHash string) *nicov1.NicoMachine {
	return &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeName,
			Namespace: constants.NamespaceNKESystem,
		},
		Spec: nicov1.NicoMachineSpec{
			ProviderID: "nico://instance-1",
		},
		Status: nicov1.NicoMachineStatus{
			Ready:        true,
			TpmEkPubHash: tpmHash,
		},
	}
}

func testEKCertBundle(t *testing.T) string {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "test-ek-cert",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(der)
}

func newControllerTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, nicov1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objs {
		builder = builder.WithObjects(obj)
	}

	return builder.Build()
}
