// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"fmt"
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
			return nil, fmt.Errorf("timeout after %v waiting for verifier pod %s (phase %s)", timeout, key, pod.Status.Phase)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for verifier pod %s: %w", key, ctx.Err())
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
