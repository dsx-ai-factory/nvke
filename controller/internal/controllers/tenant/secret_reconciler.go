// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/tenantaccess"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

type clusterReader interface {
	GetCluster(ctx context.Context, id uuid.UUID) (db.Cluster, error)
}

// SecretReconciler translates management-cluster kubeconfig Secret events into
// tenant-manager lifecycle changes. The informer LIST on startup also supplies
// create events for Secrets that existed before CWE restarted.
type SecretReconciler struct {
	database            clusterReader
	reader              client.Reader
	managerSet          *ManagerSet
	managedByLabelValue string
}

func NewSecretReconciler(database clusterReader, reader client.Reader, managerSet *ManagerSet, managedByLabelValue string) *SecretReconciler {
	return &SecretReconciler{
		database:            database,
		reader:              reader,
		managerSet:          managerSet,
		managedByLabelValue: managedByLabelValue,
	}
}

func (r *SecretReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		Named("tenant-access-secret").
		For(&corev1.Secret{}, builder.WithPredicates(tenantAccessSecretPredicate(r.managedByLabelValue))).
		Complete(r)
}

func (r *SecretReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	secretRef := request.NamespacedName
	secret := &corev1.Secret{}
	if err := r.reader.Get(ctx, request.NamespacedName, secret); err != nil {
		if apierrors.IsNotFound(err) {
			r.managerSet.RemoveKubeconfigSecret(secretRef)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting tenant-access Secret %s: %w", secretRef, err)
	}

	clusterID, ok := clusterIDForKubeconfigSecret(secret, r.managedByLabelValue)
	if !ok {
		r.managerSet.RemoveKubeconfigSecret(secretRef)
		return ctrl.Result{}, nil
	}

	cluster, err := r.database.GetCluster(ctx, clusterID)
	if errors.Is(err, db.ErrNotFound) {
		r.managerSet.RemoveCluster(clusterID)
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting cluster %s for tenant-access Secret %s: %w", clusterID, secretRef, err)
	}
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue || secret.Name != tenantaccess.KubeconfigSecretName(cluster.Name) {
		r.managerSet.RemoveCluster(clusterID)
		return ctrl.Result{}, nil
	}

	kubeconfig := secret.Data[tenantaccess.KubeconfigSecretKey]
	if len(kubeconfig) == 0 {
		r.managerSet.RemoveCluster(clusterID)
		return ctrl.Result{}, nil
	}

	if err := r.managerSet.Upsert(ctx, Target{
		ClusterID:           cluster.ID,
		ClusterName:         cluster.Name,
		KubeconfigSecretRef: secretRef,
		KubeconfigSecretUID: secret.UID,
		Kubeconfig:          kubeconfig,
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying tenant manager target for cluster %s: %w", clusterID, err)
	}
	return ctrl.Result{}, nil
}

func clusterIDForKubeconfigSecret(secret *corev1.Secret, managedByLabelValue string) (uuid.UUID, bool) {
	if secret.Namespace != tenantaccess.SecretsNamespace ||
		secret.Labels[constants.LabelKeyNkeManagedBy] != managedByLabelValue ||
		!tenantaccess.IsCWEKubeconfigSecretName(secret.Name) {
		return uuid.Nil, false
	}
	clusterID, err := uuid.Parse(secret.Labels[constants.LabelKeyClusterID])
	return clusterID, err == nil
}

func tenantAccessSecretPredicate(managedByLabelValue string) predicate.Predicate {
	matches := func(object client.Object) bool {
		secret, ok := object.(*corev1.Secret)
		if !ok {
			return false
		}
		_, matches := clusterIDForKubeconfigSecret(secret, managedByLabelValue)
		return matches
	}

	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return matches(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return matches(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return matches(e.ObjectOld) || matches(e.ObjectNew) },
		GenericFunc: func(e event.GenericEvent) bool { return matches(e.Object) },
	}
}
