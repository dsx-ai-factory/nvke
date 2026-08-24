// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mgmtcluster provides a client for reading resources from the NKE
// management cluster. This is Option B for nkectl get kubeconfig — reading
// the kubeconfig Secret that the tenantaccess controller publishes into the
// nke-system namespace. It requires the operator to have their own kubeconfig
// for the management cluster.
//
// Option A (a CWE REST endpoint) is preferred long-term but depends on a
// server-side change. See NKX-13899 for the decision record.
package mgmtcluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	// secretsNamespace is the management-cluster namespace where the
	// tenantaccess controller publishes per-cluster kubeconfig Secrets.
	secretsNamespace = "nke-system"

	// kubeconfigSecretKey is the data key inside the per-cluster kubeconfig
	// Secret (mirrors tenantaccess.KubeconfigSecretKey in the controller).
	kubeconfigSecretKey = "value"
)

// Client is a thin wrapper around a management-cluster k8s client.
type Client struct {
	kube kubernetes.Interface
}

// New builds a Client from a kubeconfig path.
// If path is empty it falls back to the KUBECONFIG env var and then
// ~/.kube/config, following standard kubectl behaviour.
func New(kubeconfigPath string) (*Client, error) {
	cfg, err := buildRestConfig(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("building management cluster client: %w", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building management cluster client: %w", err)
	}
	return &Client{kube: kube}, nil
}

// GetClusterKubeconfig returns the admin kubeconfig for the named tenant
// cluster by reading the Secret that the tenantaccess controller publishes
// into nke-system/<cluster-name>-kubeconfig.
func (c *Client) GetClusterKubeconfig(ctx context.Context, clusterName string) ([]byte, error) {
	secretName := clusterName + "-kubeconfig"
	secret, err := c.kube.CoreV1().Secrets(secretsNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig secret nke-system/%s: %w", secretName, err)
	}
	data, ok := secret.Data[kubeconfigSecretKey]
	if !ok || len(data) == 0 {
		return nil, fmt.Errorf("kubeconfig secret nke-system/%s has no %q key — cluster may not be provisioned yet", secretName, kubeconfigSecretKey)
	}
	return data, nil
}

func buildRestConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" {
		kubeconfigPath = kubeconfigEnvOrDefault()
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %q: %w", kubeconfigPath, err)
	}
	return cfg, nil
}

func kubeconfigEnvOrDefault() string {
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return env
	}
	if home := homedir.HomeDir(); home != "" {
		return filepath.Join(home, ".kube", "config")
	}
	return ""
}
