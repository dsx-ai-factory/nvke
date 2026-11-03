// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/metrics"
)

// testAdminKubeconfig is shaped like a kubeadm admin.conf.
const testAdminKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: kubernetes
  cluster:
    server: https://198.51.100.10:6443
    certificate-authority-data: ` + testCAData + `
contexts:
- name: kubernetes-admin@kubernetes
  context:
    cluster: kubernetes
    user: kubernetes-admin
current-context: kubernetes-admin@kubernetes
users:
- name: kubernetes-admin
  user:
    client-certificate-data: Y2VydA==
    client-key-data: a2V5
`

func TestSanitizeAdminKubeconfigKeepsServerCAAndClientCertificate(t *testing.T) {
	out, err := SanitizeAdminKubeconfig([]byte(testAdminKubeconfig))
	require.NoError(t, err)

	cfg, err := clientcmd.Load(out)
	require.NoError(t, err)
	require.Equal(t, "kubernetes-admin@kubernetes", cfg.CurrentContext)
	require.Len(t, cfg.Clusters, 1)
	require.Len(t, cfg.AuthInfos, 1)
	require.Equal(t, "https://198.51.100.10:6443", cfg.Clusters["kubernetes"].Server)
	require.NotEmpty(t, cfg.Clusters["kubernetes"].CertificateAuthorityData)
	require.Equal(t, []byte("cert"), cfg.AuthInfos["kubernetes-admin"].ClientCertificateData)
	require.Equal(t, []byte("key"), cfg.AuthInfos["kubernetes-admin"].ClientKeyData)

	again, err := SanitizeAdminKubeconfig(out)
	require.NoError(t, err)
	require.Equal(t, out, again)
}

func TestSanitizeAdminKubeconfigDropsUnusedEntries(t *testing.T) {
	raw := strings.Replace(testAdminKubeconfig, "users:\n", `users:
- name: other
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /bin/true
`, 1)

	out, err := SanitizeAdminKubeconfig([]byte(raw))
	require.NoError(t, err)
	require.NotContains(t, string(out), "exec")
	require.NotContains(t, string(out), "other")
}

func TestSanitizeAdminKubeconfigRejects(t *testing.T) {
	userField := func(field string) string {
		return strings.Replace(testAdminKubeconfig, "    client-key-data: a2V5\n", "    client-key-data: a2V5\n"+field, 1)
	}
	clusterField := func(field string) string {
		return strings.Replace(testAdminKubeconfig, "    server: https://198.51.100.10:6443\n", "    server: https://198.51.100.10:6443\n"+field, 1)
	}
	contextField := func(field string) string {
		return strings.Replace(testAdminKubeconfig, "    user: kubernetes-admin\n", "    user: kubernetes-admin\n"+field, 1)
	}
	withServer := func(server string) string {
		return strings.Replace(testAdminKubeconfig, "https://198.51.100.10:6443", server, 1)
	}
	const (
		userOnly    = "user may set only client-certificate-data and client-key-data"
		clusterOnly = "cluster may set only server and certificate-authority-data"
		httpsOnly   = "must be https://host[:port]"
		contextOnly = "context may set only a cluster and a user"
		extension   = `
      extensions:
      - name: example
        extension: {}
`
	)

	tests := map[string]struct {
		raw  string
		want string
	}{
		"exec plugin": {userField(`    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /bin/true
`), userOnly},
		"auth provider": {userField(`    auth-provider:
      name: oidc
`), userOnly},
		"token":                         {userField("    token: abc\n"), userOnly},
		"token file":                    {userField("    tokenFile: /tmp/token\n"), userOnly},
		"client certificate path":       {userField("    client-certificate: /tmp/client.crt\n"), userOnly},
		"client key path":               {userField("    client-key: /tmp/client.key\n"), userOnly},
		"username":                      {userField("    username: admin\n"), userOnly},
		"password":                      {userField("    password: admin\n"), userOnly},
		"impersonated user":             {userField("    as: user-a\n"), userOnly},
		"impersonated UID":              {userField("    as-uid: uid-a\n"), userOnly},
		"impersonated groups":           {userField("    as-groups: [group-a]\n"), userOnly},
		"impersonated user extra":       {userField("    as-user-extra:\n      key: [value]\n"), userOnly},
		"user extensions":               {userField(strings.ReplaceAll(extension[1:], "      ", "    ")), userOnly},
		"certificate without key":       {strings.Replace(testAdminKubeconfig, "    client-key-data: a2V5\n", "", 1), "must set both"},
		"certificate authority path":    {clusterField("    certificate-authority: /tmp/ca.crt\n"), clusterOnly},
		"insecure skip TLS verify":      {clusterField("    insecure-skip-tls-verify: true\n"), clusterOnly},
		"TLS server name":               {clusterField("    tls-server-name: other.example\n"), clusterOnly},
		"proxy":                         {clusterField("    proxy-url: http://proxy.example:3128\n"), clusterOnly},
		"cluster extensions":            {clusterField(strings.ReplaceAll(extension[1:], "      ", "    ")), clusterOnly},
		"missing CA data":               {strings.Replace(testAdminKubeconfig, "    certificate-authority-data: "+testCAData+"\n", "", 1), "no certificate-authority-data"},
		"http server":                   {withServer("http://198.51.100.10:6443"), httpsOnly},
		"server with path":              {withServer("https://198.51.100.10:6443/proxy"), httpsOnly},
		"server with query":             {withServer("https://198.51.100.10:6443?x=1"), httpsOnly},
		"server with fragment":          {withServer("https://198.51.100.10:6443#x"), httpsOnly},
		"server with userinfo":          {withServer("https://user@198.51.100.10:6443"), httpsOnly},
		"server with no host":           {withServer("https:///"), httpsOnly},
		"missing current context":       {strings.Replace(testAdminKubeconfig, "current-context: kubernetes-admin@kubernetes\n", "", 1), "current context"},
		"context with a namespace":      {contextField("    namespace: kube-system\n"), contextOnly},
		"context extensions":            {contextField(strings.ReplaceAll(extension[1:], "      ", "    ")), contextOnly},
		"current context names no user": {strings.Replace(testAdminKubeconfig, "    user: kubernetes-admin\n", "    user: missing\n", 1), `user "missing" not found`},
		"oversized":                     {testAdminKubeconfig + "# " + strings.Repeat("x", maxKubeconfigBytes) + "\n", "byte limit"},
		"not a kubeconfig":              {"not: [valid", "failed to parse kubeconfig"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := SanitizeAdminKubeconfig([]byte(tc.raw))
			require.ErrorContains(t, err, tc.want)
			_, err = NewRESTConfigFromAdminKubeconfig([]byte(tc.raw))
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestNewRESTConfigFromAdminKubeconfigUsesOnlyInlineData(t *testing.T) {
	restConfig, err := NewRESTConfigFromAdminKubeconfig([]byte(testAdminKubeconfig))
	require.NoError(t, err)
	require.Equal(t, "https://198.51.100.10:6443", restConfig.Host)
	require.Nil(t, restConfig.ExecProvider)
	require.Nil(t, restConfig.AuthProvider)
	require.Empty(t, restConfig.BearerToken)
	require.Empty(t, restConfig.BearerTokenFile)
	require.Empty(t, restConfig.CAFile)
	require.Empty(t, restConfig.CertFile)
	require.Empty(t, restConfig.KeyFile)
	require.Nil(t, restConfig.Proxy)
	require.False(t, restConfig.Insecure)
	require.Equal(t, []byte("cert"), restConfig.CertData)
	require.Equal(t, []byte("key"), restConfig.KeyData)
}

// A node's admin.conf may not name the SPIRE exec plugin: only the controller
// writes that form.
func TestNewRESTConfigFromAdminKubeconfigRejectsSPIREExec(t *testing.T) {
	_, err := NewRESTConfigFromAdminKubeconfig(newTenantClientProviderExecKubeconfig())
	require.Error(t, err)
}

func TestNewRESTConfigFromKubeconfigAcceptsSanitizedAdminKubeconfig(t *testing.T) {
	sanitized, err := SanitizeAdminKubeconfig([]byte(testAdminKubeconfig))
	require.NoError(t, err)

	restConfig, err := NewRESTConfigFromKubeconfig(sanitized, "tenant-a")
	require.NoError(t, err)
	require.Nil(t, restConfig.ExecProvider)
	require.Equal(t, "https://198.51.100.10:6443", restConfig.Host)
}

func TestNewRESTConfigFromKubeconfigRejectsOtherExec(t *testing.T) {
	spire := string(newTenantClientProviderExecKubeconfig())
	const (
		execOnly   = "must be " + SPIREExecCommand
		spireOnly  = "SPIRE user may set only exec"
		execMarker = "      interactiveMode: Never\n"
	)
	tests := map[string]struct {
		raw  string
		want string
	}{
		"other command":                {strings.Replace(spire, SPIREExecCommand, "/bin/true", 1), execOnly},
		"arguments":                    {strings.Replace(spire, execMarker, execMarker+"      args: [\"--flag\"]\n", 1), execOnly},
		"cluster info":                 {strings.Replace(spire, execMarker, execMarker+"      provideClusterInfo: true\n", 1), execOnly},
		"other environment variable":   {strings.Replace(spire, "      env:\n", "      env:\n      - name: EXTRA_VAR\n        value: x\n", 1), "environment variable EXTRA_VAR"},
		"audience for another cluster": {strings.Replace(spire, "value: tenant-a", "value: tenant-b", 1), "does not match cluster"},
		"token file":                   {strings.Replace(spire, "  user:\n", "  user:\n    tokenFile: /tmp/token\n", 1), spireOnly},
		"client certificate data":      {strings.Replace(spire, "  user:\n", "  user:\n    client-certificate-data: Y2VydA==\n", 1), spireOnly},
		"auth provider":                {strings.Replace(spire, "  user:\n", "  user:\n    auth-provider:\n      name: oidc\n", 1), spireOnly},
		"missing CA data":              {strings.Replace(spire, "    certificate-authority-data: "+testCAData+"\n", "", 1), "no certificate-authority-data"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewRESTConfigFromKubeconfig([]byte(tc.raw), "tenant-a")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// shellExecKubeconfig names an exec plugin that creates marker when it runs.
func shellExecKubeconfig(marker string) string {
	return strings.Replace(testAdminKubeconfig, `    client-certificate-data: Y2VydA==
    client-key-data: a2V5
`, `    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /bin/sh
      args: ["-c", "touch `+marker+`"]
      interactiveMode: Never
`, 1)
}

func TestClientFromControlPlaneNodeAgentDoesNotRunExecPlugin(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	cluster := newSiteDBClusterForTenantClientProviderTest()
	node := db.Node{ID: uuid.New(), Name: "cp-0", ClusterID: cluster.ID, ControlPlane: true}
	node.Conditions.Joined = db.Condition{Status: db.ConditionStatusTrue}
	nodeAgentClient := &agenttest.FakeClient{Kubeconfigs: map[uuid.UUID]string{node.ID: shellExecKubeconfig(marker)}}
	provider := NewDBClusterTenantClientProvider(nil, nil, nodeAgentClient, false, constants.LegacyManagementNamespace)

	before := tenantKubeconfigRejected(t, metrics.TenantKubeconfigSourceNodeAgent)
	_, err := provider.clientFromControlPlaneNodeAgent(context.Background(), cluster, []db.Node{node})
	require.ErrorIs(t, err, ErrInvalidKubeconfig)
	require.NoFileExists(t, marker)
	require.InDelta(t, before+1, tenantKubeconfigRejected(t, metrics.TenantKubeconfigSourceNodeAgent), 0)
}

func TestClientFromTenantAccessSecretDoesNotRunExecPlugin(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	cluster := newSiteDBClusterForTenantClientProviderTest()
	managementClient := fake.NewClientBuilder().
		WithScheme(newTenantClientProviderTestScheme(t)).
		WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       constants.LegacyManagementNamespace,
				Name:            cluster.Name + "-kubeconfig",
				ResourceVersion: "1",
			},
			Data: map[string][]byte{tenantAccessKubeconfigKey: []byte(shellExecKubeconfig(marker))},
		}).
		Build()
	provider := NewDBClusterTenantClientProvider(nil, managementClient, nil, true, constants.LegacyManagementNamespace)

	before := tenantKubeconfigRejected(t, metrics.TenantKubeconfigSourceSecret)
	_, err := provider.ClientForDBCluster(context.Background(), cluster)
	require.ErrorIs(t, err, ErrInvalidKubeconfig)
	require.NoFileExists(t, marker)
	require.InDelta(t, before+1, tenantKubeconfigRejected(t, metrics.TenantKubeconfigSourceSecret), 0)
}

// tenantKubeconfigRejected reads nvke_controller_tenant_kubeconfig_rejected_total
// for source from the default registry.
func tenantKubeconfigRejected(t *testing.T, source string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "nvke_controller_tenant_kubeconfig_rejected_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "source" && label.GetValue() == source {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("no %s series for source %q", "nvke_controller_tenant_kubeconfig_rejected_total", source)
	return 0
}
