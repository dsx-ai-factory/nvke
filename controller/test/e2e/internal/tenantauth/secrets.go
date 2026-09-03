// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantauth

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	secretsNamespace     = "nke-system"
	kubeconfigSecretKey  = "value"
	trustBundleSecretKey = "ca.crt"
)

// AssertKubeconfigAndTrustBundle checks that the OIDC kubeconfig and trust-bundle
// secrets exist on the management cluster. Their presence proves the OIDC
// reconcile ran for this cluster.
func AssertKubeconfigAndTrustBundle(ctx context.Context, c client.Client, clusterName string) error {
	if err := assertSecretKey(ctx, c, kubeconfigSecretName(clusterName), kubeconfigSecretKey); err != nil {
		return err
	}
	return assertSecretKey(ctx, c, trustBundleSecretName(clusterName), trustBundleSecretKey)
}

// KubeconfigExecEnv returns one environment value from the SPIRE exec
// credential stanza in a published tenant kubeconfig.
func KubeconfigExecEnv(ctx context.Context, c client.Client, clusterName, name string) (string, error) {
	var secret corev1.Secret
	secretName := kubeconfigSecretName(clusterName)
	if err := c.Get(ctx, client.ObjectKey{Namespace: secretsNamespace, Name: secretName}, &secret); err != nil {
		return "", fmt.Errorf("getting secret %s/%s: %w", secretsNamespace, secretName, err)
	}
	raw, ok := secret.Data[kubeconfigSecretKey]
	if !ok {
		return "", fmt.Errorf("secret %s/%s is missing key %q", secretsNamespace, secretName, kubeconfigSecretKey)
	}
	config, err := clientcmd.Load(raw)
	if err != nil {
		return "", fmt.Errorf("parsing kubeconfig from secret %s/%s: %w", secretsNamespace, secretName, err)
	}
	contextConfig, ok := config.Contexts[config.CurrentContext]
	if !ok {
		return "", fmt.Errorf("kubeconfig current context %q does not exist", config.CurrentContext)
	}
	authInfo, ok := config.AuthInfos[contextConfig.AuthInfo]
	if !ok || authInfo.Exec == nil {
		return "", fmt.Errorf("kubeconfig auth info %q has no exec credential", contextConfig.AuthInfo)
	}
	for _, variable := range authInfo.Exec.Env {
		if variable.Name == name {
			return variable.Value, nil
		}
	}
	return "", fmt.Errorf("kubeconfig exec credential has no %s environment variable", name)
}

// AssertKubeconfigAndTrustBundleDeleted checks that the OIDC kubeconfig and
// trust-bundle secrets have been removed from the management cluster — the
// inverse of AssertKubeconfigAndTrustBundle. Use it after a cluster is deleted
// to confirm the secrets were cleaned up.
func AssertKubeconfigAndTrustBundleDeleted(ctx context.Context, c client.Client, clusterName string) error {
	for _, name := range []string{kubeconfigSecretName(clusterName), trustBundleSecretName(clusterName)} {
		if err := assertSecretAbsent(ctx, c, name); err != nil {
			return err
		}
	}
	return nil
}

func assertSecretKey(ctx context.Context, c client.Client, name, key string) error {
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: secretsNamespace, Name: name}, &secret); err != nil {
		return fmt.Errorf("getting secret %s/%s: %w", secretsNamespace, name, err)
	}
	if _, ok := secret.Data[key]; !ok {
		return fmt.Errorf("secret %s/%s is missing key %q", secretsNamespace, name, key)
	}
	return nil
}

func assertSecretAbsent(ctx context.Context, c client.Client, name string) error {
	err := c.Get(ctx, client.ObjectKey{Namespace: secretsNamespace, Name: name}, &corev1.Secret{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("getting secret %s/%s: %w", secretsNamespace, name, err)
	default:
		return fmt.Errorf("secret %s/%s still exists", secretsNamespace, name)
	}
}

func kubeconfigSecretName(clusterName string) string  { return clusterName + "-kubeconfig" }
func trustBundleSecretName(clusterName string) string { return clusterName + "-cluster-trust-bundle" }
