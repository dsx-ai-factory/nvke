// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package auth mints CWE bearer tokens through the management cluster's
// Kubernetes TokenRequest API.
package auth

import (
	"context"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
	authnv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	authConfigNamespace = "cloud-workflow-engine"
	authConfigMap       = "nke-auth-config"
	authConfigKey       = "auth.yaml"
	tokenNamespace      = "nke-clients"
	tokenServiceAccount = "nke-client"
	tokenLifetime       = 10 * time.Minute
)

// Source supplies audience-scoped tokens for nkectl's CWE identity. Mint
// returns no credential when the management cluster has no CWE auth ConfigMap.
type Source struct {
	client kubernetes.Interface
}

// NewSource builds a token source from a management cluster kubeconfig.
func NewSource(kubeconfig string) (*Source, error) {
	if kubeconfig == "" {
		return nil, fmt.Errorf("kubeconfig is empty: run 'nkectl config set-site --kubeconfig <path>'")
	}

	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %q: %w", kubeconfig, err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building Kubernetes client from kubeconfig %q: %w", kubeconfig, err)
	}
	return &Source{client: client}, nil
}

// Mint creates a token for nkectl using the configured CWE audience.
func (s *Source) Mint(ctx context.Context) (string, error) {
	audience, enabled, err := s.audience(ctx)
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", nil
	}

	expirationSeconds := int64(tokenLifetime.Seconds())
	request := &authnv1.TokenRequest{
		Spec: authnv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &expirationSeconds,
		},
	}
	response, err := s.client.CoreV1().
		ServiceAccounts(tokenNamespace).
		CreateToken(ctx, tokenServiceAccount, request, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("minting token for %s/%s: %w", tokenNamespace, tokenServiceAccount, err)
	}
	if response.Status.Token == "" {
		return "", fmt.Errorf("minting token for %s/%s: apiserver returned an empty token", tokenNamespace, tokenServiceAccount)
	}
	return response.Status.Token, nil
}

func (s *Source) audience(ctx context.Context) (string, bool, error) {
	configMap, err := s.client.CoreV1().
		ConfigMaps(authConfigNamespace).
		Get(ctx, authConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading CWE authentication ConfigMap %s/%s: %w",
			authConfigNamespace, authConfigMap, err)
	}

	raw, ok := configMap.Data[authConfigKey]
	if !ok {
		return "", false, fmt.Errorf("CWE authentication ConfigMap %s/%s has no %q key",
			authConfigNamespace, authConfigMap, authConfigKey)
	}

	var config struct {
		Audiences []string `yaml:"audiences"`
	}
	if err := yaml.Unmarshal([]byte(raw), &config); err != nil {
		return "", false, fmt.Errorf("parsing %q from CWE authentication ConfigMap %s/%s: %w",
			authConfigKey, authConfigNamespace, authConfigMap, err)
	}
	if len(config.Audiences) == 0 || config.Audiences[0] == "" {
		return "", false, fmt.Errorf("CWE authentication ConfigMap %s/%s sets no audiences",
			authConfigNamespace, authConfigMap)
	}
	return config.Audiences[0], true, nil
}
