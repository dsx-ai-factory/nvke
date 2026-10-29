// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// KubectlImage is an alpine-based kubectl image carrying sh, wget (TLS),
	// tar, sha256sum and kubectl.
	KubectlImage = "alpine/k8s:1.34.1@sha256:ec714df3813b5405292860f8a1c55c5727bf8c33c88992f1e981efad8065547f"

	// PodLogTailLines is how many lines of a failed container's log
	// RunPodWithLogs appends to its error.
	PodLogTailLines = 40

	// PodEventLimit is how many of a pod's Warning events whyNotRun reports.
	PodEventLimit = 5
)

// RunPod creates a one-shot verifier pod, waits for it to finish, and deletes it
// after success. Failed pods are left behind for inspection.
func RunPod(ctx context.Context, c client.Client, pod *corev1.Pod, containerName string, timeout time.Duration) (*corev1.Pod, string, error) {
	return RunPodWithLogs(ctx, c, nil, pod, containerName, timeout)
}

// RunPodWithLogs is RunPod for a caller that also has a clientset: when the pod
// fails, the last PodLogTailLines lines of the container's log follow the
// termination message in the error, so a script that wrote the reason only to
// stderr still says why. A nil clientset reads no log.
func RunPodWithLogs(ctx context.Context, c client.Client, clientset kubernetes.Interface, pod *corev1.Pod, containerName string, timeout time.Duration) (*corev1.Pod, string, error) {
	if err := c.Create(ctx, pod); err != nil {
		return nil, "", fmt.Errorf("creating verifier pod in %s: %w", pod.Namespace, err)
	}

	keep := true
	defer func() {
		if keep {
			return
		}
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = c.Delete(delCtx, pod)
	}()

	finished, err := WaitForPod(ctx, c, client.ObjectKeyFromObject(pod), timeout)
	if err != nil {
		return nil, "", err
	}

	message := TerminatedMessage(finished, containerName)
	if finished.Status.Phase == corev1.PodFailed {
		summary := containerFailureSummary(finished, containerName, message)
		if tail := podLogTail(ctx, clientset, finished, containerName); tail != "" {
			summary += "; log tail:\n" + tail
		}
		return finished, message, fmt.Errorf("verifier pod %s/%s failed: %s", finished.Namespace, finished.Name, summary)
	}

	keep = false
	return finished, message, nil
}

// WaitForPod polls until the pod reaches a terminal phase, ctx is done, or the
// timeout elapses, returning the terminal Pod.
func WaitForPod(ctx context.Context, c client.Client, key client.ObjectKey, timeout time.Duration) (*corev1.Pod, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		var pod corev1.Pod
		if err := c.Get(ctx, key, &pod); err != nil {
			return nil, fmt.Errorf("getting verifier pod %s: %w", key, err)
		}
		if phase := pod.Status.Phase; phase == corev1.PodSucceeded || phase == corev1.PodFailed {
			return &pod, nil
		}

		select {
		case <-deadline:
			return nil, fmt.Errorf("timeout after %v waiting for verifier pod %s (phase %s)%s", timeout, key, pod.Status.Phase, whyNotRun(ctx, c, &pod))
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for verifier pod %s: %w%s", key, ctx.Err(), whyNotRun(ctx, c, &pod))
		case <-ticker.C:
		}
	}
}

// TerminatedMessage returns the termination message of the named container or
// "" if it has not terminated.
func TerminatedMessage(pod *corev1.Pod, container string) string {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == container && status.State.Terminated != nil {
			return status.State.Terminated.Message
		}
	}
	return ""
}

func containerFailureSummary(pod *corev1.Pod, container, message string) string {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != container || status.State.Terminated == nil {
			continue
		}
		terminated := status.State.Terminated
		if message != "" {
			return fmt.Sprintf("exitCode=%d reason=%s message=%s", terminated.ExitCode, terminated.Reason, message)
		}
		return fmt.Sprintf("exitCode=%d reason=%s", terminated.ExitCode, terminated.Reason)
	}
	if message != "" {
		return message
	}
	return "container did not report termination details"
}

// podLogTail returns the last PodLogTailLines lines of the container's log, or
// what stopped them from being read. It is "" without a clientset.
func podLogTail(ctx context.Context, clientset kubernetes.Interface, pod *corev1.Pod, container string) string {
	if clientset == nil {
		return ""
	}
	logCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	options := &corev1.PodLogOptions{Container: container, TailLines: ptr.To[int64](PodLogTailLines)}
	raw, err := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, options).DoRaw(logCtx)
	if err != nil {
		return fmt.Sprintf("(reading the %s container log: %v)", container, err)
	}
	return strings.TrimSpace(string(raw))
}

// whyNotRun is PodStatusDetails and the pod's latest Warning events, as a
// suffix for an error, or "" when neither says anything.
func whyNotRun(ctx context.Context, c client.Client, pod *corev1.Pod) string {
	details := PodStatusDetails(pod)
	details = append(details, PodWarningEvents(ctx, c, pod)...)
	if len(details) == 0 {
		return ""
	}
	return ": " + strings.Join(details, "; ")
}

// PodStatusDetails says what a pod's status records about why it is not
// running: its unmet conditions, its containers waiting or failed, and its
// own reason.
func PodStatusDetails(pod *corev1.Pod) []string {
	var details []string
	if strings.TrimSpace(pod.Status.Message) != "" {
		details = append(details, "pod "+reasonMessage(pod.Status.Reason, pod.Status.Message))
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Status == corev1.ConditionTrue || (condition.Reason == "" && condition.Message == "") {
			continue
		}
		details = append(details, fmt.Sprintf("%s=%s %s", condition.Type, condition.Status, reasonMessage(condition.Reason, condition.Message)))
	}
	details = append(details, ContainerDetails(pod)...)
	return details
}

// ContainerDetails reports each container, init containers included, that is
// waiting with a reason, has exited non-zero, or has restarted.
func ContainerDetails(pod *corev1.Pod) []string {
	var details []string
	for _, status := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		switch state := status.State; {
		case state.Waiting != nil && state.Waiting.Reason != "":
			details = append(details, fmt.Sprintf("container %s waiting %s", status.Name, reasonMessage(state.Waiting.Reason, state.Waiting.Message)))
		case state.Terminated != nil && state.Terminated.ExitCode != 0:
			details = append(details, fmt.Sprintf("container %s exited %d %s", status.Name, state.Terminated.ExitCode, reasonMessage(state.Terminated.Reason, state.Terminated.Message)))
		}
		if status.RestartCount > 0 {
			detail := fmt.Sprintf("container %s restarted %d times", status.Name, status.RestartCount)
			if last := status.LastTerminationState.Terminated; last != nil {
				detail += fmt.Sprintf(", last exit %d %s", last.ExitCode, last.Reason)
			}
			details = append(details, strings.TrimSpace(detail))
		}
	}
	return details
}

func reasonMessage(reason, message string) string {
	message = strings.TrimSpace(message)
	switch {
	case message == "":
		return reason
	case reason == "":
		return "(" + message + ")"
	default:
		return reason + " (" + message + ")"
	}
}

// PodWarningEvents returns the pod's latest Warning events, newest last. It
// is best effort: an identity that may not list events, or a slow API
// server, gets none, and the error they were meant to explain stands alone.
func PodWarningEvents(ctx context.Context, c client.Client, pod *corev1.Pod) []string {
	eventCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var events corev1.EventList
	if err := c.List(eventCtx, &events, client.InNamespace(pod.Namespace),
		client.MatchingFields{"involvedObject.name": pod.Name, "type": corev1.EventTypeWarning}); err != nil {
		return nil
	}
	return warningEvents(events.Items, PodEventLimit)
}

func warningEvents(events []corev1.Event, limit int) []string {
	events = slices.DeleteFunc(slices.Clone(events), func(e corev1.Event) bool { return e.Type != corev1.EventTypeWarning })
	slices.SortStableFunc(events, func(a, b corev1.Event) int { return eventTime(a).Compare(eventTime(b)) })
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	details := make([]string, 0, len(events))
	for _, event := range events {
		detail := "event " + event.Reason + ": " + strings.TrimSpace(event.Message)
		if event.Count > 1 {
			detail += fmt.Sprintf(" (x%d)", event.Count)
		}
		details = append(details, detail)
	}
	return details
}

func eventTime(event corev1.Event) time.Time {
	switch {
	case !event.LastTimestamp.IsZero():
		return event.LastTimestamp.Time
	case !event.EventTime.IsZero():
		return event.EventTime.Time
	default:
		return event.CreationTimestamp.Time
	}
}
