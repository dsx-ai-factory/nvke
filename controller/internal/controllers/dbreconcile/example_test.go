// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbreconcile_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

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

const (
	clusterMetadataNamespace = "nke-system"
)

// ExampleAddController registers a controller that reacts to both DB changes
// and Kubernetes objects owned by a cluster row.
func ExampleAddController() {
	ctx := context.Background()
	bunDB, err := testutil.NewDB(ctx, (*db.Cluster)(nil))
	if err != nil {
		panic(err)
	}
	defer func() { _ = bunDB.Close() }()
	database := dbwrapper.New(db.NewDB(bunDB), slog.Default())

	mgr, err := controllertest.NewManager(nil)
	if err != nil {
		panic(err)
	}

	reconciler := &clusterMetadataReconciler{
		db:   database,
		kube: mgr.GetClient(),
	}
	if err := dbreconcile.AddController(mgr, dbreconcile.Controller{
		// Name identifies the controller in logs and metrics.
		Name: "cluster-metadata",
		// Reconciler receives every key produced by Sources and Watches.
		Reconciler: reconciler,
		// Sources trigger reconciliation from database changes.
		Sources: []dbreconcile.Source{
			// NewClusterSource watches every Cluster row because the filter is empty.
			// It enqueues ClusterKey(cluster.ID) after DB changes and during
			// startup or periodic resyncs, causing Reconcile to reload that row.
			dbwrapper.NewClusterSource(database, dbfilters.Cluster{}),
		},
		// Watches trigger reconciliation from Kubernetes object changes.
		Watches: []dbreconcile.KubeWatch{
			// KindCluster.Owns watches Secrets in this namespace carrying an
			// owner.nke.nvidia.com/cluster=<cluster UUID> label. Secret create,
			// update, and delete events enqueue that UUID as a ClusterKey. The
			// reconciler adds the matching label below with SetOwner.
			dbreconcile.KindCluster.Owns(&corev1.Secret{}, clusterMetadataNamespace),
		},
		// Workers allows two different Cluster keys to reconcile concurrently.
		Workers: 2,
		// ReconcileTimeout cancels a reconcile that exceeds this duration.
		ReconcileTimeout: 30 * time.Second,
	}); err != nil {
		panic(err)
	}

	// Output:
}

type clusterMetadataReconciler struct {
	db   *dbwrapper.DB
	kube client.Client
}

func (r *clusterMetadataReconciler) Reconcile(
	ctx context.Context,
	key dbreconcile.Key,
) (reconcile.Result, error) {
	cluster, err := r.db.GetCluster(ctx, key.ID)
	if errors.Is(err, db.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting cluster %s: %w", key.ID, err)
	}

	objectKey := client.ObjectKey{
		Namespace: clusterMetadataNamespace,
		Name:      "cluster-" + cluster.Name,
	}
	secret := new(corev1.Secret)
	if err := r.kube.Get(ctx, objectKey, secret); err == nil {
		return reconcile.Result{}, nil
	} else if !apierrors.IsNotFound(err) {
		return reconcile.Result{}, fmt.Errorf("getting Secret %s: %w", objectKey, err)
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectKey.Name,
			Namespace: objectKey.Namespace,
		},
		Data: map[string][]byte{"cluster-id": []byte(cluster.ID.String())},
	}
	// SetOwner adds owner.nke.nvidia.com/cluster=<cluster UUID>. The Owns watch
	// uses that label both to select this Secret and to map its events back to
	// the cluster's reconcile key.
	dbreconcile.SetOwner(secret, dbreconcile.KindCluster, cluster.ID)
	if err := r.kube.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return reconcile.Result{}, fmt.Errorf("creating Secret %s: %w", objectKey, err)
	}
	return reconcile.Result{}, nil
}
