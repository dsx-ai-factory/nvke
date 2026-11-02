// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	drainedNodeLabel        = "dgx.nvidia.com/drained"
	mirrorPodAnnotation     = "kubernetes.io/config.mirror"
	podNodeNameField        = "spec.nodeName"
	drainPodDeletionTimeout = 100 * time.Second

	// drainPodPollInterval is the same as the eviction retry interval of kubectl drain
	drainPodPollInterval = 5 * time.Second

	// drainSkipWaitForDeleteTimeout is the maximum time that a drain waits for a
	// pod after its deletion timestamp. Same as kubectl drain
	drainSkipWaitForDeleteTimeout = 5 * time.Minute

	// drainErrorMaxPods is the maximum number of pods that a drain error shows.
	drainErrorMaxPods = 10

	drainKubeNodeConditionMessage = "Draining Kubernetes node"
)

// DrainKubeNodeInput is the input of DrainKubeNode. If ForceDrainOnFailure is
// true and the eviction of the pods does not complete, the activity deletes all
// pods on the node with no grace period.
type DrainKubeNodeInput struct {
	NodeID              uuid.UUID
	ForceDrainOnFailure bool
}

// DrainKubeNode cordons the node and evicts its pods. It shows its progress in
// the deleting condition of the node.
func (a *Activities) DrainKubeNode(ctx context.Context, input DrainKubeNodeInput) error {
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()

	updateDeletingCondition := a.nodeDeletingConditionMessageUpdater(input.NodeID)
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[struct{}]{
		Message: drainKubeNodeConditionMessage,
		UpdateCondition: func(ctx context.Context, message string) error {
			if err := updateDeletingCondition(ctx, message); err != nil {
				activity.GetLogger(ctx).Warn("Failed to update node deleting condition", "nodeID", input.NodeID, "error", err)
			}
			return nil
		},
		RenderError: messageForTemporalError,
		ActivityBody: func() (struct{}, error) {
			return struct{}{}, a.drainKubeNode(ctx, input, drainPodPollInterval, drainPodDeletionTimeout)
		},
	}

	_, err := conditionUpdateWrapper.Run(ctx)
	return err
}

func (a *Activities) drainKubeNode(
	ctx context.Context,
	input DrainKubeNodeInput,
	pollInterval time.Duration,
	timeout time.Duration,
) error {
	logger := activity.GetLogger(ctx)

	node, err := a.db.GetNode(ctx, input.NodeID)
	if errors.Is(err, db.ErrNotFound) {
		return nonRetryableIfNotFound("failed to get node", err)
	}
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	cluster, err := a.db.GetCluster(ctx, node.ClusterID)
	if errors.Is(err, db.ErrNotFound) {
		return nonRetryableIfNotFound("failed to get cluster", err)
	}
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		logger.Info("Cluster is deleting, nothing to drain", "nodeID", input.NodeID, "clusterID", cluster.ID)
		return nil
	}

	tenantClient, err := a.tenantClientForCluster(ctx, cluster)
	if err != nil {
		return fmt.Errorf("failed to drain Kubernetes node %s: %w", node.Name, err)
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
		return fmt.Errorf("failed to drain Kubernetes node %s: %w", node.Name, err)
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
		return fmt.Errorf("failed to drain Kubernetes node %s: %w", node.Name, err)
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
		return fmt.Errorf("failed to cordon the node: %w", err)
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
	logger := activity.GetLogger(ctx)

	stopped := func(err error) error {
		if ctx.Err() != nil {
			return fmt.Errorf("drain stopped: %w", ctx.Err())
		}
		return err
	}

	pods, err := listKubeNodePods(ctx, tenantClient, nodeName)
	if err != nil {
		return stopped(fmt.Errorf("failed to find the pods to evict: %w", err))
	}

	draining := podsEligibleForEviction(pods)
	for i := range draining {
		if metav1.GetControllerOf(&draining[i]) == nil {
			logger.Warn("Drain evicts a pod that has no controller",
				"nodeName", nodeName, "pod", client.ObjectKeyFromObject(&draining[i]).String())
		}
	}
	evictionErrors := make(map[client.ObjectKey]error)

	deadline := time.Now().Add(timeout)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for len(draining) > 0 {
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return stopped(fmt.Errorf("timed out after %s waiting for pods to leave: %s: %w",
				timeout, describeDrainingPods(draining, evictionErrors), context.DeadlineExceeded))
		}
		activity.RecordHeartbeat(ctx)

		evictPods(ctx, tenantClient, draining, evictionErrors, deadline)

		select {
		case <-waitCtx.Done():
		case <-ticker.C:
		}

		pods, err := listKubeNodePods(ctx, tenantClient, nodeName)
		if err != nil {
			return stopped(fmt.Errorf("failed to check the pods that left: %w", err))
		}
		stillOnNode, pastDeletion := podsStillOnNode(draining, pods, time.Now())
		for i := range pastDeletion {
			logger.Warn("Drain does not wait for a pod that is more than 5m past its deletion timestamp",
				"nodeName", nodeName, "pod", client.ObjectKeyFromObject(&pastDeletion[i]).String(),
				"deletionTimestamp", pastDeletion[i].DeletionTimestamp, "skipWaitTimeout", drainSkipWaitForDeleteTimeout)
		}
		draining = stillOnNode
		if err := failedEvictions(draining, evictionErrors); err != nil {
			return stopped(fmt.Errorf("failed to evict pods: %w", err))
		}
	}

	return nil
}

// podsEligibleForEviction returns the pods that a drain evicts. It does not
// return mirror pods, pods of an apps/v1 DaemonSet, or pods in the phase
// Succeeded or Failed.
func podsEligibleForEviction(pods []corev1.Pod) []corev1.Pod {
	result := make([]corev1.Pod, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		if _, mirrorPod := pod.Annotations[mirrorPodAnnotation]; mirrorPod {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if isDaemonSetPod(pod) {
			continue
		}

		result = append(result, *pod)
	}

	return result
}

// isDaemonSetPod reports whether an apps/v1 DaemonSet controls the pod.
func isDaemonSetPod(pod *corev1.Pod) bool {
	controller := metav1.GetControllerOf(pod)
	apiVersion, kind := appsv1.SchemeGroupVersion.WithKind("DaemonSet").ToAPIVersionAndKind()
	return controller != nil && controller.APIVersion == apiVersion && controller.Kind == kind
}

// evictPods evicts each pod that has no deletion timestamp. The eviction uses the
// grace period of the pod.
func evictPods(
	ctx context.Context,
	tenantClient client.Client,
	pods []corev1.Pod,
	evictionErrors map[client.ObjectKey]error,
	deadline time.Time,
) {
	for i := range pods {
		if !time.Now().Before(deadline) {
			return
		}
		pod := &pods[i]
		key := client.ObjectKeyFromObject(pod)
		if pod.DeletionTimestamp != nil {
			continue
		}
		if err, ok := evictionErrors[key]; ok && !isEvictionRefusedTemporarily(err) {
			continue
		}

		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: pod.Namespace,
				Name:      pod.Name,
			},
		}
		if err := tenantClient.SubResource("eviction").Create(ctx, pod, eviction); err != nil && !apierrors.IsNotFound(err) {
			evictionErrors[key] = err
			continue
		}
		delete(evictionErrors, key)
	}
}

// isEvictionRefusedTemporarily reports whether the API server refused the
// eviction temporarily.
func isEvictionRefusedTemporarily(err error) bool {
	switch {
	case apierrors.IsTooManyRequests(err):
		return true
	case apierrors.IsForbidden(err) && apierrors.HasStatusCause(err, policyv1.DisruptionBudgetCause):
		return true
	case apierrors.IsForbidden(err) && apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause):
		return true
	default:
		return false
	}
}

// failedEvictions returns an error that shows a maximum of drainErrorMaxPods
// failed evictions.
func failedEvictions(pods []corev1.Pod, evictionErrors map[client.ObjectKey]error) error {
	errs := make([]error, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		err, ok := evictionErrors[client.ObjectKeyFromObject(pod)]
		if !ok || pod.DeletionTimestamp != nil || isEvictionRefusedTemporarily(err) {
			return nil
		}
		errs = append(errs, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err))
	}
	if len(errs) > drainErrorMaxPods {
		errs = append(errs[:drainErrorMaxPods], fmt.Errorf("and %d more", len(errs)-drainErrorMaxPods))
	}

	return utilerrors.NewAggregate(errs)
}

// podsStillOnNode returns each pod in pods that is also in draining, with the
// same UID. It returns separately the pods that are more than
// drainSkipWaitForDeleteTimeout after their deletion timestamp.
func podsStillOnNode(draining, pods []corev1.Pod, now time.Time) ([]corev1.Pod, []corev1.Pod) {
	uids := make(map[client.ObjectKey]types.UID, len(draining))
	for i := range draining {
		uids[client.ObjectKeyFromObject(&draining[i])] = draining[i].UID
	}

	remaining := make([]corev1.Pod, 0, len(draining))
	var pastDeletion []corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if uid, ok := uids[client.ObjectKeyFromObject(pod)]; !ok || uid != pod.UID {
			continue
		}
		if pod.DeletionTimestamp != nil && now.Sub(pod.DeletionTimestamp.Time) > drainSkipWaitForDeleteTimeout {
			pastDeletion = append(pastDeletion, *pod)
			continue
		}
		remaining = append(remaining, *pod)
	}

	return remaining, pastDeletion
}

// describeDrainingPods returns the pods that the drain waits for, each with its
// deletion timestamp or with the reason for its last eviction error.
func describeDrainingPods(pods []corev1.Pod, evictionErrors map[client.ObjectKey]error) string {
	descriptions := make([]string, 0, min(len(pods), drainErrorMaxPods)+1)
	for i := range pods {
		if i == drainErrorMaxPods {
			descriptions = append(descriptions, fmt.Sprintf("and %d more", len(pods)-i))
			break
		}

		pod := &pods[i]
		description := pod.Namespace + "/" + pod.Name
		err, hasErr := evictionErrors[client.ObjectKeyFromObject(pod)]
		switch {
		case pod.DeletionTimestamp != nil:
			description += " (deletion timestamp " + pod.DeletionTimestamp.UTC().Format(time.RFC3339) + ")"
		case hasErr && isEvictionRefusedTemporarily(err):
			description += " (eviction refused: " + evictionRefusalReason(err) + ")"
		case hasErr:
			description += " (eviction failed: " + err.Error() + ")"
		}
		descriptions = append(descriptions, description)
	}

	return strings.Join(descriptions, ", ")
}

// evictionRefusalReason returns the reason for a refused eviction. For a
// PodDisruptionBudget, the reason gives the name of the budget.
func evictionRefusalReason(err error) string {
	if cause, ok := apierrors.StatusCause(err, policyv1.DisruptionBudgetCause); ok {
		return cause.Message
	}

	return err.Error()
}

func forceDeleteKubeNodePods(ctx context.Context, tenantClient client.Client, nodeName string) error {
	pods, err := listKubeNodePods(ctx, tenantClient, nodeName)
	if err != nil {
		return fmt.Errorf("failed to find the pods to force delete: %w", err)
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
		return nil, fmt.Errorf("failed to list pods: %w", err)
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
		return fmt.Errorf("failed to label the node as drained: %w", err)
	}

	return nil
}
