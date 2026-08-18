// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodehealth

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type nodeAgentProbe struct {
	agentVersion string
	err          error
}

// applyNodeHealth derives a node's conditions from its live Kubernetes Node
// object and agent probe. It is the success path: kubeNode may still be
// nil for a CWE node that isn't registered in Kubernetes yet, and agentProbe.err
// reflects a per-node reachability failure; both are normal per-node states.
func applyNodeHealth(cweNode *db.Node, kubeNode *corev1.Node, agentProbe nodeAgentProbe) {
	cweNode.Conditions.KubernetesReady = fromKubeCondition(cweNode.Conditions.KubernetesReady, kubeNode, string(corev1.NodeReady))
	cweNode.Conditions.NodeAgentReady = nodeAgentCondition(cweNode.Conditions.NodeAgentReady, agentProbe)
	cweNode.Conditions.KubernetesUnschedulable = kubernetesUnschedulableCondition(cweNode.Conditions.KubernetesUnschedulable, kubeNode)

	// NeedsRecycle and NeedsRepair use a "ratcheting" sync: we set CWE NodeCondition to true when
	// K8s NodeCondition is true, but never clear the CWE NodeCondition when the K8s NodeCondition is false.
	// Because of the asynchronous nature of setting the K8s NodeCondition via a Temporal workflow, we need to
	// ensure that the CWE NodeCondition is not cleared by health check sync just because it doesn't exist (yet)
	// on the K8s node. Which could result in the auto-mode recycling algorithm attempting to recycle
	// too many nodes and exceed its recycle budget.
	// The trade-off is that manually removing a K8s NodeCondition via kubectl won't sync back to CWE,
	// and likely require manual intervention to finish recycling. This is not expected to be a common use-case.
	needsRecycle := fromKubeCondition(cweNode.Conditions.MarkedForRecycling, kubeNode, constants.NodeConditionTypeNeedsRecycle)
	if needsRecycle.Status == db.ConditionStatusTrue {
		cweNode.Conditions.MarkedForRecycling = needsRecycle
	}
	needsRepair := fromKubeCondition(cweNode.Conditions.MarkedForRepair, kubeNode, constants.NodeConditionTypeNeedsRepair)
	if needsRepair.Status == db.ConditionStatusTrue {
		cweNode.Conditions.MarkedForRepair = needsRepair
	}
}

// markNodeHealthUnknown is the degraded path: when the cluster could not be
// read, node health conditions are set to Unknown with the cause. Ratcheted
// MarkedForRecycling/MarkedForRepair conditions are intentionally left untouched.
func markNodeHealthUnknown(cweNode *db.Node, message string) {
	cweNode.Conditions.KubernetesReady = updatedCondition(cweNode.Conditions.KubernetesReady, db.ConditionStatusUnknown, message)
	cweNode.Conditions.NodeAgentReady = updatedCondition(cweNode.Conditions.NodeAgentReady, db.ConditionStatusUnknown, message)
	cweNode.Conditions.KubernetesUnschedulable = updatedCondition(cweNode.Conditions.KubernetesUnschedulable, db.ConditionStatusUnknown, message)
}

func fromKubeCondition(existing db.Condition, kubeNode *corev1.Node, conditionType string) db.Condition {
	if kubeNode == nil {
		return updatedCondition(existing, db.ConditionStatusUnknown, "Node data not available")
	}

	for _, condition := range kubeNode.Status.Conditions {
		if string(condition.Type) == conditionType {
			return db.Condition{
				Status:             dbStatusFromKubeStatus(condition.Status),
				Message:            condition.Message,
				LastTransitionTime: condition.LastTransitionTime.Time,
			}
		}
	}

	return updatedCondition(existing, db.ConditionStatusUnknown, fmt.Sprintf("Condition %s not found on node", conditionType))
}

func nodeAgentCondition(existing db.Condition, agentProbe nodeAgentProbe) db.Condition {
	if agentProbe.err != nil {
		return updatedCondition(existing, db.ConditionStatusFalse, agentProbe.err.Error())
	}

	return updatedCondition(existing, db.ConditionStatusTrue, "Node agent is reachable")
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
