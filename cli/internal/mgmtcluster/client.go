// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mgmtcluster provides a client for reading resources from the NVKE
// management cluster.
package mgmtcluster

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// DefaultSecretsNamespace is the management-cluster namespace where the
	// tenantaccess controller publishes per-cluster kubeconfig Secrets, unless
	// the site renamed it (the controller chart's nvkeSystemNamespace.name).
	DefaultSecretsNamespace = "nke-system"

	// adminKubeconfigSecretSuffix names the Secret that holds the
	// nvke:breakglass admin kubeconfig, regardless of OIDC mode (mirrors
	// tenantaccess.AdminKubeconfigSecretName in the controller). This is
	// distinct from the "<cluster>-kubeconfig" Secret, which is the controller's and CAPI's
	// own consumed kubeconfig and holds a SPIRE exec-auth kubeconfig instead
	// of the node admin.conf when OIDC auth is enabled on the cluster.
	adminKubeconfigSecretSuffix = "-admin-kubeconfig"

	// kubeconfigSecretKey is the data key inside the kubeconfig Secret
	// (mirrors tenantaccess.KubeconfigSecretKey in the controller).
	kubeconfigSecretKey = "value"

	// requestTimeout bounds every request this package issues.
	requestTimeout = 30 * time.Second
)

// Client is a thin wrapper around a management-cluster Kubernetes client.
type Client struct {
	kube      kubernetes.Interface
	namespace string
}

// Option configures a Client.
type Option func(*Client)

// WithSecretsNamespace reads kubeconfig Secrets from namespace instead of
// DefaultSecretsNamespace. An empty namespace keeps the default.
func WithSecretsNamespace(namespace string) Option {
	return func(c *Client) {
		if namespace != "" {
			c.namespace = namespace
		}
	}
}

func newClient(kube kubernetes.Interface, opts []Option) *Client {
	c := &Client{kube: kube, namespace: DefaultSecretsNamespace}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// RESTConfig loads the management cluster kubeconfig at kubeconfigPath into a REST config
// with the request timeout set. It is what every client of the management cluster is built
// from, so they all fail the same way when the kubeconfig is missing or unreadable.
func RESTConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("management cluster kubeconfig path is empty: run 'nvkectl config set-site' with --kubeconfig to set the management cluster kubeconfig")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading management cluster kubeconfig %q: %w", kubeconfigPath, err)
	}
	cfg.Timeout = requestTimeout
	return cfg, nil
}

// New builds a Client from a management cluster kubeconfig path. The caller
// must resolve the path (e.g. from the active site's configured kubeconfig)
// — New does not fall back to KUBECONFIG or ~/.kube/config.
func New(kubeconfigPath string, opts ...Option) (*Client, error) {
	cfg, err := RESTConfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building management cluster client: %w", err)
	}
	return newClient(kube, opts), nil
}

// newWithClient builds a Client from an existing Kubernetes client, bypassing
// kubeconfig resolution. Used by tests to inject a fake clientset.
func newWithClient(kube kubernetes.Interface, opts ...Option) *Client {
	return newClient(kube, opts)
}

// GetClusterKubeconfig returns the admin kubeconfig for the named tenant
// cluster by reading the Secret that the tenantaccess controller publishes
// into <namespace>/<cluster-name>-admin-kubeconfig (nke-system by default).
func (c *Client) GetClusterKubeconfig(ctx context.Context, clusterName string) ([]byte, error) {
	secretName := clusterName + adminKubeconfigSecretSuffix
	secret, err := c.kube.CoreV1().Secrets(c.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("admin kubeconfig secret %s/%s not found -- check the cluster name, or the cluster may not be provisioned yet: %w", c.namespace, secretName, err)
		case apierrors.IsForbidden(err):
			return nil, fmt.Errorf("not allowed to read secret %[1]s/%[2]s -- the site kubeconfig identity needs 'get' on secrets in %[1]s: %[3]w", c.namespace, secretName, err)
		default:
			return nil, fmt.Errorf("reading admin kubeconfig secret %s/%s: %w", c.namespace, secretName, err)
		}
	}
	data, ok := secret.Data[kubeconfigSecretKey]
	if !ok || len(data) == 0 {
		return nil, fmt.Errorf("admin kubeconfig secret %s/%s has no %q key", c.namespace, secretName, kubeconfigSecretKey)
	}
	return data, nil
}
