// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalv1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
)

var nodePoolOwnedConditions = []string{
	clusterv1.AvailableCondition,
	internalv1.SyncedCondition,
	internalv1.ReadyCondition,
	clusterv1.PausedCondition,
	clusterv1.DeletingCondition,
}

// NodePoolReconciler reconciles a NodePool object
type NodePoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=internal.nke.nvidia.com,resources=nodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=internal.nke.nvidia.com,resources=nodepools/status,verbs=get;update;patch

func (r *NodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("reconciling NodePool")

	var pool internalv1.NodePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	patchHelper, err := patch.NewHelper(&pool, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		if conditionErr := setNodePoolConditions(&pool); conditionErr != nil {
			retErr = errors.Join(retErr, conditionErr)
		}
		if patchErr := patchHelper.Patch(ctx, &pool,
			patch.WithOwnedConditions{Conditions: nodePoolOwnedConditions},
			patch.WithStatusObservedGeneration{},
		); patchErr != nil {
			retErr = errors.Join(retErr, patchErr)
		}
	}()

	if annotations.HasPaused(&pool) {
		conditions.Set(&pool, metav1.Condition{
			Type: clusterv1.PausedCondition, Status: metav1.ConditionTrue, Reason: clusterv1.PausedReason,
		})
		return ctrl.Result{}, nil
	}
	conditions.Set(&pool, metav1.Condition{
		Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason,
	})
	return ctrl.Result{}, nil
}

func setNodePoolConditions(pool *internalv1.NodePool) error {
	deleting := metav1.ConditionFalse
	reason := clusterv1.NotDeletingReason
	if !pool.DeletionTimestamp.IsZero() {
		deleting = metav1.ConditionTrue
		reason = clusterv1.DeletingReason
	}
	conditions.Set(pool, metav1.Condition{Type: clusterv1.DeletingCondition, Status: deleting, Reason: reason})

	if err := conditions.SetSummaryCondition(pool, pool, internalv1.SyncedCondition,
		conditions.ForConditionTypes{internalv1.WorkersReadyCondition},
		conditions.CustomMergeStrategy{MergeStrategy: conditions.DefaultMergeStrategy(
			conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
				internalv1.NotSyncedReason, internalv1.SyncUnknownReason, internalv1.SyncedReason,
			)),
		)},
	); err != nil {
		return fmt.Errorf("summarize NodePool Synced condition: %w", err)
	}
	if err := conditions.SetSummaryCondition(pool, pool, clusterv1.AvailableCondition,
		conditions.ForConditionTypes{internalv1.WorkersReadyCondition},
		conditions.CustomMergeStrategy{MergeStrategy: conditions.DefaultMergeStrategy(
			conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
				clusterv1.NotAvailableReason, clusterv1.AvailableUnknownReason, clusterv1.AvailableReason,
			)),
		)},
	); err != nil {
		return fmt.Errorf("summarize NodePool Available condition: %w", err)
	}
	if err := conditions.SetSummaryCondition(pool, pool, internalv1.ReadyCondition,
		conditions.ForConditionTypes{clusterv1.AvailableCondition, clusterv1.DeletingCondition},
		conditions.NegativePolarityConditionTypes{clusterv1.DeletingCondition},
		conditions.CustomMergeStrategy{MergeStrategy: conditions.DefaultMergeStrategy(
			conditions.GetPriorityFunc(conditions.GetDefaultMergePriorityFunc(clusterv1.DeletingCondition)),
			conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
				clusterv1.NotReadyReason, clusterv1.ReadyUnknownReason, clusterv1.ReadyReason,
			)),
		)},
	); err != nil {
		return fmt.Errorf("summarize NodePool Ready condition: %w", err)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&internalv1.NodePool{}).
		Named("nodepool").
		Complete(r)
}
