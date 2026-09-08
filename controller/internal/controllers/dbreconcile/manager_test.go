// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbreconcile_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/NVIDIA/nke/controller/internal/controllers/controllertest"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

const testClusterSecretNamespace = "nke-system"

func TestAddControllerReconcilesDatabaseAndKubernetesChanges(t *testing.T) {
	bunDB := testutil.SetupDB(t.Context(), t, (*db.Cluster)(nil))
	database := dbwrapper.New(db.NewDB(bunDB), slog.New(slog.DiscardHandler))
	mgr, err := controllertest.NewManager(nil)
	require.NoError(t, err)

	cluster := db.Cluster{
		ID:                uuid.New(),
		Name:              "initial-name",
		OrgID:             "example-org",
		SiteID:            uuid.New(),
		VpcID:             uuid.New(),
		KubernetesVersion: "v1.34.0",
	}
	cluster, err = database.CreateCluster(t.Context(), &cluster)
	require.NoError(t, err)

	reconciler := &clusterSecretReconciler{db: database, kube: mgr.GetClient()}
	require.NoError(t, dbreconcile.AddController(mgr, dbreconcile.Controller{
		Name:       "cluster-secret-controller",
		Reconciler: reconciler,
		Sources: []dbreconcile.Source{
			dbwrapper.NewClusterSource(database, dbfilters.Cluster{}),
		},
		Watches: []dbreconcile.KubeWatch{
			dbreconcile.KindCluster.Owns(&corev1.Secret{}, testClusterSecretNamespace),
		},
	}))
	controllertest.Start(t.Context(), t, mgr)

	t.Run("initial database state creates the Secret", func(t *testing.T) {
		requireClusterSecretValue(t, mgr.GetClient(), cluster.ID, cluster.Name)
	})

	t.Run("database update changes the Secret", func(t *testing.T) {
		cluster.Name = "updated-name"
		cluster, err = database.UpdateCluster(t.Context(), &cluster)
		require.NoError(t, err)

		requireClusterSecretValue(t, mgr.GetClient(), cluster.ID, cluster.Name)
	})

	t.Run("Kubernetes drift is repaired", func(t *testing.T) {
		secret := getClusterSecret(t, mgr.GetClient(), cluster.ID)
		secret.Data["cluster-name"] = []byte("drifted-name")
		require.NoError(t, mgr.GetClient().Update(t.Context(), secret))

		requireClusterSecretValue(t, mgr.GetClient(), cluster.ID, cluster.Name)
	})

	t.Run("Kubernetes deletion recreates the Secret", func(t *testing.T) {
		secret := getClusterSecret(t, mgr.GetClient(), cluster.ID)
		require.NoError(t, mgr.GetClient().Delete(t.Context(), secret))

		requireClusterSecretValue(t, mgr.GetClient(), cluster.ID, cluster.Name)
	})
}

type clusterSecretReconciler struct {
	db   *dbwrapper.DB
	kube client.Client
}

func (r *clusterSecretReconciler) Reconcile(
	ctx context.Context,
	key dbreconcile.Key,
) (reconcile.Result, error) {
	cluster, err := r.db.GetCluster(ctx, key.ID)
	if errors.Is(err, db.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}

	objectKey := clusterSecretKey(cluster.ID)
	secret := new(corev1.Secret)
	create := false
	err = r.kube.Get(ctx, objectKey, secret)
	if apierrors.IsNotFound(err) {
		create = true
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      objectKey.Name,
			Namespace: objectKey.Namespace,
		}}
		dbreconcile.SetOwner(secret, dbreconcile.KindCluster, cluster.ID)
	} else if err != nil {
		return reconcile.Result{}, err
	}

	want := cluster.Name
	if string(secret.Data["cluster-name"]) == want {
		return reconcile.Result{}, nil
	}
	secret.Data = map[string][]byte{"cluster-name": []byte(want)}
	if create {
		return reconcile.Result{}, r.kube.Create(ctx, secret)
	}
	return reconcile.Result{}, r.kube.Update(ctx, secret)
}

func requireClusterSecretValue(t *testing.T, kubeClient client.Client, clusterID uuid.UUID, want string) {
	t.Helper()
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		secret := new(corev1.Secret)
		if !assert.NoError(collect, kubeClient.Get(t.Context(), clusterSecretKey(clusterID), secret)) {
			return
		}
		assert.Equal(collect, want, string(secret.Data["cluster-name"]))
	}, 10*time.Second, 10*time.Millisecond)
}

func getClusterSecret(t *testing.T, kubeClient client.Client, clusterID uuid.UUID) *corev1.Secret {
	t.Helper()
	secret := new(corev1.Secret)
	require.NoError(t, kubeClient.Get(t.Context(), clusterSecretKey(clusterID), secret))
	return secret
}

func clusterSecretKey(clusterID uuid.UUID) client.ObjectKey {
	return client.ObjectKey{
		Name:      "cluster-" + clusterID.String(),
		Namespace: testClusterSecretNamespace,
	}
}
