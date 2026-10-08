// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Deletion retains cleanup responsibility until managed resources are confirmed absent.

package controllers

import (
	"context"
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/dsx-ai-factory/nke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nke/management-engine/internal/cwe"
)

func (r *NKEClusterReconciler) reconcileDelete(ctx context.Context, cluster *internalv1.NKECluster) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, nkeClusterFinalizer) {
		return ctrl.Result{}, nil
	}
	setLifecycleConditions(cluster, metav1.ConditionFalse, metav1.ConditionUnknown, clusterv1.DeletingReason, "Waiting for cluster deletion")
	observedCluster, err := r.findCluster(ctx, cluster)
	if errors.Is(err, cwe.ErrNotFound) {
		if state := cluster.Status.Provisioning; state != nil && state.ID == "" {
			setLifecycleConditions(cluster, metav1.ConditionFalse, metav1.ConditionUnknown, "CreateOutcomeUnknown",
				"Cluster creation outcome is unknown; contact the service administrator before deletion")
			return ctrl.Result{RequeueAfter: r.PollInterval}, nil
		}
		controllerutil.RemoveFinalizer(cluster, nkeClusterFinalizer)
		return ctrl.Result{}, nil
	}
	if err != nil {
		return r.lifecycleError(cluster, err)
	}
	if observedCluster.Conditions.Deleting.Status != api.ConditionStatus("True") {
		if err := r.CWE.Delete(ctx, observedCluster.ID.String()); err != nil && !errors.Is(err, cwe.ErrNotFound) {
			return r.lifecycleError(cluster, err)
		}
	}
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}
