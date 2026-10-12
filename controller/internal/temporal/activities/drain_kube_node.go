// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	drainedNodeLabel        = "dgx.nvidia.com/drained"
	mirrorPodAnnotation     = "kubernetes.io/config.mirror"
	podNodeNameField        = "spec.nodeName"
	drainGracePeriodSeconds = int64(60)
	drainPodPollInterval    = time.Second
	drainPodDeletionTimeout = 100 * time.Second
)

type DrainKubeNodeInput struct {
	NodeID              uuid.UUID
	ForceDrainOnFailure bool
}

func (a *Activities) DrainKubeNode(ctx context.Context, input DrainKubeNodeInput) error {
	return a.drainKubeNode(ctx, input, drainPodPollInterval, drainPodDeletionTimeout)
}

func (a *Activities) drainKubeNode(
	ctx context.Context,
	input DrainKubeNodeInput,
	pollInterval time.Duration,
	timeout time.Duration,
) error {
	logger := activity.GetLogger(ctx)
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()

	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return nonRetryableIfNotFound("failed to get node", err)
	}

	cluster, err := a.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return nonRetryableIfNotFound("failed to get cluster", err)
	}

	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		logger.Info("Cluster is deleting, nothing to drain", "nodeID", input.NodeID, "clusterID", cluster.ID)
		return nil
	}

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return err
	}

	var kubeNode corev1.Node
	if err := tenantClient.Get(ctx, client.ObjectKey{Name: node.Name}, &kubeNode); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Kubernetes node does not exist, nothing to drain", "nodeID", input.NodeID, "nodeName", node.Name)
			return nil
		}
		return fmt.Errorf("failed to get Kubernetes node %s: %w", node.Name, err)
	}

	if err := cordonKubeNode(ctx, tenantClient, &kubeNode); err != nil {
		return err
	}

	logger.Info("Draining Kubernetes node", "nodeID", input.NodeID, "nodeName", node.Name)
	if err := drainKubeNodePods(ctx, tenantClient, node.Name, pollInterval, timeout); err != nil {
		if !input.ForceDrainOnFailure {
			return fmt.Errorf("failed to drain Kubernetes node %s: %w", node.Name, err)
		}

		logger.Warn("Graceful drain failed, force deleting remaining pods", "nodeID", input.NodeID, "nodeName", node.Name, "error", err)
		if err := forceDeleteKubeNodePods(ctx, tenantClient, node.Name); err != nil {
			return fmt.Errorf("failed to force delete pods from Kubernetes node %s: %w", node.Name, err)
		}
	}

	if err := labelDrainedKubeNode(ctx, tenantClient, &kubeNode); err != nil {
		return err
	}

	logger.Info("Kubernetes node drained successfully", "nodeID", input.NodeID, "nodeName", node.Name)
	return nil
}

func cordonKubeNode(ctx context.Context, tenantClient client.Client, node *corev1.Node) error {
	if node.Spec.Unschedulable {
		return nil
	}

	before := node.DeepCopy()
	node.Spec.Unschedulable = true
	if err := tenantClient.Patch(ctx, node, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("failed to cordon Kubernetes node %s: %w", node.Name, err)
	}

	return nil
}

func drainKubeNodePods(
	ctx context.Context,
	tenantClient client.Client,
	nodeName string,
	pollInterval time.Duration,
	timeout time.Duration,
) error {
	drainCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		activity.RecordHeartbeat(ctx)

		pods, err := listKubeNodePods(drainCtx, tenantClient, nodeName)
		if err != nil {
			return err
		}

		pods, err = podsEligibleForEviction(pods)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			return nil
		}

		for i := range pods {
			pod := &pods[i]
			if pod.DeletionTimestamp != nil {
				continue
			}

			eviction := &policyv1.Eviction{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: pod.Namespace,
					Name:      pod.Name,
				},
				DeleteOptions: &metav1.DeleteOptions{
					GracePeriodSeconds: ptr.To(drainGracePeriodSeconds),
				},
			}
			if err := tenantClient.SubResource("eviction").Create(drainCtx, pod, eviction); err != nil {
				if apierrors.IsNotFound(err) || apierrors.IsTooManyRequests(err) {
					continue
				}
				return fmt.Errorf("failed to evict pod %s/%s: %w", pod.Namespace, pod.Name, err)
			}
		}

		select {
		case <-drainCtx.Done():
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("context canceled while draining Kubernetes node %s: %w", nodeName, err)
			}
			return fmt.Errorf("timed out after %s waiting for pods to leave Kubernetes node %s", timeout, nodeName)
		case <-ticker.C:
		}
	}
}

func podsEligibleForEviction(pods []corev1.Pod) ([]corev1.Pod, error) {
	result := make([]corev1.Pod, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		if _, mirrorPod := pod.Annotations[mirrorPodAnnotation]; mirrorPod {
			continue
		}

		controller := metav1.GetControllerOf(pod)
		if controller != nil && controller.Kind == "DaemonSet" {
			continue
		}
		if controller == nil && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			return nil, fmt.Errorf("cannot evict pod %s/%s: pod is not managed by a controller", pod.Namespace, pod.Name)
		}

		result = append(result, *pod)
	}

	return result, nil
}

func forceDeleteKubeNodePods(ctx context.Context, tenantClient client.Client, nodeName string) error {
	pods, err := listKubeNodePods(ctx, tenantClient, nodeName)
	if err != nil {
		return err
	}

	for i := range pods {
		activity.RecordHeartbeat(ctx)
		pod := &pods[i]
		if err := tenantClient.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	return nil
}

func listKubeNodePods(ctx context.Context, tenantClient client.Client, nodeName string) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := tenantClient.List(ctx, &pods, client.MatchingFields{podNodeNameField: nodeName}); err != nil {
		return nil, fmt.Errorf("failed to list pods on Kubernetes node %s: %w", nodeName, err)
	}

	return pods.Items, nil
}

func labelDrainedKubeNode(ctx context.Context, tenantClient client.Client, node *corev1.Node) error {
	if node.Labels[drainedNodeLabel] == "true" {
		return nil
	}

	before := node.DeepCopy()
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	node.Labels[drainedNodeLabel] = "true"
	if err := tenantClient.Patch(ctx, node, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("failed to label Kubernetes node %s as drained: %w", node.Name, err)
	}

	return nil
}
