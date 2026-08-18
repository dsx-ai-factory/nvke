// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"log/slog"
	"os"
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

	forgeapi "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestReconcileForgeNodeEKPub(t *testing.T) {
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
		"skips without forge instance": {
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
			controller := &NodeTPMEKPubController{
				logger:        slog.New(slog.NewTextHandler(os.Stdout, nil)),
				forgeProvider: params.forgeProvider,
			}

			err := controller.reconcileForgeNodeEKPub(context.Background(), params.node, params.cluster)

			require.NoError(t, err)
			assert.Equal(t, params.wantOrgID, params.forgeProvider.orgID)
			assert.Equal(t, params.wantSiteID, params.forgeProvider.siteID)
			assert.Equal(t, params.wantInstanceID, params.forgeProvider.instanceID)
		})
	}
}

func TestIsForgeInstanceAvailable(t *testing.T) {
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

func TestIsNicoMachineAvailable(t *testing.T) {
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

func TestReconcileNicoNodeEKPub_PersistsHashWhenReady(t *testing.T) {
	ctx := context.Background()

	clusterID := uuid.MustParse("41000000-0000-0000-0000-000000000001")
	nodeID := uuid.MustParse("41000000-0000-0000-0000-000000000002")
	node := db.Node{
		ID:        nodeID,
		ClusterID: clusterID,
		Name:      "worker-0",
	}

	tpmHash := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f60001"
	nicoMachine := &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: "cluster-capi-cluster",
		},
		Spec: nicov1.NicoMachineSpec{
			ProviderID: "nico://instance-1",
		},
		Status: nicov1.NicoMachineStatus{
			Ready:        true,
			TpmEkPubHash: tpmHash,
		},
	}

	// Use MockDB instead of testutil.SetupDB: persistNodeTPMEKPubHash calls
	// GetNodeForUpdate (SELECT ... FOR UPDATE), which SQLite does not support.
	// Guard-path tests above can use SetupDB because they return before the write.
	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, node.ID).Return(node, nil)
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(updated *db.Node) bool {
		return updated.TpmEKPubHash == tpmHash
	})).Return(node, nil)

	mockDB := &testutil.MockDB{MockTx: mockTx}
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

	k8sClient := newNodeTPMEKPubControllerTestClient(t, nicoMachine)
	controller := NewNodeTPMEKPubController(
		config.Config{EnableForgeCapi: true},
		mockDB,
		slog.New(slog.NewTextHandler(os.Stdout, nil)),
		nil,
		k8sClient,
	)

	err := controller.reconcileNicoNodeEKPub(ctx, node, nicoMachine)
	require.NoError(t, err)
	mockTx.AssertExpectations(t)
}

func TestReconcileNicoNodeEKPub_SkipsWhenNotReady(t *testing.T) {
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
			Namespace: "cluster-capi-cluster",
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

	k8sClient := newNodeTPMEKPubControllerTestClient(t, nicoMachine)
	controller := NewNodeTPMEKPubController(
		config.Config{EnableForgeCapi: true},
		database,
		slog.New(slog.NewTextHandler(os.Stdout, nil)),
		nil,
		k8sClient,
	)

	err = controller.reconcileNicoNodeEKPub(ctx, node, nicoMachine)
	require.NoError(t, err)

	updated, err := database.GetNode(ctx, node.ID)
	require.NoError(t, err)
	assert.Empty(t, updated.TpmEKPubHash)
}

func newNodeTPMEKPubControllerTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, nicov1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objs {
		builder = builder.WithObjects(obj)
	}

	return builder.Build()
}
