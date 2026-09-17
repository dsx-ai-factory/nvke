// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodestatus

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

func applyKubernetesNodeHealth(cweNode *db.Node, kubeNode *corev1.Node) {
	cweNode.Conditions.KubernetesReady = fromKubeCondition(cweNode.Conditions.KubernetesReady, kubeNode, string(corev1.NodeReady))
	cweNode.Conditions.KubernetesUnschedulable = kubernetesUnschedulableCondition(cweNode.Conditions.KubernetesUnschedulable, kubeNode)

	needsRecycle := fromKubeCondition(cweNode.Conditions.MarkedForRecycling, kubeNode, constants.NodeConditionTypeNeedsRecycle)
	if needsRecycle.Status == db.ConditionStatusTrue {
		cweNode.Conditions.MarkedForRecycling = needsRecycle
	}
	needsRepair := fromKubeCondition(cweNode.Conditions.MarkedForRepair, kubeNode, constants.NodeConditionTypeNeedsRepair)
	if needsRepair.Status == db.ConditionStatusTrue {
		cweNode.Conditions.MarkedForRepair = needsRepair
	}
}

// describeConditionChanges summarizes what differs between two condition sets,
// one short string per changed condition. This is what the update log line
// carries: a node has sixteen conditions, and rendering both sets in full to
// report that one of them moved costs kilobytes per line and still leaves the
// reader diffing two near-identical blobs by eye.
func describeConditionChanges(previous, current db.NodeConditions) []string {
	was, now := previous.All(), current.All()

	var changes []string
	for i := range now {
		switch {
		case was[i].Condition.Equal(now[i].Condition):
			continue
		case was[i].Condition.Status != now[i].Condition.Status:
			changes = append(changes, fmt.Sprintf("%s: %s -> %s", now[i].Name, was[i].Condition.Status, now[i].Condition.Status))
		default:
			changes = append(changes, fmt.Sprintf("%s: %s unchanged, message or transition time updated", now[i].Name, now[i].Condition.Status))
		}
	}
	return changes
}

func fromKubeCondition(existing db.Condition, kubeNode *corev1.Node, conditionType string) db.Condition {
	if kubeNode == nil {
		return updatedCondition(existing, db.ConditionStatusUnknown, "Node data not available")
	}

	for _, condition := range kubeNode.Status.Conditions {
		if string(condition.Type) == conditionType {
			return db.Condition{
				Status:  dbStatusFromKubeStatus(condition.Status),
				Message: condition.Message,
				// metav1.Time decodes into the local zone, so normalize to keep
				// what is persisted to JSONB in the same shape it is read back in.
				LastTransitionTime: condition.LastTransitionTime.Time.UTC(),
			}
		}
	}

	return updatedCondition(existing, db.ConditionStatusUnknown, fmt.Sprintf("Condition %s not found on node", conditionType))
}

func kubernetesUnschedulableCondition(existing db.Condition, kubeNode *corev1.Node) db.Condition {
	if kubeNode == nil {
		return updatedCondition(existing, db.ConditionStatusUnknown, "Node data not available")
	}

	if kubeNode.Spec.Unschedulable {
		return updatedCondition(existing, db.ConditionStatusTrue, "Node is unschedulable")
	}

	return updatedCondition(existing, db.ConditionStatusFalse, "Node is schedulable")
}

func updatedCondition(existing db.Condition, status db.ConditionStatus, message string) db.Condition {
	db.SetCondition(&existing, db.Condition{Status: status, Message: message})
	return existing
}

func dbStatusFromKubeStatus(kubeStatus corev1.ConditionStatus) db.ConditionStatus {
	switch kubeStatus {
	case corev1.ConditionTrue:
		return db.ConditionStatusTrue
	case corev1.ConditionFalse:
		return db.ConditionStatusFalse
	case corev1.ConditionUnknown:
		return db.ConditionStatusUnknown
	default:
		return db.ConditionStatusUnknown
	}
}
