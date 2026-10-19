// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

type LabelNodeInput struct {
	NodeID uuid.UUID

	// Optional labels to apply to the node.
	// Labels should be in `key=value` format or `key-` (trailing dash to negate or delete label)
	Labels []string

	NodeProvisioningConditionMessage string
	NodeRebootingConditionMessage    string
}

type LabelNodeResult struct {
	Output string
}

func (a *Activities) LabelNode(ctx context.Context, input LabelNodeInput) (LabelNodeResult, error) {
	var conditionMessage string
	var updateCondition func(ctx context.Context, message string) error

	if input.NodeProvisioningConditionMessage != "" {
		conditionMessage = input.NodeProvisioningConditionMessage
		updateCondition = a.nodeProvisioningConditionMessageUpdater(input.NodeID)
	} else if input.NodeRebootingConditionMessage != "" {
		conditionMessage = input.NodeRebootingConditionMessage
		updateCondition = a.nodeRebootingConditionMessageUpdater(input.NodeID)
	}

	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[LabelNodeResult]{
		Message:         conditionMessage,
		UpdateCondition: updateCondition,
		RenderError:     messageForTemporalError,
		ActivityBody: func() (LabelNodeResult, error) {
			return a.labelNode(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) labelNode(ctx context.Context, input LabelNodeInput) (LabelNodeResult, error) {
	logger := activity.GetLogger(ctx)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return LabelNodeResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	_, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return LabelNodeResult{}, fmt.Errorf("failed to get node context: %w", err)
	}

	labels := input.Labels
	if len(labels) == 0 {
		logger.Info("No labels to apply to node", "nodeID", input.NodeID, "nodeName", node.Name)
		return LabelNodeResult{Output: fmt.Sprintf("node/%s has no labels to apply", node.Name)}, nil
	}

	logger.Info("Labeling node", "nodeID", input.NodeID, "labels", labels)
	labelChanges, err := parseNodeLabelChanges(labels)
	if err != nil {
		return LabelNodeResult{}, err
	}

	cluster, err := a.db.GetCluster(ctx, nodePool.ClusterID)
	if err != nil {
		return LabelNodeResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return LabelNodeResult{}, err
	}

	var kubeNode corev1.Node
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: node.Name}, &kubeNode); err != nil {
		return LabelNodeResult{}, fmt.Errorf("failed to get Kubernetes node %s: %w", node.Name, err)
	}

	before := kubeNode.DeepCopy()
	applyNodeLabelChanges(&kubeNode, labelChanges)
	if err := tenantClient.Patch(ctx, &kubeNode, client.MergeFrom(before)); err != nil {
		return LabelNodeResult{}, fmt.Errorf("failed to label Kubernetes node %s: %w", node.Name, err)
	}

	return LabelNodeResult{Output: fmt.Sprintf("node/%s labeled", node.Name)}, nil
}

type nodeLabelChange struct {
	key    string
	value  string
	remove bool
}

func parseNodeLabelChanges(labels []string) ([]nodeLabelChange, error) {
	changes := make([]nodeLabelChange, 0, len(labels))
	for _, label := range labels {
		if !strings.Contains(label, "=") && strings.HasSuffix(label, "-") {
			key := strings.TrimSuffix(label, "-")
			if errs := validation.IsQualifiedName(key); len(errs) > 0 {
				return nil, fmt.Errorf("invalid label %q: %s", label, strings.Join(errs, "; "))
			}
			changes = append(changes, nodeLabelChange{key: key, remove: true})
			continue
		}

		key, value, ok := strings.Cut(label, "=")
		if !ok {
			return nil, fmt.Errorf("invalid label %q: expected key=value or key-", label)
		}
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return nil, fmt.Errorf("invalid label %q: %s", label, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			return nil, fmt.Errorf("invalid label %q: %s", label, strings.Join(errs, "; "))
		}
		changes = append(changes, nodeLabelChange{key: key, value: value})
	}
	return changes, nil
}

func applyNodeLabelChanges(node *corev1.Node, changes []nodeLabelChange) {
	for _, change := range changes {
		if change.remove {
			delete(node.Labels, change.key)
			continue
		}
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		node.Labels[change.key] = change.value
	}
}
