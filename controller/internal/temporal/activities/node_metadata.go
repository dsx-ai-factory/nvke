// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

// ApplyNodeMetadataInput identifies the node whose desired labels and taints are applied.
type ApplyNodeMetadataInput struct {
	NodeID uuid.UUID

	NodeProvisioningConditionMessage string
}

// ApplyNodeMetadataResult describes what ApplyNodeMetadata applied.
type ApplyNodeMetadataResult struct {
	Output string
}

// ApplyNodeMetadata applies the labels and taints requested through the API to the node's Kubernetes Node.
// Workers take them from their node pool. Control-plane nodes take labels from their control-plane configuration
// and never take taints. Labels and taints already on the node that are not requested are left unchanged.
func (a *Activities) ApplyNodeMetadata(ctx context.Context, input ApplyNodeMetadataInput) (ApplyNodeMetadataResult, error) {
	var updateCondition func(ctx context.Context, message string) error
	if input.NodeProvisioningConditionMessage != "" {
		updateCondition = a.nodeProvisioningConditionMessageUpdater(input.NodeID)
	}

	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[ApplyNodeMetadataResult]{
		Message:         input.NodeProvisioningConditionMessage,
		UpdateCondition: updateCondition,
		RenderError:     messageForTemporalError,
		ActivityBody: func() (ApplyNodeMetadataResult, error) {
			return a.applyNodeMetadata(ctx, input)
		},
	}

	return conditionUpdateWrapper.Run(ctx)
}

func (a *Activities) applyNodeMetadata(ctx context.Context, input ApplyNodeMetadataInput) (ApplyNodeMetadataResult, error) {
	logger := activity.GetLogger(ctx)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return ApplyNodeMetadataResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	// For a control-plane node without a node pool, this returns a synthetic nodePool built from the node's pinned control-plane configuration.
	_, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return ApplyNodeMetadataResult{}, fmt.Errorf("failed to get node context: %w", err)
	}

	labels := nodePool.NodeLabels
	var taints []corev1.Taint
	if !node.ControlPlane {
		for _, taint := range nodePool.NodeTaints {
			taints = append(taints, corev1.Taint{Key: taint.Key, Value: taint.Value, Effect: corev1.TaintEffect(taint.Effect)})
		}
	}
	if len(labels) == 0 && len(taints) == 0 {
		return ApplyNodeMetadataResult{Output: fmt.Sprintf("node/%s has no labels or taints to apply", node.Name)}, nil
	}

	cluster, err := a.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return ApplyNodeMetadataResult{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return ApplyNodeMetadataResult{}, err
	}

	labelChanges, err := patchNodeLabels(ctx, tenantClient, node.Name, labels)
	if err != nil {
		return ApplyNodeMetadataResult{}, nodeMetadataPatchError("labels", node.Name, err)
	}
	taintChanges, err := patchNodeTaints(ctx, tenantClient, node.Name, taints)
	if err != nil {
		return ApplyNodeMetadataResult{}, nodeMetadataPatchError("taints", node.Name, err)
	}

	summary := summarizeNodeMetadata(node.Name, labelChanges, taintChanges)
	logger.Info("Applied node metadata", "nodeID", input.NodeID, "nodeName", node.Name, "summary", summary)
	return ApplyNodeMetadataResult{Output: summary}, nil
}

// labelUpdate is a label value the patch replaced.
type labelUpdate struct {
	Before string
	After  string
}

// taintUpdate is a taint the patch replaced, matched by key and effect.
type taintUpdate struct {
	Before corev1.Taint
	After  corev1.Taint
}

// nodeLabelChanges sorts the requested labels by what applying them did to the node.
type nodeLabelChanges struct {
	Added     map[string]string
	Updated   map[string]labelUpdate
	Unchanged map[string]string
}

// nodeTaintChanges sorts the requested taints by what applying them did to the node.
type nodeTaintChanges struct {
	Added     []corev1.Taint
	Updated   []taintUpdate
	Unchanged []corev1.Taint
}

// summarizeNodeMetadata describes which labels and taints were added, updated, or already on the node,
// omitting any empty group, for example
// "node/worker-0: labels added [team=ml], labels updated [nodeGroup=worker->gpu], taints unchanged [gpu=true:NoSchedule]".
func summarizeNodeMetadata(nodeName string, labels nodeLabelChanges, taints nodeTaintChanges) string {
	var summaryItems []string
	if len(labels.Added) > 0 {
		summaryItems = append(summaryItems, fmt.Sprintf("labels added [%s]", klabels.Set(labels.Added)))
	}
	if len(labels.Updated) > 0 {
		updated := make([]string, 0, len(labels.Updated))
		for _, key := range slices.Sorted(maps.Keys(labels.Updated)) {
			update := labels.Updated[key]
			updated = append(updated, fmt.Sprintf("%s=%s->%s", key, update.Before, update.After))
		}
		summaryItems = append(summaryItems, fmt.Sprintf("labels updated [%s]", strings.Join(updated, ",")))
	}
	if len(labels.Unchanged) > 0 {
		summaryItems = append(summaryItems, fmt.Sprintf("labels unchanged [%s]", klabels.Set(labels.Unchanged)))
	}
	if len(taints.Added) > 0 {
		summaryItems = append(summaryItems, fmt.Sprintf("taints added [%s]", formatTaints(taints.Added)))
	}
	if len(taints.Updated) > 0 {
		updated := make([]string, 0, len(taints.Updated))
		for _, update := range taints.Updated {
			updated = append(updated, fmt.Sprintf("%s=%s->%s:%s", update.After.Key, update.Before.Value, update.After.Value, update.After.Effect))
		}
		summaryItems = append(summaryItems, fmt.Sprintf("taints updated [%s]", strings.Join(updated, ",")))
	}
	if len(taints.Unchanged) > 0 {
		summaryItems = append(summaryItems, fmt.Sprintf("taints unchanged [%s]", formatTaints(taints.Unchanged)))
	}
	return fmt.Sprintf("node/%s: %s", nodeName, strings.Join(summaryItems, ", "))
}

func formatTaints(taints []corev1.Taint) string {
	taintStrings := make([]string, 0, len(taints))
	for _, taint := range taints {
		taintStrings = append(taintStrings, taint.ToString())
	}
	return strings.Join(taintStrings, ",")
}

// patchNodeLabels merges labels into the node's labels and reports which were added, updated, or already set.
// The merge patch carries only the changed keys, so it needs no optimistic lock to keep labels written
// concurrently by others.
func patchNodeLabels(ctx context.Context, tenantClient client.Client, nodeName string, labels map[string]string) (nodeLabelChanges, error) {
	if len(labels) == 0 {
		return nodeLabelChanges{}, nil
	}

	var kubeNode corev1.Node
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: nodeName}, &kubeNode); err != nil {
		return nodeLabelChanges{}, fmt.Errorf("failed to get Kubernetes node %s: %w", nodeName, err)
	}
	before := kubeNode.DeepCopy()
	changes := setNodeLabels(&kubeNode, labels)
	if len(changes.Added) == 0 && len(changes.Updated) == 0 {
		return changes, nil
	}
	if err := tenantClient.Patch(ctx, &kubeNode, client.MergeFrom(before)); err != nil {
		return nodeLabelChanges{}, err
	}
	return changes, nil
}

// patchNodeTaints adds or updates taints on the node and reports which were added, updated, or already set.
// A merge patch replaces the whole taint list, so it uses an optimistic lock and retries on conflict
// to avoid dropping taints written concurrently.
func patchNodeTaints(ctx context.Context, tenantClient client.Client, nodeName string, taints []corev1.Taint) (nodeTaintChanges, error) {
	if len(taints) == 0 {
		return nodeTaintChanges{}, nil
	}

	var changes nodeTaintChanges
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		changes = nodeTaintChanges{}
		var kubeNode corev1.Node
		if err := tenantClient.Get(ctx, client.ObjectKey{Name: nodeName}, &kubeNode); err != nil {
			return fmt.Errorf("failed to get Kubernetes node %s: %w", nodeName, err)
		}
		before := kubeNode.DeepCopy()
		for _, taint := range taints {
			current, found := findNodeTaint(&kubeNode, taint)
			switch {
			case !found:
				kubeNode.Spec.Taints = append(kubeNode.Spec.Taints, taint)
				changes.Added = append(changes.Added, taint)
			case current.Value == taint.Value:
				changes.Unchanged = append(changes.Unchanged, taint)
			default:
				changes.Updated = append(changes.Updated, taintUpdate{Before: current, After: taint})
				replaceNodeTaint(&kubeNode, taint)
			}
		}
		if len(changes.Added) == 0 && len(changes.Updated) == 0 {
			return nil
		}
		patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
		return tenantClient.Patch(ctx, &kubeNode, patch)
	})
	if err != nil {
		return nodeTaintChanges{}, err
	}
	return changes, nil
}

// findNodeTaint returns the node's taint with the same key and effect as taint, and whether the node has one.
func findNodeTaint(node *corev1.Node, taint corev1.Taint) (corev1.Taint, bool) {
	for _, existing := range node.Spec.Taints {
		if existing.Key == taint.Key && existing.Effect == taint.Effect {
			return existing, true
		}
	}
	return corev1.Taint{}, false
}

// replaceNodeTaint replaces the node's taint with the same key and effect as taint.
func replaceNodeTaint(node *corev1.Node, taint corev1.Taint) {
	for i, existing := range node.Spec.Taints {
		if existing.Key == taint.Key && existing.Effect == taint.Effect {
			node.Spec.Taints[i] = taint
			return
		}
	}
}

// nodeMetadataPatchError wraps a failed patch. When the Kubernetes API rejects the stored labels or taints
// as invalid, retrying cannot succeed, so the error is non-retryable. The Kubernetes error is kept as the cause,
// which the node's provisioning condition appends to the message.
func nodeMetadataPatchError(kind, nodeName string, err error) error {
	message := fmt.Sprintf("failed to apply %s to Kubernetes node %s", kind, nodeName)
	if apierrors.IsInvalid(err) {
		return temporal.NewNonRetryableApplicationError(message, "InvalidNodeMetadata", err)
	}
	return fmt.Errorf("%s: %w", message, err)
}

func setNodeLabels(node *corev1.Node, labels map[string]string) nodeLabelChanges {
	changes := nodeLabelChanges{
		Added:     map[string]string{},
		Updated:   map[string]labelUpdate{},
		Unchanged: map[string]string{},
	}
	for key, value := range labels {
		current, ok := node.Labels[key]
		switch {
		case ok && current == value:
			changes.Unchanged[key] = value
			continue
		case ok:
			changes.Updated[key] = labelUpdate{Before: current, After: value}
		default:
			changes.Added[key] = value
		}
		if node.Labels == nil {
			node.Labels = make(map[string]string, len(labels))
		}
		node.Labels[key] = value
	}
	return changes
}
