// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mgmtcluster provides a client for reading resources from the NKE
// management cluster.
package mgmtcluster

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	// requestTimeout bounds every request this package issues.
	requestTimeout = 30 * time.Second
)

// Client is a thin wrapper around a management-cluster Kubernetes client.
type Client struct {
	kube kubernetes.Interface
}

// New builds a Client from a management cluster kubeconfig path. The caller
// must resolve the path (e.g. from the active site's configured kubeconfig)
// — New does not fall back to KUBECONFIG or ~/.kube/config.
func New(kubeconfigPath string) (*Client, error) {
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("management cluster kubeconfig path is empty: run 'nkectl config set-site' with --kubeconfig to set the management cluster kubeconfig")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading management cluster kubeconfig %q: %w", kubeconfigPath, err)
	}
	cfg.Timeout = requestTimeout
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building management cluster client: %w", err)
	}
	return &Client{kube: kube}, nil
}

// newWithClient builds a Client from an existing Kubernetes client, bypassing
// kubeconfig resolution. Used by tests to inject a fake clientset.
func newWithClient(kube kubernetes.Interface) *Client {
	return &Client{kube: kube}
}

// GetClusterKubeconfig returns the admin kubeconfig for the named tenant
// cluster by reading the Secret that the tenantaccess controller publishes
// into nke-system/<cluster-name>-admin-kubeconfig.
func (c *Client) GetClusterKubeconfig(ctx context.Context, clusterName string) ([]byte, error) {
	secretName := clusterName + adminKubeconfigSecretSuffix
	secret, err := c.kube.CoreV1().Secrets(secretsNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("admin kubeconfig secret nke-system/%s not found -- check the cluster name, or the cluster may not be provisioned yet: %w", secretName, err)
		case apierrors.IsForbidden(err):
			return nil, fmt.Errorf("not allowed to read secret nke-system/%s -- the site kubeconfig identity needs 'get' on secrets in nke-system: %w", secretName, err)
		default:
			return nil, fmt.Errorf("reading admin kubeconfig secret nke-system/%s: %w", secretName, err)
		}
	}
	data, ok := secret.Data[kubeconfigSecretKey]
	if !ok || len(data) == 0 {
		return nil, fmt.Errorf("admin kubeconfig secret nke-system/%s has no %q key", secretName, kubeconfigSecretKey)
	}
	return data, nil
}
