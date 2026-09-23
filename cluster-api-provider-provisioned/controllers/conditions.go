// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/dsx-ai-factory/nke/cluster-api-provider-provisioned/api/v1alpha1"
)

// conditionSetter is met by both objects of this provider.
type conditionSetter interface {
	client.Object
	GetConditions() []metav1.Condition
	SetConditions(conditions []metav1.Condition)
}

// setCondition writes the Ready condition that the Cluster API contract reads.
func setCondition(obj conditionSetter, status metav1.ConditionStatus, reason, message string) {
	conditions := obj.GetConditions()
	condition := metav1.Condition{
		Type:               infrav1.ReadyCondition,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: obj.GetGeneration(),
	}

	for i := range conditions {
		if conditions[i].Type != infrav1.ReadyCondition {
			continue
		}
		condition.LastTransitionTime = conditions[i].LastTransitionTime
		if conditions[i].Status != status {
			condition.LastTransitionTime = metav1.Now()
		}
		conditions[i] = condition
		obj.SetConditions(conditions)
		return
	}

	condition.LastTransitionTime = metav1.Now()
	obj.SetConditions(append(conditions, condition))
}
