// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type TaintNodeInput struct {
	NodeID uuid.UUID

	TaintKey    string
	TaintValue  string
	TaintEffect string
}

type TaintNodeResult struct {
	Output string
}

func (a *Activities) TaintNode(ctx context.Context, input TaintNodeInput) (TaintNodeResult, error) {
	logger := activity.GetLogger(ctx)

	taint, remove, err := taintFromInput(input)
	if err != nil {
		return TaintNodeResult{}, err
	}

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return TaintNodeResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	cluster, err := a.GetClusterForNode(ctx, node.ID)
	if err != nil {
		return TaintNodeResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return TaintNodeResult{}, err
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var kubeNode corev1.Node
		if err := tenantClient.Get(ctx, client.ObjectKey{Name: node.Name}, &kubeNode); err != nil {
			return fmt.Errorf("failed to get Kubernetes node %s: %w", node.Name, err)
		}

		before := kubeNode.DeepCopy()
		if !updateNodeTaint(&kubeNode, taint, remove) {
			return nil
		}

		logger.Info("Updating node taint", "nodeID", input.NodeID, "nodeName", node.Name, "taint", taint, "remove", remove)
		patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
		return tenantClient.Patch(ctx, &kubeNode, patch)
	})
	if err != nil {
		return TaintNodeResult{}, fmt.Errorf("failed to update taint on Kubernetes node %s: %w", node.Name, err)
	}

	action := "applied to"
	if remove {
		action = "removed from"
	}
	taintSpec := fmt.Sprintf("%s:%s", taint.Key, taint.Effect)
	if taint.Value != "" {
		taintSpec = fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect)
	}
	return TaintNodeResult{Output: fmt.Sprintf("taint %s %s node/%s", taintSpec, action, node.Name)}, nil
}

func taintFromInput(input TaintNodeInput) (corev1.Taint, bool, error) {
	remove := strings.HasSuffix(input.TaintEffect, "-")
	effect := corev1.TaintEffect(strings.TrimSuffix(input.TaintEffect, "-"))
	if input.TaintKey == "" || input.TaintEffect == "" || (!remove && input.TaintValue == "") {
		return corev1.Taint{}, false, fmt.Errorf("taint key and effect are required, and value is required when applying a taint")
	}
	if errs := validation.IsQualifiedName(input.TaintKey); len(errs) > 0 {
		return corev1.Taint{}, false, fmt.Errorf("invalid taint key %q: %s", input.TaintKey, strings.Join(errs, "; "))
	}
	if errs := validation.IsValidLabelValue(input.TaintValue); len(errs) > 0 {
		return corev1.Taint{}, false, fmt.Errorf("invalid taint value %q: %s", input.TaintValue, strings.Join(errs, "; "))
	}

	switch effect {
	case corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
	default:
		return corev1.Taint{}, false, fmt.Errorf("invalid taint effect %q", input.TaintEffect)
	}

	return corev1.Taint{Key: input.TaintKey, Value: input.TaintValue, Effect: effect}, remove, nil
}

func updateNodeTaint(node *corev1.Node, taint corev1.Taint, remove bool) bool {
	if remove {
		taints := make([]corev1.Taint, 0, len(node.Spec.Taints))
		for _, existing := range node.Spec.Taints {
			if existing.Key == taint.Key && existing.Effect == taint.Effect {
				continue
			}
			taints = append(taints, existing)
		}
		if len(taints) == len(node.Spec.Taints) {
			return false
		}
		if len(taints) == 0 {
			taints = nil
		}
		node.Spec.Taints = taints
		return true
	}

	for index := range node.Spec.Taints {
		existing := node.Spec.Taints[index]
		if existing.Key != taint.Key || existing.Effect != taint.Effect {
			continue
		}

		if existing.Value == taint.Value {
			return false
		}
		node.Spec.Taints[index] = taint
		return true
	}

	node.Spec.Taints = append(node.Spec.Taints, taint)
	return true
}
