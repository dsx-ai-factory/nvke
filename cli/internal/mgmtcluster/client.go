// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mgmtcluster provides a client for reading resources from the NKE
// management cluster — specifically the per-cluster admin kubeconfig Secret
// that the tenantaccess controller publishes into the nke-system namespace.
//
// Note: cwedeploy.Engine.TenantKubeconfig reads a different Secret
// (<cluster>-kubeconfig, not -admin-kubeconfig) via a polling kubectl
// subprocess, because it waits for a freshly-bootstrapped site's own
// kubeconfig to appear. This package does a single, non-polling read for an
// already-provisioned cluster, so it doesn't reuse that function.
package mgmtcluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	kube client.Client
}

// New builds a Client from a management cluster kubeconfig path. The caller
// must resolve the path (e.g. from the active site's configured kubeconfig,
// or an explicit override) — New does not fall back to KUBECONFIG or
// ~/.kube/config.
//
// Follows the pattern in controller/internal/kube: a controller-runtime
// client.Client built from clientcmd, rather than a raw client-go Clientset.
func New(kubeconfigPath string) (*Client, error) {
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("management cluster kubeconfig path is empty")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading management cluster kubeconfig %q: %w", kubeconfigPath, err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("building scheme: %w", err)
	}
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building management cluster client: %w", err)
	}
	return &Client{kube: kube}, nil
}

// newWithClient builds a Client from an existing controller-runtime client,
// bypassing kubeconfig resolution. Used by tests to inject a fake.
func newWithClient(kube client.Client) *Client {
	return &Client{kube: kube}
}

// GetClusterKubeconfig returns the admin kubeconfig for the named tenant
// cluster by reading the Secret that the tenantaccess controller publishes
// into nke-system/<cluster-name>-admin-kubeconfig.
func (c *Client) GetClusterKubeconfig(ctx context.Context, clusterName string) ([]byte, error) {
	secretName := clusterName + adminKubeconfigSecretSuffix
	secret := &corev1.Secret{}
	if err := c.kube.Get(ctx, client.ObjectKey{Namespace: secretsNamespace, Name: secretName}, secret); err != nil {
		return nil, fmt.Errorf("reading admin kubeconfig secret nke-system/%s: %w", secretName, err)
	}
	data, ok := secret.Data[kubeconfigSecretKey]
	if !ok || len(data) == 0 {
		return nil, fmt.Errorf("admin kubeconfig secret nke-system/%s has no %q key — cluster may not be provisioned yet", secretName, kubeconfigSecretKey)
	}
	return data, nil
}
