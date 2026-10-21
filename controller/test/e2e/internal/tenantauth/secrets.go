// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantauth

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/mgmtns"
)

// The tenant-access controller publishes and deletes per-cluster Secrets in
// the site's management namespace, which mgmtns.Namespace resolves -- it is
// per-site configurable, so it is never assumed here.
const (
	kubeconfigSecretKey  = "value"
	trustBundleSecretKey = "ca.crt"
)

// spireRejectionKubeconfig uses admin trust to probe clusters that do not publish SPIRE credentials.
func spireRejectionKubeconfig(ctx context.Context, c client.Client, clusterName string) ([]byte, error) {
	var secret corev1.Secret
	secretName := adminKubeconfigSecretName(clusterName)
	if err := c.Get(ctx, client.ObjectKey{Namespace: mgmtns.Namespace(), Name: secretName}, &secret); err != nil {
		return nil, fmt.Errorf("getting admin kubeconfig secret %s/%s: %w", mgmtns.Namespace(), secretName, err)
	}
	config, err := clientcmd.Load(secret.Data[kubeconfigSecretKey])
	if err != nil {
		// Parser errors can quote admin credentials, so omit them from probe errors.
		return nil, fmt.Errorf("parsing admin kubeconfig from secret %s/%s", mgmtns.Namespace(), secretName)
	}
	current := config.Contexts[config.CurrentContext]
	if current == nil {
		return nil, errors.New("admin kubeconfig has no current context")
	}
	cluster := config.Clusters[current.Cluster]
	if cluster == nil || cluster.Server == "" || len(cluster.CertificateAuthorityData) == 0 || cluster.InsecureSkipTLSVerify {
		return nil, errors.New("admin kubeconfig has invalid cluster trust")
	}
	probe := clientcmdapi.NewConfig()
	probe.Clusters["tenant"] = &clientcmdapi.Cluster{
		Server: cluster.Server, CertificateAuthorityData: cluster.CertificateAuthorityData, TLSServerName: cluster.TLSServerName,
	}
	probe.AuthInfos["spiffe"] = &clientcmdapi.AuthInfo{}
	probe.Contexts["tenant"] = &clientcmdapi.Context{Cluster: "tenant", AuthInfo: "spiffe"}
	probe.CurrentContext = "tenant"
	raw, err := clientcmd.Write(*probe)
	if err != nil {
		return nil, fmt.Errorf("serializing SPIRE rejection kubeconfig: %w", err)
	}
	return raw, nil
}

// AssertKubeconfigAndTrustBundle checks that the OIDC kubeconfig and trust-bundle
// secrets exist on the management cluster. Their presence proves the OIDC
// reconcile ran for this cluster.
func AssertKubeconfigAndTrustBundle(ctx context.Context, c client.Client, clusterName string) error {
	if err := assertSecretKey(ctx, c, kubeconfigSecretName(clusterName), kubeconfigSecretKey); err != nil {
		return err
	}
	return assertSecretKey(ctx, c, trustBundleSecretName(clusterName), trustBundleSecretKey)
}

// AssertSPIREKubeconfigHasNoFallback checks that the published kubeconfig can
// authenticate only through the SPIRE exec plugin.
func AssertSPIREKubeconfigHasNoFallback(ctx context.Context, c client.Client, clusterName string) error {
	var secret corev1.Secret
	secretName := kubeconfigSecretName(clusterName)
	if err := c.Get(ctx, client.ObjectKey{Namespace: mgmtns.Namespace(), Name: secretName}, &secret); err != nil {
		return fmt.Errorf("getting secret %s/%s: %w", mgmtns.Namespace(), secretName, err)
	}
	config, err := clientcmd.Load(secret.Data[kubeconfigSecretKey])
	if err != nil {
		// Parser errors can quote credentials, so report only the secret location.
		return fmt.Errorf("parsing kubeconfig from secret %s/%s", mgmtns.Namespace(), secretName)
	}
	if len(config.Contexts) != 1 || len(config.Clusters) != 1 || len(config.AuthInfos) != 1 {
		return fmt.Errorf("published SPIRE kubeconfig must contain exactly one context, cluster, and user")
	}
	contextConfig := config.Contexts[config.CurrentContext]
	if contextConfig == nil {
		return fmt.Errorf("kubeconfig current context %q does not exist", config.CurrentContext)
	}
	cluster := config.Clusters[contextConfig.Cluster]
	authInfo := config.AuthInfos[contextConfig.AuthInfo]
	if cluster == nil || cluster.Server == "" || len(cluster.CertificateAuthorityData) == 0 || cluster.InsecureSkipTLSVerify {
		return fmt.Errorf("published SPIRE kubeconfig has invalid cluster trust")
	}
	if authInfo == nil || authInfo.Exec == nil || authInfo.Exec.Command != pluginInstallPath {
		return fmt.Errorf("published SPIRE kubeconfig does not select the SPIRE exec plugin")
	}
	if authInfo.Token != "" || authInfo.TokenFile != "" || authInfo.Username != "" || authInfo.Password != "" ||
		authInfo.ClientCertificate != "" || len(authInfo.ClientCertificateData) != 0 ||
		authInfo.ClientKey != "" || len(authInfo.ClientKeyData) != 0 || authInfo.AuthProvider != nil ||
		authInfo.Impersonate != "" || authInfo.ImpersonateUID != "" || len(authInfo.ImpersonateGroups) != 0 || len(authInfo.ImpersonateUserExtra) != 0 {
		return fmt.Errorf("published SPIRE kubeconfig contains fallback or impersonation credentials")
	}
	return nil
}

// KubeconfigExecEnv returns one environment value from the SPIRE exec
// credential stanza in a published tenant kubeconfig.
func KubeconfigExecEnv(ctx context.Context, c client.Client, clusterName, name string) (string, error) {
	var secret corev1.Secret
	secretName := kubeconfigSecretName(clusterName)
	if err := c.Get(ctx, client.ObjectKey{Namespace: mgmtns.Namespace(), Name: secretName}, &secret); err != nil {
		return "", fmt.Errorf("getting secret %s/%s: %w", mgmtns.Namespace(), secretName, err)
	}
	raw, ok := secret.Data[kubeconfigSecretKey]
	if !ok {
		return "", fmt.Errorf("secret %s/%s is missing key %q", mgmtns.Namespace(), secretName, kubeconfigSecretKey)
	}
	config, err := clientcmd.Load(raw)
	if err != nil {
		// Parser errors can quote credentials, so report only the secret location.
		return "", fmt.Errorf("parsing kubeconfig from secret %s/%s", mgmtns.Namespace(), secretName)
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

// AssertAdminKubeconfigDeleted checks that the always-published admin
// kubeconfig secret has been removed from the management cluster. Unlike the
// OIDC kubeconfig and trust bundle, this secret is published for every
// cluster regardless of OIDC mode, so it is the one universal proof that
// deleting the CAPI Cluster garbage-collected its owned tenant-access
// secrets rather than any workflow step deleting them explicitly.
func AssertAdminKubeconfigDeleted(ctx context.Context, c client.Client, clusterName string) error {
	return assertSecretAbsent(ctx, c, adminKubeconfigSecretName(clusterName))
}

func assertSecretKey(ctx context.Context, c client.Client, name, key string) error {
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: mgmtns.Namespace(), Name: name}, &secret); err != nil {
		return fmt.Errorf("getting secret %s/%s: %w", mgmtns.Namespace(), name, err)
	}
	if _, ok := secret.Data[key]; !ok {
		return fmt.Errorf("secret %s/%s is missing key %q", mgmtns.Namespace(), name, key)
	}
	return nil
}

func assertSecretAbsent(ctx context.Context, c client.Client, name string) error {
	err := c.Get(ctx, client.ObjectKey{Namespace: mgmtns.Namespace(), Name: name}, &corev1.Secret{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("getting secret %s/%s: %w", mgmtns.Namespace(), name, err)
	default:
		return fmt.Errorf("secret %s/%s still exists", mgmtns.Namespace(), name)
	}
}

func kubeconfigSecretName(clusterName string) string  { return clusterName + "-kubeconfig" }
func trustBundleSecretName(clusterName string) string { return clusterName + "-cluster-trust-bundle" }

func adminKubeconfigSecretName(clusterName string) string { return clusterName + "-admin-kubeconfig" }
