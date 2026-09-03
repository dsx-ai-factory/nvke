// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

// The shared namespace holds every cluster's CAPI objects plus the tenant-access
// Secrets and trust bundles for the whole site, so a teardown that deleted it
// would take the site down with the cluster.
func TestDeleteCAPIClusterResources_LeavesSharedNamespaceIntact(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	vpcID := uuid.New()
	cluster := db.Cluster{
		ID:           uuid.New(),
		Name:         "alpha",
		OrgID:        "org-1",
		SiteID:       siteID,
		VpcID:        vpcID,
		ProviderType: db.ProviderTypeNico,
		Nico:         &db.NicoClusterProvider{OrgID: "org-1", SiteID: siteID, VpcID: vpcID},
	}
	ns := constants.NamespaceNKESystem

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
			&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: ns}},
			&nicov1.NicoCluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: ns}},
		).
		Build()

	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)

	a := &Activities{db: mockDB, k8sClient: k8sClient}
	require.NoError(t, a.DeleteCAPIClusterResources(ctx, DeleteCAPIClusterResourcesInput{ClusterID: cluster.ID}))

	err := k8sClient.Get(ctx, client.ObjectKey{Name: cluster.Name, Namespace: ns}, &clusterv1.Cluster{})
	assert.True(t, apierrors.IsNotFound(err), "CAPI cluster should be deleted")
	err = k8sClient.Get(ctx, client.ObjectKey{Name: cluster.Name, Namespace: ns}, &nicov1.NicoCluster{})
	assert.True(t, apierrors.IsNotFound(err), "infra cluster should be deleted")
	assert.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: ns}, &corev1.Namespace{}), "shared namespace must survive cluster teardown")
}

func TestDeleteCAPIClusterResources_WaitsForRemainingMachines(t *testing.T) {
	ctx := context.Background()
	siteID := uuid.New()
	vpcID := uuid.New()
	cluster := db.Cluster{
		ID:           uuid.New(),
		Name:         "alpha",
		OrgID:        "org-1",
		SiteID:       siteID,
		VpcID:        vpcID,
		ProviderType: db.ProviderTypeNico,
		Nico:         &db.NicoClusterProvider{OrgID: "org-1", SiteID: siteID, VpcID: vpcID},
	}
	ns := constants.NamespaceNKESystem

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: ns}},
			&nicov1.NicoCluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: ns}},
			&clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "worker-0",
					Namespace:  ns,
					Labels:     map[string]string{clusterv1.ClusterNameLabel: cluster.Name},
					Finalizers: []string{"test.finalizer"},
				},
			},
		).
		Build()

	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)

	a := &Activities{db: mockDB, k8sClient: k8sClient}

	testActivity := func(ctx context.Context, input DeleteCAPIClusterResourcesInput) error {
		ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		return a.DeleteCAPIClusterResources(ctx, input)
	}

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(testActivity)

	_, err := env.ExecuteActivity(testActivity, DeleteCAPIClusterResourcesInput{ClusterID: cluster.ID})
	require.Error(t, err)

	assert.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: cluster.Name, Namespace: ns}, &nicov1.NicoCluster{}),
		"infra cluster must not be deleted while a Machine still exists")
	assert.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: cluster.Name, Namespace: ns}, &clusterv1.Cluster{}),
		"CAPI cluster must not be deleted while a Machine still exists")
}
