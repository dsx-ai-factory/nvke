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
	// tenantaccess controller publishes per-cluster kubeconfig Secrets on a site
	// installed with the chart's defaults.
	DefaultSecretsNamespace = "nvke-system"

	// LegacySecretsNamespace is the namespace a site installed before
	// nvkeSystemNamespace.name existed publishes into. A site that pinned the
	// old name keeps using it indefinitely, so reads fall back to it.
	LegacySecretsNamespace = "nke-system"

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
	kube kubernetes.Interface
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
func New(kubeconfigPath string) (*Client, error) {
	cfg, err := RESTConfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
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
// cluster by reading the Secret that the tenantaccess controller publishes into
// <namespace>/<cluster-name>-admin-kubeconfig.
//
// namespaces are tried in order, and a Secret missing from one is not an error
// until every one has been tried; the error then names the last tried, unless
// one of the namespaces was unreadable, in which case it reports that. The
// caller passes more than one so a site that pinned the legacy management
// namespace is still found without the operator having to know which one the
// site was installed with. An empty slice means DefaultSecretsNamespace alone.
func (c *Client) GetClusterKubeconfig(ctx context.Context, clusterName string, namespaces []string) ([]byte, error) {
	if len(namespaces) == 0 {
		namespaces = []string{DefaultSecretsNamespace}
	}
	secretName := clusterName + adminKubeconfigSecretSuffix

	// A Forbidden outranks a later NotFound: "the Secret is not there" is only
	// true of a namespace we were allowed to look in, so reporting the NotFound
	// would send the operator after a cluster name that may be perfectly good.
	var lastErr, forbiddenErr error
	for _, namespace := range namespaces {
		secret, err := c.kube.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		switch {
		case err == nil:
			data, ok := secret.Data[kubeconfigSecretKey]
			if !ok || len(data) == 0 {
				return nil, fmt.Errorf("admin kubeconfig secret %s/%s has no %q key", namespace, secretName, kubeconfigSecretKey)
			}
			return data, nil
		case apierrors.IsNotFound(err):
			lastErr = fmt.Errorf("admin kubeconfig secret %s/%s not found -- check the cluster name, or the cluster may not be provisioned yet: %w", namespace, secretName, err)
		case apierrors.IsForbidden(err):
			lastErr = fmt.Errorf("not allowed to read secret %s/%s -- the site kubeconfig identity needs 'get' on secrets in %s: %w", namespace, secretName, namespace, err)
			if forbiddenErr == nil {
				forbiddenErr = lastErr
			}
		default:
			return nil, fmt.Errorf("reading admin kubeconfig secret %s/%s: %w", namespace, secretName, err)
		}
	}
	if forbiddenErr != nil {
		return nil, forbiddenErr
	}
	return nil, lastErr
}
