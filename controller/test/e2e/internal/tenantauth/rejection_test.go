// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantauth

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/mgmtns"
)

func TestSPIRERejectionKubeconfigUsesAdminTrustWithoutFallbackCredentials(t *testing.T) {
	t.Parallel()
	config := clientcmdapi.NewConfig()
	config.Clusters["admin"] = &clientcmdapi.Cluster{
		Server: "https://tenant.example:6443", CertificateAuthorityData: []byte("tenant-ca"), TLSServerName: "tenant.example",
		ProxyURL: "http://example-proxy", Extensions: map[string]runtime.Object{},
	}
	config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{
		ClientCertificate: "admin.crt", ClientCertificateData: []byte("example-certificate"),
		ClientKey: "admin.key", ClientKeyData: []byte("example-key"),
		Token: "example-token", TokenFile: "admin.token", Username: "admin", Password: "example-password",
		Impersonate: "admin", ImpersonateUID: "admin-uid", ImpersonateGroups: []string{"admins"},
		ImpersonateUserExtra: map[string][]string{"scope": {"admin"}},
		AuthProvider:         &clientcmdapi.AuthProviderConfig{Name: "example-provider"},
		Exec:                 &clientcmdapi.ExecConfig{Command: "example-admin-plugin"},
	}
	config.Contexts["admin"] = &clientcmdapi.Context{Cluster: "admin", AuthInfo: "admin"}
	config.CurrentContext = "admin"
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: mgmtns.Namespace(), Name: "tenant-a-admin-kubeconfig"},
		Data:       map[string][]byte{kubeconfigSecretKey: requireKubeconfig(t, config)},
	}).Build()

	raw, err := spireRejectionKubeconfig(t.Context(), c, "tenant-a")
	require.NoError(t, err)
	probe, err := clientcmd.Load(raw)
	require.NoError(t, err)
	require.Len(t, probe.Clusters, 1)
	expectedCluster := config.Clusters["admin"].DeepCopy()
	expectedCluster.ProxyURL = ""
	require.Equal(t, expectedCluster, probe.Clusters["tenant"])
	require.Len(t, probe.AuthInfos, 1)
	expectedAuth := &clientcmdapi.AuthInfo{Extensions: map[string]runtime.Object{}}
	require.Equal(t, expectedAuth, probe.AuthInfos["spiffe"])
	require.Len(t, probe.Contexts, 1)
	require.Equal(t, "tenant", probe.Contexts[probe.CurrentContext].Cluster)
	require.Equal(t, "spiffe", probe.Contexts[probe.CurrentContext].AuthInfo)
	for _, credential := range []string{"example-certificate", "example-key", "example-token", "example-password", "example-admin-plugin"} {
		require.NotContains(t, string(raw), credential)
	}
}

func TestRejectScriptRetriesInfrastructureFailures(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name        string
		statuses    string
		bearerProof bool
		attempts    int
		status      int
		commandExit int
		failure     string
		wantErr     error
	}{
		{name: "unauthorized", statuses: "401", bearerProof: true, attempts: 1, status: 401, commandExit: 1, failure: "none"},
		{name: "unauthorized with successful command", statuses: "401-success", bearerProof: true, attempts: 1, status: 401, commandExit: 0, failure: "none", wantErr: ErrSPIREProbeInfrastructure},
		{name: "unavailable then unauthorized", statuses: "503 401", bearerProof: true, attempts: 2, status: 401, commandExit: 1, failure: "none"},
		{name: "server errors exhausted", statuses: "500 503", bearerProof: true, attempts: 12, status: 503, commandExit: 1, failure: "request_failed", wantErr: ErrSPIREProbeInfrastructure},
		{name: "other HTTP status exhausted", statuses: "429", bearerProof: true, attempts: 12, status: 429, commandExit: 1, failure: "request_failed", wantErr: ErrSPIREProbeInfrastructure},
		{name: "transport failures exhausted", statuses: "transport", bearerProof: true, attempts: 12, commandExit: 7, failure: "request_failed", wantErr: ErrSPIREProbeInfrastructure},
		{name: "accepted token", statuses: "200 401", bearerProof: true, attempts: 1, status: 200, commandExit: 0, failure: "none", wantErr: ErrSPIREAuthentication},
		{name: "forbidden", statuses: "403 401", bearerProof: true, attempts: 1, status: 403, commandExit: 1, failure: "none", wantErr: ErrSPIREAuthentication},
		{name: "unauthorized without token proof", statuses: "401", attempts: 12, status: 401, commandExit: 1, failure: "request_failed", wantErr: ErrSPIREProbeInfrastructure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			message, attempts := runRejectScript(t, testCase.statuses, testCase.bearerProof)
			require.Equal(t, testCase.attempts, attempts)
			result, err := parseRejection(message)
			require.NoError(t, err)
			require.Equal(t, testCase.bearerProof, result.minted)
			require.Equal(t, testCase.status, result.status)
			require.Equal(t, testCase.commandExit, result.commandExit)
			require.Equal(t, testCase.failure, result.failure)
			err = classifyRejection(message)
			if testCase.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, testCase.wantErr)
				require.NotContains(t, err.Error(), "example-token")
			}
		})
	}
}

func runRejectScript(t *testing.T, statuses string, bearerProof bool) (string, int) {
	t.Helper()
	work := t.TempDir()
	// Keep the socket below the Unix path limit even when TMPDIR is long.
	socketDir, err := os.MkdirTemp("/tmp", "nke-spire-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(socketDir)) })
	socket := socketDir + "/spire-agent.sock"
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	plugin := work + "/mint"
	require.NoError(t, os.WriteFile(plugin, []byte(`#!/bin/sh
[ "$SPIFFE_JWT_AUDIENCE" = tenant-a ] || exit 1
# The pinned plugin emits YAML ExecCredential output.
printf 'apiVersion: client.authentication.k8s.io/v1\nkind: ExecCredential\nstatus:\n  token: "example-token"\n'
printf 'example-token private mint diagnostic\n' >&2
`), 0o700))
	request := work + "/request"
	require.NoError(t, os.WriteFile(request, []byte(`#!/bin/sh
case " $* " in *' --token=example-token '*) ;; *) exit 2 ;; esac
for arg do
  case "$arg" in --token=*) printf '%s' "${arg#--token=}" > "$REQUEST_TOKEN_FILE" ;; esac
done
attempt=0
[ -f "$REQUEST_COUNT_FILE" ] && attempt=$(cat "$REQUEST_COUNT_FILE")
attempt=$((attempt + 1))
printf '%s' "$attempt" > "$REQUEST_COUNT_FILE"
status=$(printf '%s\n' "$RESPONSE_STATUSES" | awk -v attempt="$attempt" '{ print (attempt <= NF ? $attempt : $NF) }')
if [ "$BEARER_PROOF" = true ]; then
  printf 'I1009 12:34:56.000000 1 round_trippers.go:527] "Request" verb="GET" url="https://tenant.example.test/api" headers=<map[Accept:[application/json] Authorization:[Bearer <masked>]]>\n' >&2
fi
printf 'example-token private request diagnostic\n' >&2
[ "$status" = transport ] && exit 7
command_status=1
case "$status" in
  200) command_status=0 ;;
  401-success) status=401; command_status=0 ;;
esac
printf 'I1009 12:34:56.000001 1 round_trippers.go:632] "Response" verb="GET" url="https://tenant.example.test/api" status="%s response" headers=<map[]> milliseconds=1\n' "$status" >&2
exit "$command_status"
`), 0o700))
	message := work + "/result"
	script := strings.NewReplacer(
		"/spiffe-workload-api/spire-agent.sock", socket,
		"/work", work,
		"/dev/termination-log", message,
		"kubectl ", request+" ",
		"sleep 5", "sleep 0",
	).Replace(rejectScript)
	command := exec.CommandContext(t.Context(), "sh", "-c", script)
	command.Env = append(os.Environ(), "CLUSTER=tenant-a", "SPIRE_KUBECONFIG=example-kubeconfig", "PLUGIN_INSTALL_PATH="+plugin,
		"REQUEST_TOKEN_FILE="+work+"/sent-token", "REQUEST_COUNT_FILE="+work+"/attempts", "RESPONSE_STATUSES="+statuses, "BEARER_PROOF="+strconv.FormatBool(bearerProof))
	output, err := command.CombinedOutput()
	require.NoError(t, err)
	require.Empty(t, string(output))
	sentToken, err := os.ReadFile(work + "/sent-token")
	require.NoError(t, err)
	require.Equal(t, "example-token", string(sentToken))
	summary, err := os.ReadFile(message)
	require.NoError(t, err)
	require.NotContains(t, string(summary), "example-token")
	require.NotContains(t, string(summary), "example-kubeconfig")
	attemptsRaw, err := os.ReadFile(work + "/attempts")
	require.NoError(t, err)
	attempts, err := strconv.Atoi(string(attemptsRaw))
	require.NoError(t, err)
	return string(summary), attempts
}

func TestParseRejectionClassifiesOnlyStructuredSummary(t *testing.T) {
	t.Parallel()
	result, err := parseRejection("MINTED yes\nHTTP 401\nCOMMAND 1\nFAILURE none\n")
	if err != nil {
		t.Fatalf("parseRejection() error = %v", err)
	}
	if !result.minted || result.status != 401 || result.commandExit != 1 {
		t.Fatalf("parseRejection() = %#v", result)
	}
	for _, input := range []string{
		"MINTED yes\nHTTP 401\n",
		"MINTED yes\nHTTP 401\nCOMMAND 1\n",
		"MINTED token-value\nHTTP 401\nCOMMAND 1\n",
		"MINTED yes\nHTTP 401 token-value\nCOMMAND 1\n",
		"MINTED no\nHTTP none\nCOMMAND 1\nFAILURE token-value\n",
	} {
		if _, err = parseRejection(input); err == nil || strings.Contains(err.Error(), "token-value") {
			t.Fatalf("parseRejection(%q) error = %v", input, err)
		}
	}
}

func TestVerifySPIRERejectedPreservesPodCreationFailure(t *testing.T) {
	t.Parallel()
	config := clientcmdapi.NewConfig()
	config.Clusters["tenant"] = &clientcmdapi.Cluster{Server: "https://tenant.example", CertificateAuthorityData: []byte("example-ca")}
	config.Contexts["tenant"] = &clientcmdapi.Context{Cluster: "tenant"}
	config.CurrentContext = "tenant"
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	podError := errors.New("example-create-failure")
	id := Identity{
		Namespace: "controller", ServiceAccount: "controller",
		CABundleConfigMap: "exchange-ca", JWTExchangeEndpoint: "https://identity.example/v1/exchange",
	}
	positive := buildPod(id, "tenant-b", "example-user", nil)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: mgmtns.Namespace(), Name: "tenant-a-admin-kubeconfig"},
		Data:       map[string][]byte{kubeconfigSecretKey: requireKubeconfig(t, config)},
	}).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, object client.Object, _ ...client.CreateOption) error {
			pod := object.(*corev1.Pod)
			require.Contains(t, pod.Spec.Containers[0].Env, corev1.EnvVar{Name: "CLUSTER", Value: "tenant-a"})
			require.Contains(t, pod.Spec.Containers[0].Env, corev1.EnvVar{Name: "PLUGIN_INSTALL_PATH", Value: pluginInstallPath})
			require.Contains(t, pod.Spec.Containers[0].Env, corev1.EnvVar{Name: "SPIFFE_JWT_EXCHANGE_ENDPOINT", Value: id.JWTExchangeEndpoint})
			require.Contains(t, pod.Spec.Containers[0].Env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: exchangeCAPath})
			require.Equal(t, positive.Spec.Containers[0].VolumeMounts, pod.Spec.Containers[0].VolumeMounts)
			require.Equal(t, positive.Spec.Volumes, pod.Spec.Volumes)
			return podError
		},
	}).Build()
	err := VerifySPIRERejected(t.Context(), c, id, "tenant-a")
	require.ErrorIs(t, err, ErrSPIREProbeInfrastructure)
	require.ErrorIs(t, err, podError)
	require.ErrorContains(t, err, "example-create-failure")
}

func TestClassifyRejectionRejectsNonterminalHTTPStatus(t *testing.T) {
	t.Parallel()
	err := classifyRejection("MINTED yes\nHTTP 503\nCOMMAND 1\nFAILURE none\n")
	require.ErrorIs(t, err, ErrSPIREProbeInfrastructure)
}

func TestRejectionPodReusesCredentialPodSetup(t *testing.T) {
	t.Parallel()
	pod := buildCredentialPod(Identity{Namespace: "controller", ServiceAccount: "controller"}, "tenant", rejectScript, nil, nil)
	if len(pod.Spec.InitContainers) != 1 || len(pod.Spec.Containers) != 1 {
		t.Fatalf("credential pod containers = %#v", pod.Spec)
	}
	if !strings.Contains(strings.Join(pod.Spec.Containers[0].Command, " "), "MINTED") {
		t.Fatalf("rejection command = %q", pod.Spec.Containers[0].Command)
	}
	if pod.Spec.Containers[0].Env[0].Name != "CLUSTER" || pod.Spec.Containers[0].Env[0].Value != "tenant" {
		t.Fatalf("rejection env = %#v", pod.Spec.Containers[0].Env)
	}
}
