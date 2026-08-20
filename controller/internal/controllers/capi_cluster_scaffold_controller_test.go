// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

const testClusterName = "test-cluster"

func TestCAPIClusterScaffoldController_Reconcile(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	clusterID := uuid.New()

	activeCluster := scaffoldCluster(t, clusterID, siteID, testClusterName)
	deletingCluster := scaffoldCluster(t, uuid.New(), siteID, "deleting-cluster")
	deletingCluster.Conditions.Deleting = db.Condition{Status: db.ConditionStatusTrue}
	emptyCluster := db.Cluster{
		ID:   uuid.New(),
		Name: "empty-cluster",
	}
	emptyCluster.SetCAPIProvider(db.CAPIClusterProvider{InfraCluster: json.RawMessage(`{}`)})

	t.Run("applies CAPI objects in nke-system and does not write kubeconfig", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.On("ListClusters", mock.Anything, mock.MatchedBy(func(filters db.ClusterFilters) bool {
			return filters.SiteID != nil && *filters.SiteID == siteID
		})).Return([]db.Cluster{activeCluster}, nil)

		k8sClient := fake.NewClientBuilder().WithScheme(controllerTestScheme(t)).Build()
		controller := controllers.NewCAPIClusterScaffoldController(
			config.Config{},
			slog.Default(),
			mockDB,
			k8sClient,
			nil,
		)
		require.NoError(t, controller.Reconcile(ctx, db.Site{ID: siteID}))

		clusterNS := "cluster-" + activeCluster.Name
		clusterObj := &clusterv1.Cluster{}
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
			Name:      activeCluster.Name,
			Namespace: constants.NamespaceNKESystem,
		}, clusterObj))

		err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      activeCluster.Name + "-kubeconfig",
			Namespace: clusterNS,
		}, &clusterv1.Cluster{})
		assert.True(t, apierrors.IsNotFound(err), "scaffold must not write into the legacy per-cluster namespace")
		err = k8sClient.Get(ctx, types.NamespacedName{
			Name:      activeCluster.Name + "-kubeconfig",
			Namespace: constants.NamespaceNKESystem,
		}, &corev1.Secret{})
		assert.True(t, apierrors.IsNotFound(err), "scaffold must not write the kubeconfig Secret in nke-system")
		mockDB.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
		mockDB.AssertExpectations(t)
	})

	t.Run("skips deleting and empty clusters", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.On("ListClusters", mock.Anything, mock.Anything).Return([]db.Cluster{deletingCluster, emptyCluster}, nil)
		k8sClient := fake.NewClientBuilder().WithScheme(controllerTestScheme(t)).Build()
		controller := controllers.NewCAPIClusterScaffoldController(
			config.Config{},
			slog.Default(),
			mockDB,
			k8sClient,
			nil,
		)
		require.NoError(t, controller.Reconcile(ctx, db.Site{ID: siteID}))
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      deletingCluster.Name,
			Namespace: constants.NamespaceNKESystem,
		}, &clusterv1.Cluster{})
		assert.True(t, apierrors.IsNotFound(err))
		mockDB.AssertExpectations(t)
	})
}

func scaffoldCluster(t *testing.T, clusterID, siteID uuid.UUID, name string) db.Cluster {
	t.Helper()

	cluster := db.Cluster{
		ID:   clusterID,
		Name: name,
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{OrgID: "org-1", SiteID: siteID, VpcID: uuid.New()})
	return cluster
}

func controllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))
	return scheme
}
