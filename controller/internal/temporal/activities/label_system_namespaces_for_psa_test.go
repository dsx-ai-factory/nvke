// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestLabelSystemNamespacesForPSALabelsSystemNamespacesPrivileged(t *testing.T) {
	tests := map[string]struct {
		cniPlugin      db.CniPlugin
		otherNamespace []string
	}{
		"calico":         {cniPlugin: db.CNICalico, otherNamespace: []string{tigeraOperatorNS}},
		"ovn-kubernetes": {cniPlugin: db.CNIOvnKubernetes, otherNamespace: []string{ovnKubernetesNamespace}},
		"unrecognized":   {cniPlugin: db.CniPlugin("something-else")},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a", CniPlugin: params.cniPlugin}
			tenantClient := newPSATenantClient(t, append([]string{metav1.NamespaceSystem}, params.otherNamespace...)...)
			a := newPSAActivities(t, cluster, tenantClient, 1)

			require.NoError(t, executeLabelSystemNamespacesForPSAActivity(t, a, cluster.ID))

			requireNamespacePrivileged(t, tenantClient, metav1.NamespaceSystem)
			for _, ns := range params.otherNamespace {
				requireNamespacePrivileged(t, tenantClient, ns)
			}
		})
	}
}

func TestLabelSystemNamespacesForPSAIsSafeToRetry(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a", CniPlugin: db.CNICalico}
	tenantClient := newPSATenantClient(t, metav1.NamespaceSystem, tigeraOperatorNS)
	a := newPSAActivities(t, cluster, tenantClient, 2)

	require.NoError(t, executeLabelSystemNamespacesForPSAActivity(t, a, cluster.ID))
	require.NoError(t, executeLabelSystemNamespacesForPSAActivity(t, a, cluster.ID), "must be safe to retry")

	requireNamespacePrivileged(t, tenantClient, metav1.NamespaceSystem)
	requireNamespacePrivileged(t, tenantClient, tigeraOperatorNS)
}

func TestLabelSystemNamespacesForPSADoesNotWaitForCalicoSystem(t *testing.T) {
	// The tigera operator creates calico-system, already labeled privileged,
	// some time after the CNI install returns. Its absence must not fail the
	// activity, and the activity must not create it.
	cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a", CniPlugin: db.CNICalico}
	tenantClient := newPSATenantClient(t, metav1.NamespaceSystem, tigeraOperatorNS)
	a := newPSAActivities(t, cluster, tenantClient, 1)

	require.NoError(t, executeLabelSystemNamespacesForPSAActivity(t, a, cluster.ID))

	requireNamespacePrivileged(t, tenantClient, metav1.NamespaceSystem)
	requireNamespacePrivileged(t, tenantClient, tigeraOperatorNS)
	err := tenantClient.Get(context.Background(), types.NamespacedName{Name: "calico-system"}, &corev1.Namespace{})
	require.True(t, apierrors.IsNotFound(err), "calico-system must not be created, got %v", err)
}

func TestLabelSystemNamespacesForPSAFailsWhenLabeledNamespaceIsMissing(t *testing.T) {
	tests := map[string]struct {
		cniPlugin db.CniPlugin
		present   []string
		missing   string
	}{
		"kube-system":     {cniPlugin: db.CNICalico, present: []string{tigeraOperatorNS}, missing: metav1.NamespaceSystem},
		"tigera-operator": {cniPlugin: db.CNICalico, present: []string{metav1.NamespaceSystem}, missing: tigeraOperatorNS},
		"ovn-kubernetes":  {cniPlugin: db.CNIOvnKubernetes, present: []string{metav1.NamespaceSystem}, missing: ovnKubernetesNamespace},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a", CniPlugin: params.cniPlugin}
			tenantClient := newPSATenantClient(t, params.present...)
			a := newPSAActivities(t, cluster, tenantClient, 1)

			err := executeLabelSystemNamespacesForPSAActivity(t, a, cluster.ID)
			require.ErrorContains(t, err, "getting namespace "+params.missing)
		})
	}
}

func TestLabelSystemNamespacesForPSAReturnsTenantClientProviderError(t *testing.T) {
	cluster := db.Cluster{ID: uuid.New(), Name: "cluster-a", CniPlugin: db.CNICalico}
	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()
	t.Cleanup(func() { database.AssertExpectations(t) })
	a := &Activities{
		db:                   database,
		tenantClientProvider: &fakeTenantClientProvider{err: context.DeadlineExceeded},
	}

	err := executeLabelSystemNamespacesForPSAActivity(t, a, cluster.ID)
	require.ErrorContains(t, err, context.DeadlineExceeded.Error())
}

func newPSATenantClient(t *testing.T, namespaces ...string) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	objects := make([]client.Object, 0, len(namespaces))
	for _, ns := range namespaces {
		objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func newPSAActivities(t *testing.T, cluster db.Cluster, tenantClient client.Client, getClusterCalls int) *Activities {
	t.Helper()

	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Times(getClusterCalls)
	t.Cleanup(func() { database.AssertExpectations(t) })
	return &Activities{
		db:                   database,
		tenantClientProvider: &fakeTenantClientProvider{tenantClient: tenantClient},
	}
}

func requireNamespacePrivileged(t *testing.T, tenantClient client.Client, name string) {
	t.Helper()

	var ns corev1.Namespace
	require.NoError(t, tenantClient.Get(context.Background(), types.NamespacedName{Name: name}, &ns))
	require.Equal(t, psaLevelPrivileged, ns.Labels[psaEnforceLabelKey])
}

func executeLabelSystemNamespacesForPSAActivity(t *testing.T, a *Activities, clusterID uuid.UUID) error {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.LabelSystemNamespacesForPSA)

	_, err := env.ExecuteActivity(a.LabelSystemNamespacesForPSA, LabelSystemNamespacesForPSAInput{ClusterID: clusterID})
	return err
}
