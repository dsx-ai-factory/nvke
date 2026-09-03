// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mgmtcluster provides a client for reading resources from the NKE
// management cluster — specifically the per-cluster admin kubeconfig Secret
// that the tenantaccess controller publishes into the nke-system namespace.
package mgmtcluster

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// secretsNamespace is the management-cluster namespace where the
	// tenantaccess controller publishes per-cluster kubeconfig Secrets.
	secretsNamespace = "nke-system"

	// adminKubeconfigSecretSuffix names the Secret that always holds the
	// verbatim control-plane admin.conf, regardless of OIDC mode (mirrors
	// tenantaccess.AdminKubeconfigSecretName in the controller). This is
	// distinct from the "<cluster>-kubeconfig" Secret, which is CWE/CAPI's
	// own consumed kubeconfig and holds a SPIRE exec-auth kubeconfig instead
	// of admin.conf when OIDC auth is enabled on the cluster.
	adminKubeconfigSecretSuffix = "-admin-kubeconfig"

	// kubeconfigSecretKey is the data key inside the kubeconfig Secret
	// (mirrors tenantaccess.KubeconfigSecretKey in the controller).
	kubeconfigSecretKey = "value"
)

// Client is a thin wrapper around a management-cluster k8s client.
type Client struct {
	kube kubernetes.Interface
}

// New builds a Client from a management cluster kubeconfig path. The caller
// must resolve the path (e.g. from the active site's configured kubeconfig,
// or an explicit override) — New does not fall back to KUBECONFIG or
// ~/.kube/config.
func New(kubeconfigPath string) (*Client, error) {
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("management cluster kubeconfig path is empty")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading management cluster kubeconfig %q: %w", kubeconfigPath, err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building management cluster client: %w", err)
	}
	return &Client{kube: kube}, nil
}

// NewWithClient builds a Client from an existing kubernetes.Interface,
// bypassing kubeconfig resolution. Used by tests to inject a fake clientset.
func NewWithClient(kube kubernetes.Interface) *Client {
	return &Client{kube: kube}
}

// GetClusterKubeconfig returns the admin kubeconfig for the named tenant
// cluster by reading the Secret that the tenantaccess controller publishes
// into nke-system/<cluster-name>-admin-kubeconfig.
func (c *Client) GetClusterKubeconfig(ctx context.Context, clusterName string) ([]byte, error) {
	secretName := clusterName + adminKubeconfigSecretSuffix
	secret, err := c.kube.CoreV1().Secrets(secretsNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading admin kubeconfig secret nke-system/%s: %w", secretName, err)
	}
	data, ok := secret.Data[kubeconfigSecretKey]
	if !ok || len(data) == 0 {
		return nil, fmt.Errorf("admin kubeconfig secret nke-system/%s has no %q key — cluster may not be provisioned yet", secretName, kubeconfigSecretKey)
	}
	return data, nil
}
