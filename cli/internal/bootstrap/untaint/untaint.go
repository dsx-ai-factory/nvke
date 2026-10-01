// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package untaint makes the control-plane nodes of a freshly bootstrapped
// cluster schedulable.
package untaint

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

// ControlPlaneTaintKey is the taint kubeadm puts on every control-plane node.
const ControlPlaneTaintKey = "node-role.kubernetes.io/control-plane"

// ManualCommand is the kubectl command that does what ControlPlane does, for
// an operator to run by hand when the command could not.
const ManualCommand = "kubectl taint nodes --all " + ControlPlaneTaintKey + "-"

// NewClient builds a Kubernetes client from a kubeconfig file.
func NewClient(kubeconfig string) (kubernetes.Interface, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %s: %w", kubeconfig, err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating client for kubeconfig %s: %w", kubeconfig, err)
	}
	return client, nil
}

// ControlPlane removes every taint keyed ControlPlaneTaintKey from every node
// and returns the names of the nodes it changed, in list order. A node that
// does not carry the taint is left alone, so a second run changes nothing.
func ControlPlane(ctx context.Context, client kubernetes.Interface, logger *slog.Logger) ([]string, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	var changed []string
	for i := range nodes.Items {
		name := nodes.Items[i].Name
		if !hasControlPlaneTaint(nodes.Items[i].Spec.Taints) {
			logger.Debug("node already schedulable", "node", name)
			continue
		}
		if err := untaintNode(ctx, client, name); err != nil {
			return changed, err
		}
		logger.Info("removed the control-plane taint", "node", name)
		changed = append(changed, name)
	}
	return changed, nil
}

// untaintNode re-reads the node on every attempt so a concurrent update by the
// kubelet or a controller does not make the write fail for good.
func untaintNode(ctx context.Context, client kubernetes.Interface, name string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		node.Spec.Taints = withoutControlPlaneTaint(node.Spec.Taints)
		_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("removing the control-plane taint from node %s: %w", name, err)
	}
	return nil
}

func hasControlPlaneTaint(taints []corev1.Taint) bool {
	for _, taint := range taints {
		if taint.Key == ControlPlaneTaintKey {
			return true
		}
	}
	return false
}

func withoutControlPlaneTaint(taints []corev1.Taint) []corev1.Taint {
	kept := make([]corev1.Taint, 0, len(taints))
	for _, taint := range taints {
		if taint.Key != ControlPlaneTaintKey {
			kept = append(kept, taint)
		}
	}
	return kept
}
