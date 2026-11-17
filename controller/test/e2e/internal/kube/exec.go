// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream" //nolint:staticcheck // the fallback predicate kubectl uses, as in portforward.go
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	// defaultContainerAnnotation names the container kubectl exec picks when none is given.
	defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"
)

// Exec runs command in the pod's default container, as kubectl exec picks it,
// and returns its stdout. It prefers websockets and falls back to SPDY.
func Exec(ctx context.Context, restConfig *rest.Config, clientset kubernetes.Interface, pod *corev1.Pod, command ...string) (string, error) {
	request := clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(pod.Namespace).Name(pod.Name).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: DefaultContainer(pod), Command: command, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	websocket, err := remotecommand.NewWebSocketExecutor(restConfig, http.MethodGet, request.URL().String())
	if err != nil {
		return "", fmt.Errorf("building websocket executor: %w", err)
	}
	spdy, err := remotecommand.NewSPDYExecutor(restConfig, http.MethodPost, request.URL())
	if err != nil {
		return "", fmt.Errorf("building SPDY executor: %w", err)
	}
	executor, err := remotecommand.NewFallbackExecutor(websocket, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return "", fmt.Errorf("building exec executor: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return stdout.String(), fmt.Errorf("%s in %s/%s: %w: %s", strings.Join(command, " "), pod.Namespace, pod.Name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// DefaultContainer is the container kubectl exec picks: the one the pod's
// default-container annotation names, or else its first container.
func DefaultContainer(pod *corev1.Pod) string {
	if name := pod.Annotations[defaultContainerAnnotation]; name != "" {
		return name
	}
	return pod.Spec.Containers[0].Name
}
