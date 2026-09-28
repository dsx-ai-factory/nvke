//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package verify asserts cluster-level facts from the test host, using the
// kubeconfig the agent hands back from the control-plane node.
package verify

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// NodesReady polls the API server until expectN nodes report Ready and none
// report otherwise, and returns their names.
//
// client-go rather than shelling out to kubectl, which is what the GitLab
// original does. Three reasons: the runner has no kubectl and this lane
// deliberately does not install one; client-go is already a direct dependency
// of the controller module, so this adds nothing to go.mod; and the pinned
// client is v0.36.3 against a v1.36.3 cluster, so
// the version-skew problem the GitLab script solves by parsing a version out of
// a package name does not arise at all.
//
// The predicate matches the original: every node must be Ready, so an
// unexpected extra node fails rather than being ignored.
func NodesReady(ctx context.Context, kubeconfig string, expectN int, timeout, poll time.Duration) ([]string, error) {
	if strings.TrimSpace(kubeconfig) == "" {
		return nil, fmt.Errorf("verify.NodesReady: kubeconfig is empty")
	}
	if expectN <= 0 {
		return nil, fmt.Errorf("verify.NodesReady: expectN must be > 0, got %d", expectN)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("verify.NodesReady: parse kubeconfig: %w", err)
	}
	// Bound each call so a wedged API server cannot pin the poll loop; the
	// outer timeout stays in charge.
	cfg.Timeout = 20 * time.Second

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("verify.NodesReady: build client: %w", err)
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	var lastNotReady []string

	for {
		ready, notReady, err := snapshot(ctx, client)
		switch {
		case err != nil:
			lastErr = err
		case len(ready) >= expectN && len(notReady) == 0:
			sort.Strings(ready)
			return ready, nil
		default:
			lastErr = nil
			lastNotReady = notReady
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return nil, fmt.Errorf("verify.NodesReady: timed out after %s; last error: %w", timeout, lastErr)
			}
			// The Reason/Message is the whole point of reporting this: a CNI
			// that never came up shows here as "container runtime network not
			// ready: NetworkReady=false ... cni plugin not initialized", which
			// names the actual problem. The GitLab version returns names only.
			return nil, fmt.Errorf("verify.NodesReady: timed out after %s; %d Ready, not ready:\n  %s",
				timeout, len(lastNotReady), strings.Join(lastNotReady, "\n  "))
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// snapshot returns the Ready node names, plus a human-readable line for each
// node that is not Ready including its condition Reason and Message.
func snapshot(ctx context.Context, client kubernetes.Interface) (ready, notReady []string, err error) {
	list, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	for i := range list.Items {
		n := &list.Items[i]
		cond := nodeReadyCondition(n)
		if cond != nil && cond.Status == corev1.ConditionTrue {
			ready = append(ready, n.Name)
			continue
		}
		if cond == nil {
			notReady = append(notReady, fmt.Sprintf("%s: no Ready condition yet", n.Name))
			continue
		}
		notReady = append(notReady, fmt.Sprintf("%s: Ready=%s reason=%s message=%s",
			n.Name, cond.Status, cond.Reason, strings.TrimSpace(cond.Message)))
	}
	return ready, notReady, nil
}

func nodeReadyCondition(n *corev1.Node) *corev1.NodeCondition {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == corev1.NodeReady {
			return &n.Status.Conditions[i]
		}
	}
	return nil
}
