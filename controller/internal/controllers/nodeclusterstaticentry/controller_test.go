// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeclusterstaticentry

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestUnit_Controller_ReconcileAppliesTPMEntry(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	clusterID := uuid.New()
	nodeID := uuid.New()
	tpmHash := "hash-from-node"
	node := testStaticEntryNode(nodeID, clusterID, "worker-0", tpmHash)
	cluster := testStaticEntryCluster(siteID, clusterID, db.ProviderTypeNico)
	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()
	k8sClient := newControllerTestClient(t)
	controller := newTestController(mockDB, k8sClient)

	_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
	require.NoError(t, err)

	got := &spirev1alpha1.ClusterStaticEntry{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: clusterStaticEntryName(nodeID)}, got))
	assert.Equal(t, constants.LabelValueNkeManagedBy, got.Labels[constants.LabelKeyNkeManagedBy])
	assert.NotContains(t, got.Labels, constants.LabelKeySiteID)
	assert.Equal(t, "spire", got.Spec.ClassName)
	assert.Equal(t, "spiffe://example.org"+constants.TPMSpireAgentPath+tpmHash, got.Spec.ParentID)
	assert.Equal(t, []string{unixRootSelector}, got.Spec.Selectors)
	assert.Equal(t, "spiffe://example.org/host/"+nodeID.String(), got.Spec.SPIFFEID)
	mockDB.AssertExpectations(t)
}

func TestUnit_Controller_ReconcileUpdatesTPMEntry(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	clusterID := uuid.New()
	nodeID := uuid.New()
	node := testStaticEntryNode(nodeID, clusterID, "worker-0", "new-hash")
	cluster := testStaticEntryCluster(siteID, clusterID, db.ProviderTypeNico)
	existing := testOwnedClusterStaticEntry(nodeID, "old-hash")
	existing.Annotations = map[string]string{"custom": "kept"}
	existing.Labels["custom"] = "kept"
	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil).Twice()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Twice()
	k8sClient := newControllerTestClient(t, existing)
	controller := newTestController(mockDB, k8sClient)

	_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
	require.NoError(t, err)
	_, err = controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
	require.NoError(t, err)

	got := &spirev1alpha1.ClusterStaticEntry{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: clusterStaticEntryName(nodeID)}, got))
	assert.Equal(t, "spiffe://example.org"+constants.TPMSpireAgentPath+"new-hash", got.Spec.ParentID)
	assert.Equal(t, "kept", got.Annotations["custom"])
	assert.Equal(t, "kept", got.Labels["custom"])
	mockDB.AssertExpectations(t)
}

func TestUnit_Controller_ReconcileDeletesOwnedEntryWhenNodeHasNoTPMHash(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.New()
	nodeID := uuid.New()
	node := testStaticEntryNode(nodeID, clusterID, "worker-0", "")
	entry := testOwnedClusterStaticEntry(nodeID, "old-hash")
	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
	k8sClient := newControllerTestClient(t, entry)
	controller := newTestController(mockDB, k8sClient)

	_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
	require.NoError(t, err)

	err = k8sClient.Get(ctx, types.NamespacedName{Name: clusterStaticEntryName(nodeID)}, &spirev1alpha1.ClusterStaticEntry{})
	require.True(t, apierrors.IsNotFound(err))
	mockDB.AssertExpectations(t)
}

func TestUnit_Controller_ReconcileDeletesOwnedEntryWhenNodeMissing(t *testing.T) {
	ctx := context.Background()
	nodeID := uuid.New()
	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(db.Node{}, db.ErrNotFound).Once()
	k8sClient := newControllerTestClient(t, testOwnedClusterStaticEntry(nodeID, "old-hash"))
	controller := newTestController(mockDB, k8sClient)

	_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
	require.NoError(t, err)

	err = k8sClient.Get(ctx, types.NamespacedName{Name: clusterStaticEntryName(nodeID)}, &spirev1alpha1.ClusterStaticEntry{})
	require.True(t, apierrors.IsNotFound(err))
	mockDB.AssertExpectations(t)
}

func TestUnit_Controller_ReconcilePreservesUnownedEntry(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.New()
	nodeID := uuid.New()
	node := testStaticEntryNode(nodeID, clusterID, "worker-0", "")
	entry := testOwnedClusterStaticEntry(nodeID, "old-hash")
	entry.Labels[constants.LabelKeyNkeManagedBy] = constants.FieldManagerMachineSpireEntry
	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
	k8sClient := newControllerTestClient(t, entry)
	controller := newTestController(mockDB, k8sClient)

	_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
	require.NoError(t, err)

	got := &spirev1alpha1.ClusterStaticEntry{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: clusterStaticEntryName(nodeID)}, got))
	assert.Equal(t, constants.FieldManagerMachineSpireEntry, got.Labels[constants.LabelKeyNkeManagedBy])
	mockDB.AssertExpectations(t)
}

func TestUnit_Controller_ReconcilePreservesJoinTokenCAPIMachineEntry(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	clusterID := uuid.New()
	nodeID := uuid.New()
	node := testStaticEntryNode(nodeID, clusterID, "join-token-capi-cp-0", "hash-from-nicomachine")
	cluster := testStaticEntryCluster(siteID, clusterID, db.ProviderTypeCAPI)
	joinTokenParentID := "spiffe://example.org" + constants.JoinTokenAlternateSpireAgentPath + nodeID.String()
	entry := testOwnedClusterStaticEntry(nodeID, "old-hash")
	entry.Labels[constants.LabelKeyNkeManagedBy] = constants.FieldManagerMachineSpireEntry
	entry.Spec.ParentID = joinTokenParentID
	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()
	k8sClient := newControllerTestClient(t,
		testCAPIMachine(node.Name, constants.JoinTokenAlternateSpireAgentPath+nodeID.String()),
		entry,
	)
	controller := newTestController(mockDB, k8sClient)

	_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
	require.NoError(t, err)

	got := &spirev1alpha1.ClusterStaticEntry{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: clusterStaticEntryName(nodeID)}, got))
	assert.Equal(t, joinTokenParentID, got.Spec.ParentID)
	assert.Equal(t, constants.FieldManagerMachineSpireEntry, got.Labels[constants.LabelKeyNkeManagedBy])
	mockDB.AssertExpectations(t)
}

func TestUnit_Controller_ReconcilePreservesTPMEntryWhenCAPIMachineMissing(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	clusterID := uuid.New()
	nodeID := uuid.New()
	node := testStaticEntryNode(nodeID, clusterID, "missing-machine-capi-cp-0", "hash-from-nicomachine")
	cluster := testStaticEntryCluster(siteID, clusterID, db.ProviderTypeCAPI)
	existing := testOwnedClusterStaticEntry(nodeID, "existing-hash")
	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil).Once()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()
	k8sClient := newControllerTestClient(t, existing)
	controller := newTestController(mockDB, k8sClient)

	_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
	require.ErrorContains(t, err, "failed to find CAPI machine")

	got := &spirev1alpha1.ClusterStaticEntry{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: clusterStaticEntryName(nodeID)}, got))
	assert.Equal(t, "spiffe://example.org"+constants.TPMSpireAgentPath+"existing-hash", got.Spec.ParentID)
	mockDB.AssertExpectations(t)
}

func TestUnit_Controller_MapsOwnedTPMClusterStaticEntry(t *testing.T) {
	nodeID := uuid.New()
	controller := newTestController(&testutil.MockDB{}, newControllerTestClient(t))

	keys := controller.keysForClusterStaticEntry(context.Background(), testOwnedClusterStaticEntry(nodeID, "hash"))

	require.Equal(t, []dbreconcile.Key{dbreconcile.NodeKey(nodeID)}, keys)
}

func TestUnit_Controller_DoesNotMapUnownedClusterStaticEntry(t *testing.T) {
	nodeID := uuid.New()
	controller := newTestController(&testutil.MockDB{}, newControllerTestClient(t))
	parameters := map[string]struct {
		entry *spirev1alpha1.ClusterStaticEntry
	}{
		"wrong name": {
			entry: func() *spirev1alpha1.ClusterStaticEntry {
				entry := testOwnedClusterStaticEntry(nodeID, "hash")
				entry.Name = "other-" + nodeID.String()
				return entry
			}(),
		},
		"wrong owner": {
			entry: func() *spirev1alpha1.ClusterStaticEntry {
				entry := testOwnedClusterStaticEntry(nodeID, "hash")
				entry.Labels[constants.LabelKeyNkeManagedBy] = constants.FieldManagerMachineSpireEntry
				return entry
			}(),
		},
		"non TPM parent": {
			entry: func() *spirev1alpha1.ClusterStaticEntry {
				entry := testOwnedClusterStaticEntry(nodeID, "hash")
				entry.Spec.ParentID = "spiffe://example.org" + constants.JoinTokenAlternateSpireAgentPath + nodeID.String()
				return entry
			}(),
		},
		"wrong spiffe ID": {
			entry: func() *spirev1alpha1.ClusterStaticEntry {
				entry := testOwnedClusterStaticEntry(nodeID, "hash")
				entry.Spec.SPIFFEID = "spiffe://example.org/host/" + uuid.New().String()
				return entry
			}(),
		},
		"wrong class": {
			entry: func() *spirev1alpha1.ClusterStaticEntry {
				entry := testOwnedClusterStaticEntry(nodeID, "hash")
				entry.Spec.ClassName = "other"
				return entry
			}(),
		},
	}

	for name, params := range parameters {
		t.Run(name, func(t *testing.T) {
			keys := controller.keysForClusterStaticEntry(context.Background(), params.entry)
			require.Empty(t, keys)
		})
	}
}

func newTestController(mockDB *testutil.MockDB, k8sClient client.Client) *Controller {
	return &Controller{
		cfg:         config.Config{SpireClassName: "spire"},
		db:          dbwrapper.New(mockDB, slog.New(slog.DiscardHandler)),
		k8sClient:   k8sClient,
		logger:      slog.New(slog.DiscardHandler),
		trustDomain: "example.org",
	}
}

func testStaticEntryNode(nodeID, clusterID uuid.UUID, name, tpmHash string) db.Node {
	return db.Node{
		ID:           nodeID,
		ClusterID:    clusterID,
		Name:         name,
		TpmEKPubHash: tpmHash,
	}
}

func testStaticEntryCluster(siteID, clusterID uuid.UUID, providerType db.ProviderType) db.Cluster {
	return db.Cluster{
		ID:           clusterID,
		SiteID:       siteID,
		Name:         "cluster-test",
		ProviderType: providerType,
	}
}

func testCAPIMachine(name, spireAgentIDPath string) *clusterv1.Machine {
	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: constants.NamespaceNKESystem,
			Annotations: map[string]string{
				constants.MachineAnnotationKeySpireAgentSpiffeIDPath: spireAgentIDPath,
			},
		},
	}
}

func testOwnedClusterStaticEntry(nodeID uuid.UUID, tpmHash string) *spirev1alpha1.ClusterStaticEntry {
	return &spirev1alpha1.ClusterStaticEntry{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spirev1alpha1.SchemeGroupVersion.String(),
			Kind:       clusterStaticEntryKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterStaticEntryName(nodeID),
			Labels: map[string]string{
				constants.LabelKeyNkeManagedBy: constants.LabelValueNkeManagedBy,
			},
		},
		Spec: spirev1alpha1.ClusterStaticEntrySpec{
			ClassName: "spire",
			ParentID:  "spiffe://example.org" + constants.TPMSpireAgentPath + tpmHash,
			Selectors: []string{unixRootSelector},
			SPIFFEID:  "spiffe://example.org/host/" + nodeID.String(),
		},
	}
}

func newControllerTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}
