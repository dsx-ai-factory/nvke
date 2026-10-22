// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Stored resources are projected into public fields and reviewed lifecycle values.
package cluster

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/apis/cluster/mapping"
	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

// result validates stored data before returning the public resource projection.
func (s *Service) result(ctx context.Context, operation, name string, object *unstructured.Unstructured, err error) (*clusterv1.Cluster, error) {
	if err != nil {
		return nil, s.storageError(ctx, operation, name, err)
	}
	if object == nil || object.GetNamespace() != s.config.Namespace {
		return nil, s.internalError(ctx, operation, name, "storage returned an invalid cluster", err)
	}
	spec, ok := object.Object[specField].(map[string]any)
	if !ok || spec == nil {
		return nil, s.internalError(ctx, operation, name, "stored cluster spec is invalid", nil)
	}
	publicSpec, err := mapping.ProjectSpec(spec)
	if err != nil {
		return nil, s.internalError(ctx, operation, name, "stored cluster cannot be projected", err)
	}
	return &clusterv1.Cluster{
		Name: object.GetName(), Uid: string(object.GetUID()), ResourceVersion: object.GetResourceVersion(),
		Generation: object.GetGeneration(), Spec: publicSpec, Status: projectStatus(object),
	}, nil
}

// projectStatus translates current observations without exposing private diagnostics.
func projectStatus(object *unstructured.Unstructured) *clusterv1.ClusterStatus {
	out := &clusterv1.ClusterStatus{State: clusterv1.ClusterStatus_STATE_PENDING, Reason: clusterv1.ClusterStatus_REASON_AWAITING_STATUS}
	ready := condition(object, internalv1.ReadyCondition)
	if ready != nil {
		out.ObservedGeneration = ready.ObservedGeneration
	}
	if object.GetDeletionTimestamp() != nil {
		out.State, out.Reason = clusterv1.ClusterStatus_STATE_DELETING, clusterv1.ClusterStatus_REASON_UNSPECIFIED
		for _, kind := range []string{internalv1.ReadyCondition, internalv1.SyncedCondition} {
			if observed := condition(object, kind); observed != nil && observed.ObservedGeneration == object.GetGeneration() &&
				observed.Status == metav1.ConditionFalse && observed.Reason == "CreateOutcomeUnknown" {
				out.Reason, out.ObservedGeneration = clusterv1.ClusterStatus_REASON_DELETION_BLOCKED, observed.ObservedGeneration
				break
			}
		}
		return out
	}
	if ready == nil || ready.ObservedGeneration != object.GetGeneration() {
		return out
	}
	switch ready.Status {
	case metav1.ConditionTrue:
		out.State, out.Reason = clusterv1.ClusterStatus_STATE_READY, clusterv1.ClusterStatus_REASON_UNSPECIFIED
	case metav1.ConditionFalse:
		out.Reason = clusterv1.ClusterStatus_REASON_UNSPECIFIED
		for _, kind := range []string{internalv1.SyncedCondition, internalv1.ControlPlaneReadyCondition} {
			observed := condition(object, kind)
			if observed == nil || observed.ObservedGeneration != object.GetGeneration() || observed.Status != metav1.ConditionFalse {
				continue
			}
			if reason := failureReason(observed.Reason); reason != clusterv1.ClusterStatus_REASON_UNSPECIFIED {
				out.State, out.Reason = clusterv1.ClusterStatus_STATE_FAILED, reason
				break
			}
			if observed.Reason == "Provisioning" {
				out.Reason = clusterv1.ClusterStatus_REASON_PROVISIONING
			}
		}
	default:
		if ready.Status != metav1.ConditionUnknown || !awaitingInitialObservation(object, ready) {
			out.State, out.Reason = clusterv1.ClusterStatus_STATE_UNKNOWN, clusterv1.ClusterStatus_REASON_STATUS_UNAVAILABLE
		}
	}
	return out
}

// awaitingInitialObservation distinguishes initial pending state from unavailable status.
// It checks structured conditions without inspecting private messages.
func awaitingInitialObservation(object *unstructured.Unstructured, ready *metav1.Condition) bool {
	if ready.Reason != "ReadyUnknown" && ready.Reason != "NotYetReported" {
		return false
	}
	checkpoint, found, err := unstructured.NestedFieldNoCopy(object.Object, "status", "provisioning")
	if err != nil || (found && checkpoint != nil) {
		return false
	}
	for _, kind := range []string{internalv1.SyncedCondition, internalv1.ControlPlaneReadyCondition} {
		observed := condition(object, kind)
		if observed != nil && (observed.ObservedGeneration != object.GetGeneration() ||
			observed.Status != metav1.ConditionUnknown || observed.Reason != "NotYetReported") {
			return false
		}
	}
	return true
}

// failureReason allows only reviewed failure categories into public responses.
func failureReason(reason string) clusterv1.ClusterStatus_Reason {
	switch reason {
	case "CreateRejected":
		return clusterv1.ClusterStatus_REASON_CREATION_REJECTED
	case "UnsupportedConfiguration":
		return clusterv1.ClusterStatus_REASON_UNSUPPORTED_CONFIGURATION
	case "UnsupportedUpdate":
		return clusterv1.ClusterStatus_REASON_UNSUPPORTED_UPDATE
	case "ProvisioningFailed":
		return clusterv1.ClusterStatus_REASON_PROVISIONING_FAILED
	default:
		return clusterv1.ClusterStatus_REASON_UNSPECIFIED
	}
}

// condition reads lifecycle evidence without exposing the internal status structure.
func condition(object *unstructured.Unstructured, kind string) *metav1.Condition {
	value, _, err := unstructured.NestedFieldNoCopy(object.Object, "status", "conditions")
	conditions, ok := value.([]any)
	if err != nil || !ok {
		return nil
	}
	for _, entry := range conditions {
		fields, ok := entry.(map[string]any)
		if !ok || fields["type"] != kind {
			continue
		}
		state, _, _ := unstructured.NestedString(fields, "status")
		reason, _, _ := unstructured.NestedString(fields, "reason")
		generation, _, _ := unstructured.NestedInt64(fields, "observedGeneration")
		return &metav1.Condition{Status: metav1.ConditionStatus(state), Reason: reason, ObservedGeneration: generation}
	}
	return nil
}
