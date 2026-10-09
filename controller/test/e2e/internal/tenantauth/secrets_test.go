// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantauth

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubeconfigExecEnv(t *testing.T) {
	t.Parallel()
	config := clientcmdapi.NewConfig()
	config.Clusters["tenant"] = &clientcmdapi.Cluster{Server: "https://tenant.example"}
	config.AuthInfos["spiffe"] = &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{
		APIVersion: "client.authentication.k8s.io/v1",
		Command:    "/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth",
		Env: []clientcmdapi.ExecEnvVar{
			{Name: "SPIFFE_JWT_AUDIENCE", Value: "tenant-a"},
			{Name: "SPIFFE_JWT_EXCHANGE_ENDPOINT", Value: "https://identity.example/v1/exchange"},
		},
	}}
	config.Contexts["tenant"] = &clientcmdapi.Context{Cluster: "tenant", AuthInfo: "spiffe"}
	config.CurrentContext = "tenant"
	raw := requireKubeconfig(t, config)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: secretsNamespace, Name: "tenant-a-kubeconfig"},
		Data:       map[string][]byte{kubeconfigSecretKey: raw},
	}).Build()

	got, err := KubeconfigExecEnv(t.Context(), client, "tenant-a", "SPIFFE_JWT_EXCHANGE_ENDPOINT")
	require.NoError(t, err)
	require.Equal(t, "https://identity.example/v1/exchange", got)
}

func TestAssertSPIREKubeconfigHasNoFallback(t *testing.T) {
	t.Parallel()
	config := clientcmdapi.NewConfig()
	config.Clusters["tenant"] = &clientcmdapi.Cluster{Server: "https://tenant.example", CertificateAuthorityData: []byte("ca")}
	config.AuthInfos["spiffe"] = &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{Command: pluginInstallPath}}
	config.Contexts["tenant"] = &clientcmdapi.Context{Cluster: "tenant", AuthInfo: "spiffe"}
	config.CurrentContext = "tenant"

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: secretsNamespace, Name: "tenant-a-kubeconfig"},
		Data:       map[string][]byte{kubeconfigSecretKey: requireKubeconfig(t, config)},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	require.NoError(t, AssertSPIREKubeconfigHasNoFallback(t.Context(), c, "tenant-a"))

	config.AuthInfos["spiffe"].Token = "fallback-token"
	secret.Data[kubeconfigSecretKey] = requireKubeconfig(t, config)
	require.NoError(t, c.Update(t.Context(), secret))
	err := AssertSPIREKubeconfigHasNoFallback(t.Context(), c, "tenant-a")
	require.ErrorContains(t, err, "fallback or impersonation")
	require.NotContains(t, err.Error(), "fallback-token")

	config.AuthInfos["spiffe"].Token = ""
	for _, testCase := range []struct {
		name string
		add  func(*clientcmdapi.Config)
	}{
		{
			name: "alternate admin context",
			add: func(config *clientcmdapi.Config) {
				config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "example-token"}
				config.Contexts["admin"] = &clientcmdapi.Context{Cluster: "tenant", AuthInfo: "admin"}
			},
		},
		{
			name: "unused credential user",
			add: func(config *clientcmdapi.Config) {
				config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "example-token"}
			},
		},
		{
			name: "additional context",
			add: func(config *clientcmdapi.Config) {
				config.Contexts["extra"] = &clientcmdapi.Context{Cluster: "tenant", AuthInfo: "spiffe"}
			},
		},
		{
			name: "additional cluster",
			add: func(config *clientcmdapi.Config) {
				config.Clusters["extra"] = config.Clusters["tenant"].DeepCopy()
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			extra := config.DeepCopy()
			testCase.add(extra)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: secretsNamespace, Name: "tenant-a-kubeconfig"},
				Data:       map[string][]byte{kubeconfigSecretKey: requireKubeconfig(t, extra)},
			}).Build()

			err := AssertSPIREKubeconfigHasNoFallback(t.Context(), c, "tenant-a")
			require.EqualError(t, err, "published SPIRE kubeconfig must contain exactly one context, cluster, and user")
			require.NotContains(t, err.Error(), "example-token")
		})
	}
}

func TestKubeconfigReadersOmitParserCredentials(t *testing.T) {
	t.Parallel()
	raw := []byte(`apiVersion: v1
kind: Config
users:
- name: spiffe
  user:
    token: !!int example-token
    password: example-password
`)
	_, parseErr := clientcmd.Load(raw)
	require.ErrorContains(t, parseErr, "example-token")

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: secretsNamespace, Name: "tenant-a-kubeconfig"},
		Data:       map[string][]byte{kubeconfigSecretKey: raw},
	}).Build()

	for _, read := range []func() error{
		func() error { return AssertSPIREKubeconfigHasNoFallback(t.Context(), c, "tenant-a") },
		func() error {
			value, err := KubeconfigExecEnv(t.Context(), c, "tenant-a", "SPIFFE_JWT_EXCHANGE_ENDPOINT")
			require.Empty(t, value)
			return err
		},
	} {
		err := read()
		require.EqualError(t, err, "parsing kubeconfig from secret nke-system/tenant-a-kubeconfig")
		require.NotContains(t, err.Error(), "example-token")
		require.NotContains(t, err.Error(), "example-password")
		require.NoError(t, errors.Unwrap(err))
	}
}

func requireKubeconfig(t *testing.T, config *clientcmdapi.Config) []byte {
	t.Helper()
	raw, err := clientcmd.Write(*config)
	require.NoError(t, err)
	return raw
}
