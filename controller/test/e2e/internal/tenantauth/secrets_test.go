// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantauth

import (
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

func requireKubeconfig(t *testing.T, config *clientcmdapi.Config) []byte {
	t.Helper()
	raw, err := clientcmd.Write(*config)
	require.NoError(t, err)
	return raw
}
