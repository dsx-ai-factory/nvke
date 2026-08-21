// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/constants"
)

func TestMigrateLegacyCAPINamespaceObjectsTerminatingNamespace(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	k8sClient := fake.NewClientBuilder().
		WithScheme(legacyMigrationScheme(t)).
		WithObjects(
			&corev1.Namespace{
				TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Namespace"},
				ObjectMeta: metav1.ObjectMeta{
					Name:              "cluster-test-cluster",
					DeletionTimestamp: &now,
					Finalizers:        []string{"kubernetes"},
				},
			},
			&clusterv1.Cluster{
				TypeMeta: metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster"},
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-cluster",
					Namespace:         "cluster-test-cluster",
					DeletionTimestamp: &now,
					Finalizers:        []string{"cluster.cluster.x-k8s.io"},
				},
			},
		).
		Build()

	migrated, err := migrateLegacyCAPINamespaceObjects(ctx, k8sClient, "test-cluster")
	require.NoError(t, err)
	assert.False(t, migrated)

	copied := &clusterv1.Cluster{}
	err = k8sClient.Get(ctx, ctrlclient.ObjectKey{
		Namespace: constants.NamespaceNKESystem,
		Name:      "test-cluster",
	}, copied)
	require.True(t, apierrors.IsNotFound(err), "terminating leftover must not be copied")
}

func TestCopyLegacyCAPIObjects(t *testing.T) {
	ctx := context.Background()
	legacy := testLegacyCAPIObjects()
	k8sClient := fake.NewClientBuilder().WithScheme(legacyMigrationScheme(t)).Build()

	require.NoError(t, copyLegacyCAPINamespaceObjects(ctx, k8sClient, legacy))

	cluster := &clusterv1.Cluster{}
	require.NoError(t, k8sClient.Get(ctx, ctrlclient.ObjectKey{
		Namespace: constants.NamespaceNKESystem,
		Name:      "test-cluster",
	}, cluster))
	assert.Equal(t, "true", cluster.Annotations[clusterv1.PausedAnnotation])
	require.NotNil(t, cluster.Spec.Paused)
	assert.True(t, *cluster.Spec.Paused)
	assert.Empty(t, cluster.Finalizers)
	assert.Empty(t, cluster.Status)

	machine := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(ctx, ctrlclient.ObjectKey{
		Namespace: constants.NamespaceNKESystem,
		Name:      "test-node",
	}, machine))
	assert.Equal(t, "true", machine.Annotations[clusterv1.PausedAnnotation])
	assert.Empty(t, machine.Status)

	nicoMachine := &nicov1.NicoMachine{}
	require.NoError(t, k8sClient.Get(ctx, ctrlclient.ObjectKey{
		Namespace: constants.NamespaceNKESystem,
		Name:      "test-node",
	}, nicoMachine))
	assert.Equal(t, "true", nicoMachine.Annotations[clusterv1.PausedAnnotation])
	assert.Equal(t, "nico://instance-1", nicoMachine.Spec.ProviderID)
	assert.Empty(t, nicoMachine.Status)
}

func TestDeleteLegacyCAPIObjectsPausesBeforeDelete(t *testing.T) {
	ctx := context.Background()
	legacy := testLegacyCAPIObjects()
	k8sClient := &pauseCheckingClient{
		Client: fake.NewClientBuilder().
			WithScheme(legacyMigrationScheme(t)).
			WithObjects(legacyObjectsAsClientObjects(legacy)...).
			Build(),
		t: t,
	}

	require.NoError(t, deleteLegacyCAPINamespaceObjects(ctx, k8sClient, "cluster-test-cluster", legacy))
	assert.Equal(t, []string{"NicoMachine/test-node", "Machine/test-node", "NicoCluster/test-cluster", "Cluster/test-cluster", "Namespace/cluster-test-cluster"}, k8sClient.deletes)
}

type pauseCheckingClient struct {
	ctrlclient.Client
	t       *testing.T
	deletes []string
}

func (c *pauseCheckingClient) Delete(ctx context.Context, obj ctrlclient.Object, opts ...ctrlclient.DeleteOption) error {
	c.t.Helper()
	if _, isNamespace := obj.(*corev1.Namespace); !isNamespace {
		assert.Equal(c.t, "true", obj.GetAnnotations()[clusterv1.PausedAnnotation])
	}
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	switch obj.(type) {
	case *corev1.Namespace:
		kind = "Namespace"
	case *clusterv1.Cluster:
		kind = "Cluster"
	case *clusterv1.Machine:
		kind = "Machine"
	}
	c.deletes = append(c.deletes, kind+"/"+obj.GetName())
	return c.Client.Delete(ctx, obj, opts...)
}

func testLegacyCAPIObjects() *legacyCAPIObjects {
	namespace := "cluster-test-cluster"
	cluster := &clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-cluster",
			Namespace:  namespace,
			Finalizers: []string{"cluster.cluster.x-k8s.io"},
		},
		Status: clusterv1.ClusterStatus{Initialization: clusterv1.ClusterInitializationStatus{InfrastructureProvisioned: boolPtr(true)}},
	}
	machine := clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-node",
			Namespace:  namespace,
			Finalizers: []string{"machine.cluster.x-k8s.io"},
			Labels:     map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
		},
	}
	nicoCluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": nicov1.GroupVersion.String(),
		"kind":       "NicoCluster",
		"metadata": map[string]any{
			"name":       "test-cluster",
			"namespace":  namespace,
			"finalizers": []any{"nicocluster.infrastructure.cluster.x-k8s.io"},
		},
		"spec": map[string]any{"siteID": "site-1"},
	}}
	nicoMachine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": nicov1.GroupVersion.String(),
		"kind":       "NicoMachine",
		"metadata": map[string]any{
			"name":       "test-node",
			"namespace":  namespace,
			"finalizers": []any{"nicomachine.infrastructure.cluster.x-k8s.io"},
			"labels":     map[string]any{clusterv1.ClusterNameLabel: "test-cluster"},
		},
		"spec": map[string]any{"providerID": "nico://instance-1"},
		"status": map[string]any{
			"ready": true,
		},
	}}
	return &legacyCAPIObjects{
		cluster:       cluster,
		infraCluster:  nicoCluster,
		machines:      []clusterv1.Machine{machine},
		infraMachines: []*unstructured.Unstructured{nicoMachine},
	}
}

func legacyObjectsAsClientObjects(legacy *legacyCAPIObjects) []ctrlclient.Object {
	return []ctrlclient.Object{
		&corev1.Namespace{
			TypeMeta:   metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-test-cluster"},
		},
		legacy.cluster,
		legacy.infraCluster,
		&legacy.machines[0],
		legacy.infraMachines[0],
	}
}

func legacyMigrationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))
	return scheme
}

func boolPtr(value bool) *bool {
	return &value
}
