// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestNodeClusterStaticEntryController_SkipsTPMEntryForJoinTokenCAPIMachine(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	clusterID := uuid.New()
	nodeID := uuid.New()
	cluster := db.Cluster{
		ID:           clusterID,
		Name:         "join-token-capi",
		ProviderType: db.ProviderTypeCAPI,
	}
	node := db.Node{
		ID:           nodeID,
		ClusterID:    clusterID,
		Name:         "join-token-capi-cp-0",
		TpmEKPubHash: "hash-from-nicomachine",
	}
	entryName := "host-" + nodeID.String()
	joinTokenParentID := "spiffe://example.org" + constants.JoinTokenAlternateSpireAgentPath + nodeID.String()

	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.SiteID != nil && *filters.SiteID == siteID
	})).Return([]db.Node{node}, nil).Once()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()

	k8sClient := newNodeClusterStaticEntryControllerTestClient(t,
		&clusterv1.Machine{
			TypeMeta: metav1.TypeMeta{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Machine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      node.Name,
				Namespace: constants.NamespaceNKESystem,
				Annotations: map[string]string{
					constants.MachineAnnotationKeySpireAgentSpiffeIDPath: constants.JoinTokenAlternateSpireAgentPath + nodeID.String(),
				},
			},
		},
		&spirev1alpha1.ClusterStaticEntry{
			TypeMeta: metav1.TypeMeta{
				APIVersion: spirev1alpha1.SchemeGroupVersion.String(),
				Kind:       "ClusterStaticEntry",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: entryName,
				Labels: map[string]string{
					constants.LabelKeyNkeManagedBy: constants.FieldManagerMachineSpireEntry,
				},
			},
			Spec: spirev1alpha1.ClusterStaticEntrySpec{
				ClassName: "spire",
				ParentID:  joinTokenParentID,
				Selectors: []string{"unix:uid:0"},
				SPIFFEID:  "spiffe://example.org/host/" + nodeID.String(),
			},
		},
	)

	controller := &NodeClusterStaticEntryController{
		cfg:         config.Config{SpireClassName: "spire"},
		db:          mockDB,
		k8sClient:   k8sClient,
		logger:      slog.New(slog.DiscardHandler),
		trustDomain: "example.org",
	}

	require.NoError(t, controller.Reconcile(ctx, db.Site{ID: siteID}))

	got := &spirev1alpha1.ClusterStaticEntry{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: entryName}, got))
	require.Equal(t, joinTokenParentID, got.Spec.ParentID)
	mockDB.AssertExpectations(t)
}

func TestNodeClusterStaticEntryController_DeletesStaleTPMEntryForJoinTokenCAPIMachine(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	clusterID := uuid.New()
	nodeID := uuid.New()
	cluster := db.Cluster{
		ID:           clusterID,
		Name:         "join-token-capi",
		ProviderType: db.ProviderTypeCAPI,
	}
	node := db.Node{
		ID:           nodeID,
		ClusterID:    clusterID,
		Name:         "join-token-capi-cp-0",
		TpmEKPubHash: "hash-from-nicomachine",
	}
	entryName := "host-" + nodeID.String()

	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.SiteID != nil && *filters.SiteID == siteID
	})).Return([]db.Node{node}, nil).Once()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()

	k8sClient := newNodeClusterStaticEntryControllerTestClient(t,
		&clusterv1.Machine{
			TypeMeta: metav1.TypeMeta{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Machine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      node.Name,
				Namespace: constants.NamespaceNKESystem,
				Annotations: map[string]string{
					constants.MachineAnnotationKeySpireAgentSpiffeIDPath: constants.JoinTokenAlternateSpireAgentPath + nodeID.String(),
				},
			},
		},
		&spirev1alpha1.ClusterStaticEntry{
			TypeMeta: metav1.TypeMeta{
				APIVersion: spirev1alpha1.SchemeGroupVersion.String(),
				Kind:       "ClusterStaticEntry",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: entryName,
				Labels: map[string]string{
					constants.LabelKeyNkeManagedBy: constants.LabelValueNkeManagedBy,
					constants.LabelKeySiteID:       siteID.String(),
				},
			},
			Spec: spirev1alpha1.ClusterStaticEntrySpec{
				ClassName: "spire",
				ParentID:  "spiffe://example.org" + constants.TPMSpireAgentPath + node.TpmEKPubHash,
				Selectors: []string{"unix:uid:0"},
				SPIFFEID:  "spiffe://example.org/host/" + nodeID.String(),
			},
		},
	)

	controller := &NodeClusterStaticEntryController{
		cfg:         config.Config{SpireClassName: "spire"},
		db:          mockDB,
		k8sClient:   k8sClient,
		logger:      slog.New(slog.DiscardHandler),
		trustDomain: "example.org",
	}

	require.NoError(t, controller.Reconcile(ctx, db.Site{ID: siteID}))

	got := &spirev1alpha1.ClusterStaticEntry{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: entryName}, got)
	require.Error(t, err)
	mockDB.AssertExpectations(t)
}

func TestNodeClusterStaticEntryController_AppliesTPMEntryForTPMCAPIMachine(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	clusterID := uuid.New()
	nodeID := uuid.New()
	tpmHash := "hash-from-nicomachine"
	cluster := db.Cluster{
		ID:           clusterID,
		Name:         "tpm-capi",
		ProviderType: db.ProviderTypeCAPI,
	}
	node := db.Node{
		ID:           nodeID,
		ClusterID:    clusterID,
		Name:         "tpm-capi-cp-0",
		TpmEKPubHash: tpmHash,
	}

	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.SiteID != nil && *filters.SiteID == siteID
	})).Return([]db.Node{node}, nil).Once()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()

	k8sClient := newNodeClusterStaticEntryControllerTestClient(t,
		&clusterv1.Machine{
			TypeMeta: metav1.TypeMeta{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Machine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      node.Name,
				Namespace: constants.NamespaceNKESystem,
				Annotations: map[string]string{
					constants.MachineAnnotationKeySpireAgentSpiffeIDPath: constants.TPMSpireAgentPath + tpmHash,
				},
			},
		},
	)

	controller := &NodeClusterStaticEntryController{
		cfg:         config.Config{SpireClassName: "spire"},
		db:          mockDB,
		k8sClient:   k8sClient,
		logger:      slog.New(slog.DiscardHandler),
		trustDomain: "example.org",
	}

	require.NoError(t, controller.Reconcile(ctx, db.Site{ID: siteID}))

	got := &spirev1alpha1.ClusterStaticEntry{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "host-" + nodeID.String()}, got))
	require.Equal(t, "spiffe://example.org"+constants.TPMSpireAgentPath+tpmHash, got.Spec.ParentID)
	mockDB.AssertExpectations(t)
}

func TestNodeClusterStaticEntryController_PreservesTPMEntryWhenCAPIMachineMissing(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	clusterID := uuid.New()
	nodeID := uuid.New()
	cluster := db.Cluster{
		ID:           clusterID,
		Name:         "missing-machine-capi",
		ProviderType: db.ProviderTypeCAPI,
	}
	node := db.Node{
		ID:           nodeID,
		ClusterID:    clusterID,
		Name:         "missing-machine-capi-cp-0",
		TpmEKPubHash: "hash-from-nicomachine",
	}
	entryName := "host-" + nodeID.String()
	existingParentID := "spiffe://example.org" + constants.TPMSpireAgentPath + "existing-hash"

	mockDB := &testutil.MockDB{}
	mockDB.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.SiteID != nil && *filters.SiteID == siteID
	})).Return([]db.Node{node}, nil).Once()
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil).Once()

	k8sClient := newNodeClusterStaticEntryControllerTestClient(t,
		&spirev1alpha1.ClusterStaticEntry{
			TypeMeta: metav1.TypeMeta{
				APIVersion: spirev1alpha1.SchemeGroupVersion.String(),
				Kind:       "ClusterStaticEntry",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: entryName,
				Labels: map[string]string{
					constants.LabelKeyNkeManagedBy: constants.LabelValueNkeManagedBy,
					constants.LabelKeySiteID:       siteID.String(),
				},
			},
			Spec: spirev1alpha1.ClusterStaticEntrySpec{
				ClassName: "spire",
				ParentID:  existingParentID,
				Selectors: []string{"unix:uid:0"},
				SPIFFEID:  "spiffe://example.org/host/" + nodeID.String(),
			},
		},
	)

	controller := &NodeClusterStaticEntryController{
		cfg:         config.Config{SpireClassName: "spire"},
		db:          mockDB,
		k8sClient:   k8sClient,
		logger:      slog.New(slog.DiscardHandler),
		trustDomain: "example.org",
	}

	require.ErrorContains(t, controller.Reconcile(ctx, db.Site{ID: siteID}), "failed to find CAPI machine")

	got := &spirev1alpha1.ClusterStaticEntry{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: entryName}, got))
	require.Equal(t, existingParentID, got.Spec.ParentID)
	mockDB.AssertExpectations(t)
}

func newNodeClusterStaticEntryControllerTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}
