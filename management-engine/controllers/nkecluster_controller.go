// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Controller setup and reconcile dispatch coordinate lifecycle work and status persistence.

package controllers

import (
	"context"
	"errors"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/dsx-ai-factory/nke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
)

const (
	nkeClusterFinalizer = "internal.nke.nvidia.com/cluster-cleanup"
	defaultPollInterval = 10 * time.Second
)

// CWEClient is the existing REST API surface used by cluster reconciliation.
type CWEClient interface {
	Endpoint() string
	FindByName(ctx context.Context, name string) (*api.Cluster, error)
	Get(ctx context.Context, id string) (*api.Cluster, error)
	Create(ctx context.Context, request api.CreateClusterRequest) (*api.Cluster, error)
	Delete(ctx context.Context, id string) error
}

// NKEClusterReconciler coordinates cluster provisioning from declarative intent.
type NKEClusterReconciler struct {
	client.Client
	CWE          CWEClient
	Bindings     []CWEBinding
	PollInterval time.Duration
}

// +kubebuilder:rbac:groups=internal.nke.nvidia.com,resources=nkeclusters,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=internal.nke.nvidia.com,resources=nkeclusters/status,verbs=update;patch

func (r *NKEClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("reconciling NKECluster")

	var cluster internalv1.NKECluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	patchHelper, err := patch.NewHelper(&cluster, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	observed := true
	defer func() {
		if conditionErr := setNKEClusterConditions(&cluster); conditionErr != nil {
			retErr = errors.Join(retErr, conditionErr)
		}
		if observed && retErr == nil {
			// Set this explicitly: the patch helper returns early when other status fields are unchanged.
			cluster.Status.ObservedGeneration = cluster.Generation
		}
		if patchErr := patchHelper.Patch(ctx, &cluster, patch.WithOwnedConditions{Conditions: nkeClusterOwnedConditions}); client.IgnoreNotFound(patchErr) != nil {
			retErr = errors.Join(retErr, patchErr)
		}
	}()

	if r.CWE == nil {
		setLifecycleConditions(&cluster, metav1.ConditionUnknown, metav1.ConditionUnknown, "ProvisioningNotConfigured", "Cluster provisioning is not configured")
		return ctrl.Result{}, nil
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &cluster)
	}
	if !controllerutil.ContainsFinalizer(&cluster, nkeClusterFinalizer) {
		// Persist cleanup responsibility before making any provisioning request.
		controllerutil.AddFinalizer(&cluster, nkeClusterFinalizer)
		observed = false
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}
	return r.reconcileCluster(ctx, &cluster)
}

// SetupWithManager sets up the controller with the Manager.
func (r *NKEClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.PollInterval <= 0 {
		r.PollInterval = defaultPollInterval
	}
	// Status writes must not resubmit rejected requests. Deletion must still run without a spec change.
	return ctrl.NewControllerManagedBy(mgr).
		For(&internalv1.NKECluster{}, builder.WithPredicates(predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
				!e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp())
		}})).
		Named("nkecluster").
		Complete(r)
}
