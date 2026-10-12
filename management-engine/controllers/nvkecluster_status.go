// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Status mapping presents provisioning observations and failures as public cluster conditions.

package controllers

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/dsx-ai-factory/nvke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

var nvkeClusterOwnedConditions = []string{
	clusterv1.AvailableCondition,
	internalv1.SyncedCondition,
	internalv1.ReadyCondition,
	internalv1.ControlPlaneReadyCondition,
}

func setClusterStatus(cluster *internalv1.NVKECluster, observedCluster *api.Cluster) {
	ready := metav1.ConditionFalse
	reason, message := "Provisioning", "Waiting for control-plane provisioning"
	if observedCluster.Conditions.Deleting.Status == api.ConditionStatus("True") {
		reason, message = clusterv1.DeletingReason, "Cluster deletion is in progress"
	} else if observedCluster.Conditions.Deleting.Status == api.ConditionStatus("False") &&
		observedCluster.Conditions.Provisioned.Status == api.ConditionStatus("True") &&
		observedCluster.Conditions.Provisioning.Status == api.ConditionStatus("False") {
		ready, reason, message = metav1.ConditionTrue, "Provisioned", "Control-plane provisioning completed"
	} else if observedCluster.Conditions.Provisioning.Status == api.ConditionStatus("False") {
		reason, message = "ProvisioningFailed", "Control-plane provisioning did not complete"
	}
	conditions.Set(cluster, metav1.Condition{Type: internalv1.ControlPlaneReadyCondition, Status: ready, Reason: reason, Message: message})
	if cluster.Status.Provisioning.Generation != cluster.Generation {
		conditions.Set(cluster, metav1.Condition{Type: internalv1.SyncedCondition, Status: metav1.ConditionFalse,
			Reason: "UnsupportedUpdate", Message: "Changes to an existing cluster are not supported; observing the original request"})
	} else {
		conditions.Set(cluster, metav1.Condition{Type: internalv1.SyncedCondition, Status: metav1.ConditionTrue,
			Reason: internalv1.SyncedReason, Message: "Cluster configuration has been accepted"})
	}
}

func setNVKEClusterConditions(cluster *internalv1.NVKECluster) error {
	// Remove conditions left by earlier development builds.
	conditions.Delete(cluster, clusterv1.PausedCondition)
	conditions.Delete(cluster, clusterv1.DeletingCondition)

	if err := conditions.SetSummaryCondition(cluster, cluster, clusterv1.AvailableCondition,
		conditions.ForConditionTypes{internalv1.ControlPlaneReadyCondition},
		conditions.CustomMergeStrategy{MergeStrategy: conditions.DefaultMergeStrategy(
			conditions.SummaryMessageTransformFunc(plainSummaryMessages),
			conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
				clusterv1.NotAvailableReason, clusterv1.AvailableUnknownReason, clusterv1.AvailableReason,
			)),
		)},
	); err != nil {
		return fmt.Errorf("summarize NVKECluster Available condition: %w", err)
	}
	if !cluster.DeletionTimestamp.IsZero() {
		reason, message := clusterv1.DeletingReason, "Cluster deletion is in progress"
		// Keep cleanup failures visible when clients only inspect Ready.
		if synced := conditions.Get(cluster, internalv1.SyncedCondition); synced != nil && synced.Status != metav1.ConditionTrue && synced.Reason != clusterv1.DeletingReason {
			reason, message = synced.Reason, synced.Message
		}
		conditions.Set(cluster, metav1.Condition{Type: internalv1.ReadyCondition, Status: metav1.ConditionFalse,
			Reason: reason, Message: message})
		return nil
	}
	if err := conditions.SetSummaryCondition(cluster, cluster, internalv1.ReadyCondition,
		conditions.ForConditionTypes{clusterv1.AvailableCondition, internalv1.SyncedCondition},
		conditions.CustomMergeStrategy{MergeStrategy: conditions.DefaultMergeStrategy(
			conditions.SummaryMessageTransformFunc(plainSummaryMessages),
			conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
				clusterv1.NotReadyReason, clusterv1.ReadyUnknownReason, clusterv1.ReadyReason,
			)),
		)},
	); err != nil {
		return fmt.Errorf("summarize NVKECluster Ready condition: %w", err)
	}
	return nil
}

// Flatten CAPI's labels so nested summaries show each user-facing message once.
// Messages must already be safe for users. This only changes presentation.
func plainSummaryMessages(messages []string) []string {
	var plain []string
	for _, message := range messages {
		if label, text, found := strings.Cut(message, ":"); found && strings.HasPrefix(label, "* ") {
			message = text
		}
		for line := range strings.SplitSeq(message, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !slices.Contains(plain, line) {
				plain = append(plain, line)
			}
		}
	}
	return plain
}

func setLifecycleConditions(cluster *internalv1.NVKECluster, synced, ready metav1.ConditionStatus, reason, message string) {
	conditions.Set(cluster, metav1.Condition{Type: internalv1.SyncedCondition, Status: synced, Reason: reason, Message: message})
	conditions.Set(cluster, metav1.Condition{Type: internalv1.ControlPlaneReadyCondition, Status: ready, Reason: reason, Message: message})
}

func (r *NVKEClusterReconciler) lifecycleError(cluster *internalv1.NVKECluster, err error) (ctrl.Result, error) {
	if errors.Is(err, errCreateRejected) {
		setLifecycleConditions(cluster, metav1.ConditionFalse, metav1.ConditionFalse, "CreateRejected", "Cluster creation request was rejected")
		return ctrl.Result{}, nil
	}
	if invalid, ok := errors.AsType[*InvalidSpecError](err); ok {
		setLifecycleConditions(cluster, metav1.ConditionFalse, metav1.ConditionFalse, "UnsupportedConfiguration", invalid.Message)
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}
	setLifecycleConditions(cluster, metav1.ConditionUnknown, metav1.ConditionUnknown, "ReconcileFailed", "Unable to complete the cluster operation; contact the service administrator")
	return ctrl.Result{}, err
}
