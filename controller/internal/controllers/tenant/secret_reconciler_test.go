// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenant

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/tenantaccess"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type staticClusterReader struct {
	cluster db.Cluster
	err     error
}

func (r staticClusterReader) GetCluster(context.Context, uuid.UUID) (db.Cluster, error) {
	return r.cluster, r.err
}

func TestSecretReconcilerFeedsSecretIdentityToManagerSet(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster := db.Cluster{ID: uuid.New(), Name: "active"}
	secret := tenantAccessSecret(cluster.ID, cluster.Name, "uid-active", "7", []byte("active-kubeconfig"))

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	started := make(chan Target, 1)
	managerSet := newTestManagerSet(t, ctx, runtimeFactoryFunc(func(_ context.Context, target Target) (Runtime, error) {
		started <- target
		return newFakeRuntime(nil), nil
	}), 1)
	t.Cleanup(managerSet.Shutdown)
	reconciler := NewSecretReconciler(staticClusterReader{cluster: cluster}, reader, managerSet, "cloud-workflow-engine")

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: secret.Namespace,
		Name:      secret.Name,
	}})
	require.NoError(t, err)
	target := <-started
	require.Equal(t, cluster.ID, target.ClusterID)
	require.Equal(t, types.NamespacedName{
		Namespace: tenantaccess.SecretsNamespace,
		Name:      "active-kubeconfig",
	}, target.KubeconfigSecretRef)
	require.Equal(t, types.UID("uid-active"), target.KubeconfigSecretUID)
	require.Equal(t, []byte("active-kubeconfig"), target.Kubeconfig)
}

func TestSecretReconcilerIgnoresMetadataOnlySecretUpdate(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster := db.Cluster{ID: uuid.New(), Name: "active"}
	secret := tenantAccessSecret(cluster.ID, cluster.Name, "uid-active", "7", []byte("active-kubeconfig"))

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	started := make(chan struct{}, 2)
	stopped := make(chan struct{}, 2)
	managerSet := newTestManagerSet(t, ctx, runtimeFactoryFunc(func(_ context.Context, _ Target) (Runtime, error) {
		started <- struct{}{}
		return newFakeRuntime(func() { stopped <- struct{}{} }), nil
	}), 1)
	t.Cleanup(managerSet.Shutdown)
	reconciler := NewSecretReconciler(staticClusterReader{cluster: cluster}, reader, managerSet, "cloud-workflow-engine")
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}}

	_, err := reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(testEventuallyTimeout):
		t.Fatal("runtime was not started")
	}

	updated := &corev1.Secret{}
	require.NoError(t, reader.Get(ctx, request.NamespacedName, updated))
	updated.Annotations = map[string]string{"metadata-only": "change"}
	require.NoError(t, reader.Update(ctx, updated))
	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Never(t, func() bool {
		return len(started) > 0 || len(stopped) > 0
	}, 50*time.Millisecond, testEventuallyInterval)
}

func TestSecretReconcilerStopsRuntimeWhenSecretIsDeleted(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster := db.Cluster{ID: uuid.New(), Name: "active"}
	secret := tenantAccessSecret(cluster.ID, cluster.Name, "uid-active", "7", []byte("active-kubeconfig"))

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	started := make(chan struct{}, 1)
	stopped := make(chan struct{}, 1)
	managerSet := newTestManagerSet(t, ctx, runtimeFactoryFunc(func(_ context.Context, _ Target) (Runtime, error) {
		started <- struct{}{}
		return newFakeRuntime(func() { stopped <- struct{}{} }), nil
	}), 1)
	t.Cleanup(managerSet.Shutdown)
	reconciler := NewSecretReconciler(staticClusterReader{cluster: cluster}, reader, managerSet, "cloud-workflow-engine")
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}}

	_, err := reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(testEventuallyTimeout):
		t.Fatal("runtime was not started")
	}
	require.NoError(t, reader.Delete(ctx, secret))
	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	select {
	case <-stopped:
	case <-time.After(testEventuallyTimeout):
		t.Fatal("runtime was not stopped")
	}
}

func TestSecretReconcilerRejectsDeletingCluster(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster := db.Cluster{
		ID:   uuid.New(),
		Name: "deleting",
		Conditions: db.ClusterConditions{
			Deleting: db.Condition{Status: db.ConditionStatusTrue},
		},
	}
	secret := tenantAccessSecret(cluster.ID, cluster.Name, "uid-deleting", "4", []byte("kubeconfig"))
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	starts := 0
	managerSet := newTestManagerSet(t, ctx, runtimeFactoryFunc(func(_ context.Context, _ Target) (Runtime, error) {
		starts++
		return newFakeRuntime(nil), nil
	}), 1)
	t.Cleanup(managerSet.Shutdown)
	reconciler := NewSecretReconciler(staticClusterReader{cluster: cluster}, reader, managerSet, "cloud-workflow-engine")

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}})
	require.NoError(t, err)
	require.Zero(t, starts)
}

func tenantAccessSecret(clusterID uuid.UUID, clusterName, uid, resourceVersion string, kubeconfig []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       tenantaccess.SecretsNamespace,
			Name:            tenantaccess.KubeconfigSecretName(clusterName),
			UID:             types.UID(uid),
			ResourceVersion: resourceVersion,
			Labels: map[string]string{
				constants.LabelKeyNvkeManagedBy: "cloud-workflow-engine",
				constants.LabelKeyClusterID:     clusterID.String(),
			},
		},
		Data: map[string][]byte{tenantaccess.KubeconfigSecretKey: kubeconfig},
	}
}
