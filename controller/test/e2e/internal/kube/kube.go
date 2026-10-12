// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package kube contains helpers to interact with the kube-api server of
// NVKE clusters.
package kube

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RESTConfig builds a *rest.Config for the management cluster. If kubeconfig or
// context are set they pin the explicit path / current-context; otherwise the
// default loading rules apply (honoring KUBECONFIG and ~/.kube/config).
func RESTConfig(kubeconfig, context string) (*rest.Config, error) {
	clientConfig := clientConfig(kubeconfig, context)
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building REST config: %w", err)
	}
	return restConfig, nil
}

// RESTConfigWithInClusterFallback builds a management-cluster REST config and,
// when no kubeconfig or context was explicitly selected, falls back to the
// pod's in-cluster configuration.
func RESTConfigWithInClusterFallback(kubeconfig, context string) (*rest.Config, error) {
	restConfig, kubeconfigErr := RESTConfig(kubeconfig, context)
	if kubeconfigErr == nil {
		return restConfig, nil
	}
	if kubeconfig != "" || context != "" {
		return nil, fmt.Errorf("load Kubernetes configuration: %w", kubeconfigErr)
	}

	restConfig, inClusterErr := rest.InClusterConfig()
	if inClusterErr == nil {
		return restConfig, nil
	}

	return nil, fmt.Errorf("load Kubernetes configuration from kubeconfig/default rules: %w; in-cluster configuration also failed: %w", kubeconfigErr, inClusterErr)
}

// RuntimeClient builds a controller-runtime client for the management cluster.
func RuntimeClient(restConfig *rest.Config) (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering core/v1 scheme: %w", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering apps/v1 scheme: %w", err)
	}
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building controller-runtime client: %w", err)
	}
	return c, nil
}

// TrustDomain derives the SPIRE trust domain from the active kube context name.
// The final "kind-X" segment maps to "X.nke.local" so remote-sync contexts
// such as "kind-cwe-capi" or "colossus-kind-cwe-capi" resolve to "cwe-capi.nke.local".
func TrustDomain(kubeconfig, context string) (string, error) {
	raw, err := clientConfig(kubeconfig, context).RawConfig()
	if err != nil {
		return "", fmt.Errorf("loading kube config: %w", err)
	}
	ctx := context
	if ctx == "" {
		ctx = raw.CurrentContext
	}
	kindIndex := strings.LastIndex(ctx, "kind-")
	if kindIndex == -1 {
		return "", fmt.Errorf("context %q is not a kind context; expected name to contain kind-", ctx)
	}
	return ctx[kindIndex+len("kind-"):] + ".nke.local", nil
}

// clientConfig resolves the file and the context independently; either can be
// set without the other.
//
// ExplicitPath is set on the default rules rather than replacing them. A bare
// ClientConfigLoadingRules has an empty Precedence and loads no file, so naming
// a context without also naming a file fails with `context "X" does not exist`
// against a kubeconfig that contains it.
func clientConfig(kubeconfig, context string) clientcmd.ClientConfig {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules,
		&clientcmd.ConfigOverrides{CurrentContext: context},
	)
}
