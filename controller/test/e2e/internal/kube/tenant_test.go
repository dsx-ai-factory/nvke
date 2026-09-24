// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/remotecommand"
)

func TestExpiringJWTSourceCachesAndRefreshesBeforeExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	mintCount := 0
	source := newExpiringJWTSource(func(context.Context) (string, error) {
		mintCount++
		return testJWT(now.Add(time.Hour), fmt.Sprintf("token-%d", mintCount)), nil
	}, func() time.Time { return now })

	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token() error = %v", err)
	}
	second, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token() error = %v", err)
	}
	if first != second || mintCount != 1 {
		t.Fatalf("cached Token() = (%q, %q), mint count = %d; want same token and one mint", first, second, mintCount)
	}

	now = now.Add(time.Hour - time.Minute)
	refreshed, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("refresh Token() error = %v", err)
	}
	if refreshed == first || mintCount != 2 {
		t.Fatalf("refreshed Token() = %q, mint count = %d; want a new token and two mints", refreshed, mintCount)
	}
}

func TestExpiringJWTSourceHonorsContextWhileWaitingForMint(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	mintStarted := make(chan struct{})
	releaseMint := make(chan struct{})
	source := newExpiringJWTSource(func(context.Context) (string, error) {
		close(mintStarted)
		<-releaseMint
		return testJWT(now.Add(time.Hour), "first"), nil
	}, func() time.Time { return now })

	done := make(chan error, 1)
	go func() {
		_, err := source.Token(context.Background())
		done <- err
	}()
	<-mintStarted

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.Token(ctx); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Token(canceled context) error = %v, want context canceled", err)
	}

	close(releaseMint)
	if err := <-done; err != nil {
		t.Fatalf("in-flight Token() error = %v", err)
	}
}

func TestExpiringJWTSourceRejectsInvalidTokens(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := map[string]struct {
		token   string
		wantErr string
	}{
		"malformed":   {token: "not-a-jwt", wantErr: "want 3"},
		"missing exp": {token: testJWTClaims(`{"sub":"spiffe://test/workload"}`), wantErr: "no valid exp"},
		"expired":     {token: testJWT(now.Add(-time.Second), "expired"), wantErr: "expired at"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			source := newExpiringJWTSource(
				func(context.Context) (string, error) { return test.token, nil },
				func() time.Time { return now },
			)
			if _, err := source.Token(context.Background()); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Token() error = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestJWTExpirationAcceptsRFCNumericDate(t *testing.T) {
	tests := map[string]struct {
		claim string
		want  time.Time
	}{
		"integer": {
			claim: `1788471177`,
			want:  time.Unix(1_788_471_177, 0),
		},
		"scientific notation": {
			claim: `1.788471177e+09`,
			want:  time.Unix(1_788_471_177, 0),
		},
		"fractional seconds": {
			claim: `1788471177.5`,
			want:  time.Unix(1_788_471_177, 500_000_000),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			token := testJWTClaims(`{"exp":` + test.claim + `}`)
			got, err := jwtExpiration(token)
			if err != nil {
				t.Fatalf("jwtExpiration() error = %v", err)
			}
			if !got.Equal(test.want) {
				t.Fatalf("jwtExpiration() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestConcurrentTokenCallsShareOneMint(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	releaseMint := make(chan struct{})
	mintStarted := make(chan struct{})
	var mintCalls atomic.Int32
	var closeStarted sync.Once
	source := newExpiringJWTSource(func(context.Context) (string, error) {
		mintCalls.Add(1)
		closeStarted.Do(func() { close(mintStarted) })
		<-releaseMint
		return testJWT(now.Add(time.Hour), "shared"), nil
	}, func() time.Time { return now })

	const callers = 16
	start := make(chan struct{})
	results := make([]string, callers)
	errs := make([]error, callers)
	var waitGroup sync.WaitGroup
	waitGroup.Add(callers)
	for i := range callers {
		go func() {
			defer waitGroup.Done()
			<-start
			results[i], errs[i] = source.Token(context.Background())
		}()
	}
	close(start)
	<-mintStarted
	close(releaseMint)
	waitGroup.Wait()

	if got := mintCalls.Load(); got != 1 {
		t.Fatalf("mint count = %d, want 1", got)
	}
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d error = %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Fatalf("caller %d token differs from caller 0", i)
		}
	}
}

func TestJWTBearerRoundTripperRefreshesAndRetriesAfterUnauthorized(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	mintCount := 0
	source := newExpiringJWTSource(func(context.Context) (string, error) {
		mintCount++
		return testJWT(now.Add(time.Hour), fmt.Sprintf("token-%d", mintCount)), nil
	}, func() time.Time { return now })

	var authorizations []string
	transport := &jwtBearerRoundTripper{
		source: source,
		delegate: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			authorizations = append(authorizations, request.Header.Get("Authorization"))
			status := http.StatusOK
			if len(authorizations) == 1 {
				status = http.StatusUnauthorized
			}
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(strings.NewReader("response")),
				Header:     make(http.Header),
				Request:    request,
			}, nil
		}),
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://tenant.example/api/v1/nodes", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("RoundTrip() status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if mintCount != 2 {
		t.Fatalf("mint count = %d, want 2", mintCount)
	}
	if len(authorizations) != 2 || authorizations[0] == authorizations[1] {
		t.Fatalf("Authorization headers = %q, want two distinct bearer tokens", authorizations)
	}
	if request.Header.Get("Authorization") != "" {
		t.Fatalf("original request Authorization = %q, want unchanged", request.Header.Get("Authorization"))
	}
}

func TestTenantRESTConfigReplacesSPIREExecCredentials(t *testing.T) {
	kubeconfig := testSPIREKubeconfig(t, "tenant-a")
	factory := newTestTenantClientFactory(t,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: tenantKubeconfigNamespace, Name: "tenant-a-kubeconfig"},
			Data:       map[string][]byte{tenantKubeconfigKey: kubeconfig},
		},
		spireServerConfigMapObject(`{"server":{"trust_domain":"test.nke.local"}}`),
	)

	config, err := factory.tenantRESTConfig(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("tenantRESTConfig() error = %v", err)
	}
	if config.Host != "https://tenant.example:6443" {
		t.Fatalf("Host = %q, want tenant endpoint", config.Host)
	}
	if string(config.CAData) != "tenant-ca" {
		t.Fatalf("CAData = %q, want published tenant CA", config.CAData)
	}
	if config.ExecProvider != nil || config.AuthProvider != nil || config.BearerToken != "" || config.BearerTokenFile != "" ||
		config.Username != "" || config.Password != "" || len(config.CertData) != 0 || len(config.KeyData) != 0 {
		t.Fatalf("tenant config retained published user credentials: %#v", config)
	}
	if config.WrapTransport == nil {
		t.Fatal("tenant config has no JWT transport wrapper")
	}
}

func TestRelayEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name       string
		host       string
		serverName string
		want       string
		wantAsked  string
		wantServer string
	}{
		{
			name: "substitutes the address and pins the published host",
			host: "https://10.91.194.69:6443", wantAsked: "10.91.194.69:6443",
			want: "https://127.0.0.1:40000", wantServer: "10.91.194.69",
		},
		{
			// A kubeconfig may leave the port implicit. The relay is asked
			// for a host:port either way, so it never has to guess.
			name: "supplies the default port when the URL omits it",
			host: "https://tenant.example", wantAsked: "tenant.example:443",
			want: "https://127.0.0.1:40000", wantServer: "tenant.example",
		},
		{
			// A published tls-server-name is the cluster's own statement
			// about which name its certificate covers, and beats inference.
			name: "leaves an explicit server name alone",
			host: "https://10.91.194.69:6443", serverName: "tenant.example",
			wantAsked: "10.91.194.69:6443",
			want:      "https://127.0.0.1:40000", wantServer: "tenant.example",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked string
			factory := &TenantClientFactory{relay: func(_ context.Context, hostPort string) (string, error) {
				asked = hostPort
				return "127.0.0.1:40000", nil
			}}
			config := &rest.Config{Host: tc.host}
			config.ServerName = tc.serverName

			if err := factory.relayEndpoint(context.Background(), config); err != nil {
				t.Fatalf("relayEndpoint() error = %v", err)
			}
			if asked != tc.wantAsked {
				t.Errorf("relay asked for %q, want %q", asked, tc.wantAsked)
			}
			if config.Host != tc.want {
				t.Errorf("Host = %q, want %q", config.Host, tc.want)
			}
			if config.ServerName != tc.wantServer {
				t.Errorf("ServerName = %q, want %q", config.ServerName, tc.wantServer)
			}
		})
	}
}

func TestRelayEndpointWithoutARelay(t *testing.T) {
	// The lanes that share a network with their clusters set no relay, and
	// must keep talking to the address the tenant published.
	factory := &TenantClientFactory{}
	config := &rest.Config{Host: "https://tenant.example:6443"}

	if err := factory.relayEndpoint(context.Background(), config); err != nil {
		t.Fatalf("relayEndpoint() error = %v", err)
	}
	if config.Host != "https://tenant.example:6443" || config.ServerName != "" {
		t.Errorf("config changed without a relay: %q, server name %q", config.Host, config.ServerName)
	}
}

func TestRelayEndpointFailureNamesTheCluster(t *testing.T) {
	factory := newTestTenantClientFactory(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantKubeconfigNamespace, Name: "tenant-a-kubeconfig"},
		Data:       map[string][]byte{tenantKubeconfigKey: testSPIREKubeconfig(t, "tenant-a")},
	})
	factory.relay = func(context.Context, string) (string, error) {
		return "", errors.New("the tunnel is shut")
	}

	_, err := factory.tenantRESTConfig(context.Background(), "tenant-a")
	if err == nil || !strings.Contains(err.Error(), "tenant-a") || !strings.Contains(err.Error(), "the tunnel is shut") {
		t.Fatalf("error = %v, want one naming the cluster and the cause", err)
	}
}

func TestTenantRESTConfigRelaysTheSPIREEndpoint(t *testing.T) {
	// The SPIRE path rebuilds the config with AnonymousClientConfig, so this
	// is the one that would silently drop the substitution.
	factory := newTestTenantClientFactory(t,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: tenantKubeconfigNamespace, Name: "tenant-a-kubeconfig"},
			Data:       map[string][]byte{tenantKubeconfigKey: testSPIREKubeconfig(t, "tenant-a")},
		},
		spireServerConfigMapObject(`{"server":{"trust_domain":"test.nke.local"}}`),
	)
	factory.relay = func(context.Context, string) (string, error) { return "127.0.0.1:40000", nil }

	config, err := factory.tenantRESTConfig(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("tenantRESTConfig() error = %v", err)
	}
	if config.Host != "https://127.0.0.1:40000" {
		t.Errorf("Host = %q, want the relayed address", config.Host)
	}
	if config.ServerName != "tenant.example" {
		t.Errorf("ServerName = %q, want the published host", config.ServerName)
	}
	if string(config.CAData) != "tenant-ca" {
		t.Errorf("CAData = %q, want the published tenant CA", config.CAData)
	}
}

func TestTenantRESTConfigExchangesSPIREJWTCredentials(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	sourceToken := testJWT(now.Add(time.Hour), "source")
	exchangedToken := testJWT(now.Add(time.Hour), "exchanged")

	var exchangeForm url.Values
	exchangeServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		exchangeForm = request.PostForm
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"access_token":%q,"issued_token_type":%q,"token_type":"Bearer"}`,
			exchangedToken, tokenTypeJWT)
	}))
	defer exchangeServer.Close()

	factory := newTestTenantClientFactory(t,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: tenantKubeconfigNamespace, Name: "tenant-a-kubeconfig"},
			Data: map[string][]byte{tenantKubeconfigKey: testSPIREExchangeKubeconfig(
				t, "tenant-a", exchangeServer.URL+"/v1/exchange",
			)},
		},
		spireServerConfigMapObject(`{"server":{"trust_domain":"test.nke.local"}}`),
		spireServerPod("spire-server-1", true, spireServerSelector()),
	)
	factory.now = func() time.Time { return now }
	caBundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: exchangeServer.Certificate().Raw})
	if err := WithJWTExchangeCABundle(caBundle)(factory); err != nil {
		t.Fatalf("WithJWTExchangeCABundle() error = %v", err)
	}

	var mintURL *url.URL
	factory.newExecutor = func(_ string, requestURL *url.URL) (remotecommand.Executor, error) {
		copyURL := *requestURL
		mintURL = &copyURL
		return &fakeExecutor{stdout: sourceToken + "\n"}, nil
	}

	config, err := factory.tenantRESTConfig(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("tenantRESTConfig() error = %v", err)
	}
	var authorization string
	transport := config.WrapTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		authorization = request.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	}))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://tenant.example:6443/api", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	defer response.Body.Close()

	if authorization != "Bearer "+exchangedToken {
		t.Fatalf("Authorization = %q, want exchanged token", authorization)
	}
	if exchangeForm.Get("grant_type") != tokenExchangeGrantType ||
		exchangeForm.Get("subject_token") != sourceToken ||
		exchangeForm.Get("subject_token_type") != tokenTypeJWT ||
		exchangeForm.Get("requested_token_type") != tokenTypeJWT {
		t.Fatalf("exchange form = %#v, want RFC 8693 JWT exchange", exchangeForm)
	}
	if mintURL == nil {
		t.Fatal("source JWT was not minted")
	}
	wantSourceID := "spiffe://test.nke.local/tenant/nke/service/cloud-workflow-engine/bm/cwe"
	if !slices.Contains(mintURL.Query()["command"], wantSourceID) {
		t.Fatalf("source JWT mint command = %#v, want %q", mintURL.Query()["command"], wantSourceID)
	}
}

func TestTenantRESTConfigPreservesAdminCredentials(t *testing.T) {
	kubeconfig := testAdminKubeconfig(t)
	factory := newTestTenantClientFactory(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantKubeconfigNamespace, Name: "tenant-a-kubeconfig"},
		Data:       map[string][]byte{tenantKubeconfigKey: kubeconfig},
	})

	config, err := factory.tenantRESTConfig(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("tenantRESTConfig() error = %v", err)
	}
	if config.Host != "https://tenant.example:6443" || string(config.CAData) != "tenant-ca" {
		t.Fatalf("tenant endpoint or CA was not preserved: %#v", config)
	}
	if string(config.CertData) != "admin-certificate" || string(config.KeyData) != "admin-key" {
		t.Fatalf("admin client credentials were not preserved: %#v", config)
	}
	if config.WrapTransport != nil {
		t.Fatal("admin config unexpectedly has a custom JWT transport")
	}
}

func TestGetAdminClientReadsPublishedKubeconfigAndRegistersVerifierSchemes(t *testing.T) {
	factory := newTestTenantClientFactory(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantKubeconfigNamespace, Name: "tenant-a-kubeconfig"},
		Data:       map[string][]byte{tenantKubeconfigKey: testClientKubeconfig(t)},
	})

	tenantClient, err := factory.GetAdminClient(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("GetAdminClient() error = %v", err)
	}
	for _, gvk := range []schema.GroupVersionKind{
		appsv1.SchemeGroupVersion.WithKind("ReplicaSet"),
		coordinationv1.SchemeGroupVersion.WithKind("Lease"),
		corev1.SchemeGroupVersion.WithKind("Pod"),
		networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"),
		policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"),
	} {
		if !tenantClient.Scheme().Recognizes(gvk) {
			t.Errorf("GetAdminClient() scheme does not recognize %s", gvk)
		}
	}
}

func TestGetAdminClientReportsMissingKubeconfig(t *testing.T) {
	factory := newTestTenantClientFactory(t)

	_, err := factory.GetAdminClient(context.Background(), "tenant-a")
	if err == nil || !strings.Contains(err.Error(), "getting tenant kubeconfig secret nke-system/tenant-a-kubeconfig") {
		t.Fatalf("GetAdminClient() error = %v, want contextual Secret error", err)
	}
}

// On an OIDC cluster the published kubeconfig authenticates as the CWE
// identity, which is deliberately not cluster-admin. GetAdminClient must mint
// the capi-manager identity instead, or every caller gets a 403 on test setup.
func TestAdminAndCWEPathsMintDistinctIdentities(t *testing.T) {
	objects := []runtime.Object{
		spireServerConfigMapObject(`{"server":{"trust_domain":"test.nke.local"}}`),
		spireServerPod("spire-server-1", true, spireServerSelector()),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: tenantKubeconfigNamespace, Name: "tenant-a-kubeconfig"},
			Data:       map[string][]byte{tenantKubeconfigKey: testSPIREKubeconfig(t, "tenant-a")},
		},
	}
	factory := newTestTenantClientFactory(t, objects...)

	var gotURL *url.URL
	factory.newExecutor = func(_ string, requestURL *url.URL) (remotecommand.Executor, error) {
		copyURL := *requestURL
		gotURL = &copyURL
		return &fakeExecutor{stdout: testJWT(time.Now().Add(time.Hour), "minted") + "\n"}, nil
	}

	// The token is minted lazily, so drive one request through each transport
	// and read the SPIFFE ID the exec command asked SPIRE to mint.
	mintedFor := func(t *testing.T, config *rest.Config) []string {
		t.Helper()
		gotURL = nil
		transport := config.WrapTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		}))
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://tenant.example:6443/api", http.NoBody)
		if err != nil {
			t.Fatalf("http.NewRequestWithContext() error = %v", err)
		}
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Fatalf("RoundTrip() error = %v", err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatalf("closing response body: %v", err)
		}
		if gotURL == nil {
			t.Fatal("no JWT was minted")
		}
		return gotURL.Query()["command"]
	}

	adminConfig, err := factory.tenantRESTConfigAs(context.Background(), "tenant-a", adminSPIFFEPath)
	if err != nil {
		t.Fatalf("tenantRESTConfigAs(admin) error = %v", err)
	}
	if want := "spiffe://test.nke.local" + adminSPIFFEPath; !slices.Contains(mintedFor(t, adminConfig), want) {
		t.Fatalf("admin path did not mint %q", want)
	}

	// The GetClient path must keep minting the CWE identity. Pointing it at the
	// admin identity would silently give every tenant read cluster-admin.
	cweConfig, err := factory.tenantRESTConfig(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("tenantRESTConfig() error = %v", err)
	}
	command := mintedFor(t, cweConfig)
	if want := "spiffe://test.nke.local" + cweSPIFFEPath; !slices.Contains(command, want) {
		t.Fatalf("GetClient path did not mint %q, got %#v", want, command)
	}
	if slices.Contains(command, "spiffe://test.nke.local"+adminSPIFFEPath) {
		t.Fatal("GetClient path minted the cluster-admin identity")
	}
}

func TestSPIREAudience(t *testing.T) {
	tests := map[string]struct {
		env     []clientcmdapi.ExecEnvVar
		want    string
		wantErr string
	}{
		"extracts": {
			env:  []clientcmdapi.ExecEnvVar{{Name: spireAudienceEnv, Value: " tenant-a "}},
			want: "tenant-a",
		},
		"missing": {wantErr: "is not set"},
		"empty": {
			env:     []clientcmdapi.ExecEnvVar{{Name: spireAudienceEnv, Value: " "}},
			wantErr: "is empty",
		},
		"conflicting": {
			env: []clientcmdapi.ExecEnvVar{
				{Name: spireAudienceEnv, Value: "tenant-a"},
				{Name: spireAudienceEnv, Value: "tenant-b"},
			},
			wantErr: "conflicting values",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := spireAudience(&clientcmdapi.ExecConfig{Env: test.env})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("spireAudience() error = %v, want error containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("spireAudience() = (%q, %v), want (%q, nil)", got, err, test.want)
			}
		})
	}
}

func TestTenantRESTConfigRejectsSPIREExecWithoutAudience(t *testing.T) {
	factory := newTestTenantClientFactory(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantKubeconfigNamespace, Name: "tenant-a-kubeconfig"},
		Data:       map[string][]byte{tenantKubeconfigKey: testSPIREKubeconfig(t, "")},
	})

	_, err := factory.tenantRESTConfig(context.Background(), "tenant-a")
	if err == nil || !strings.Contains(err.Error(), "SPIFFE_JWT_AUDIENCE is not set") {
		t.Fatalf("tenantRESTConfig() error = %v, want missing audience error", err)
	}
}

func TestTenantRESTConfigReportsMissingKubeconfigSecret(t *testing.T) {
	factory := newTestTenantClientFactory(t)

	_, err := factory.tenantRESTConfig(context.Background(), "tenant-a")
	if err == nil || !strings.Contains(err.Error(), "getting tenant kubeconfig secret nke-system/tenant-a-kubeconfig") {
		t.Fatalf("tenantRESTConfig() error = %v, want contextual Secret error", err)
	}
}

func TestMintJWTSelectsReadyPodAndBuildsExpectedCommand(t *testing.T) {
	objects := []runtime.Object{
		spireServerConfigMapObject(`{"server":{"trust_domain":"test.nke.local"}}`),
		spireServerPod("wrong-labels", true, map[string]string{"app": "other"}),
		spireServerPod("spire-server-0", false, spireServerSelector()),
		spireServerPod("spire-server-2", true, spireServerSelector()),
		spireServerPod("spire-server-1", true, spireServerSelector()),
	}
	factory := newTestTenantClientFactory(t, objects...)

	wantToken := testJWT(time.Now().Add(time.Hour), "minted")
	var gotMethod string
	var gotURL *url.URL
	factory.newExecutor = func(method string, requestURL *url.URL) (remotecommand.Executor, error) {
		gotMethod = method
		copyURL := *requestURL
		gotURL = &copyURL
		return &fakeExecutor{stdout: wantToken + "\n"}, nil
	}

	token, err := factory.mintJWT(context.Background(), "test.nke.local", "tenant-a")
	if err != nil {
		t.Fatalf("mintJWT() error = %v", err)
	}
	if token != wantToken {
		t.Fatalf("mintJWT() = %q, want %q", token, wantToken)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("executor method = %q, want POST", gotMethod)
	}
	if gotURL == nil {
		t.Fatal("executor URL was not captured")
	}
	if !strings.Contains(gotURL.Path, "/namespaces/spire-server/pods/spire-server-1/exec") {
		t.Fatalf("executor URL path = %q, want lexically first Ready pod", gotURL.Path)
	}

	wantCommand := []string{
		"bin/spire-server", "jwt", "mint",
		"-spiffeID", "spiffe://test.nke.local/nke-service/cloud-workflow-engine",
		"-audience", "tenant-a",
	}
	if got := gotURL.Query()["command"]; !reflect.DeepEqual(got, wantCommand) {
		t.Fatalf("exec command = %#v, want %#v", got, wantCommand)
	}
	if got := gotURL.Query().Get("container"); got != spireServerContainer {
		t.Fatalf("exec container = %q, want %q", got, spireServerContainer)
	}
	if gotURL.Query().Get("stdout") != "true" || gotURL.Query().Get("stderr") != "true" {
		t.Fatalf("exec stream query = %q, want stdout=true and stderr=true", gotURL.RawQuery)
	}
}

func TestTrustDomainFromSPIREServerConfig(t *testing.T) {
	tests := map[string]struct {
		config  string
		want    string
		wantErr string
	}{
		"trust domain": {
			config: `{"server":{"trust_domain":"test.nke.local","admin_ids":["spiffe://ignored.nke.local/nke-service/cloud-workflow-engine"]}}`,
			want:   "test.nke.local",
		},
		"CWE admin ID fallback": {
			config: `{"server":{"admin_ids":["spiffe://fallback.nke.local/nke-service/cloud-workflow-engine"]}}`,
			want:   "fallback.nke.local",
		},
		"missing":   {config: `{"server":{}}`, wantErr: "neither trust_domain nor a CWE admin_id"},
		"malformed": {config: `{`, wantErr: "parsing server.conf JSON"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := trustDomainFromSPIREServerConfig([]byte(test.config))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("trustDomainFromSPIREServerConfig() error = %v, want error containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("trustDomainFromSPIREServerConfig() = (%q, %v), want (%q, nil)", got, err, test.want)
			}
		})
	}
}

func TestReadySPIREServerPodFailsWhenNoPodIsReady(t *testing.T) {
	factory := newTestTenantClientFactory(t,
		spireServerPod("spire-server-0", false, spireServerSelector()),
	)

	if _, err := factory.readySPIREServerPod(context.Background()); err == nil || !strings.Contains(err.Error(), "no Ready SPIRE server pod") {
		t.Fatalf("readySPIREServerPod() error = %v, want no Ready pod error", err)
	}
}

func TestMintJWTIncludesExecutorStderr(t *testing.T) {
	factory := newTestTenantClientFactory(t,
		spireServerConfigMapObject(`{"server":{"admin_ids":["spiffe://test.nke.local/nke-service/cloud-workflow-engine"]}}`),
		spireServerPod("spire-server-0", true, spireServerSelector()),
	)
	factory.newExecutor = func(string, *url.URL) (remotecommand.Executor, error) {
		return &fakeExecutor{stderr: "permission denied", err: errors.New("remote command failed")}, nil
	}

	_, err := factory.mintJWT(context.Background(), "test.nke.local", "tenant-a")
	if err == nil || !strings.Contains(err.Error(), "remote command failed: permission denied") {
		t.Fatalf("mintJWT() error = %v, want executor error and stderr", err)
	}
}

func newTestTenantClientFactory(t *testing.T, objects ...runtime.Object) *TenantClientFactory {
	t.Helper()
	managementConfig := &rest.Config{Host: "https://management.example"}
	factory, err := BuildTenantClientFactory(managementConfig)
	if err != nil {
		t.Fatalf("NewTenantClientFactory() error = %v", err)
	}
	factory.coreClient = k8sfake.NewClientset(objects...).CoreV1()
	return factory
}

func testSPIREKubeconfig(t *testing.T, audience string) []byte {
	t.Helper()
	config := clientcmdapi.NewConfig()
	config.Clusters["tenant"] = &clientcmdapi.Cluster{
		Server:                   "https://tenant.example:6443",
		CertificateAuthorityData: []byte("tenant-ca"),
	}
	config.AuthInfos["published-user"] = &clientcmdapi.AuthInfo{
		Exec: &clientcmdapi.ExecConfig{
			APIVersion:      "client.authentication.k8s.io/v1",
			Command:         "/opt/spiffe/bin/k8s-spiffe-workload-jwt-exec-auth",
			InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
			Env: func() []clientcmdapi.ExecEnvVar {
				if audience == "" {
					return nil
				}
				return []clientcmdapi.ExecEnvVar{{Name: spireAudienceEnv, Value: audience}}
			}(),
		},
	}
	config.Contexts["tenant"] = &clientcmdapi.Context{Cluster: "tenant", AuthInfo: "published-user"}
	config.CurrentContext = "tenant"

	kubeconfig, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatalf("clientcmd.Write() error = %v", err)
	}
	return kubeconfig
}

func testSPIREExchangeKubeconfig(t *testing.T, audience, endpoint string) []byte {
	t.Helper()
	config, err := clientcmd.Load(testSPIREKubeconfig(t, audience))
	if err != nil {
		t.Fatalf("clientcmd.Load() error = %v", err)
	}
	config.AuthInfos["published-user"].Exec.Env = append(
		config.AuthInfos["published-user"].Exec.Env,
		clientcmdapi.ExecEnvVar{Name: spireExchangeEnv, Value: endpoint},
	)
	kubeconfig, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatalf("clientcmd.Write() error = %v", err)
	}
	return kubeconfig
}

func testAdminKubeconfig(t *testing.T) []byte {
	t.Helper()
	config := clientcmdapi.NewConfig()
	config.Clusters["tenant"] = &clientcmdapi.Cluster{
		Server:                   "https://tenant.example:6443",
		CertificateAuthorityData: []byte("tenant-ca"),
	}
	config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: []byte("admin-certificate"),
		ClientKeyData:         []byte("admin-key"),
	}
	config.Contexts["tenant"] = &clientcmdapi.Context{Cluster: "tenant", AuthInfo: "admin"}
	config.CurrentContext = "tenant"

	kubeconfig, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatalf("clientcmd.Write() error = %v", err)
	}
	return kubeconfig
}

func testClientKubeconfig(t *testing.T) []byte {
	t.Helper()
	config := clientcmdapi.NewConfig()
	config.Clusters["tenant"] = &clientcmdapi.Cluster{
		Server:                "https://tenant.example:6443",
		InsecureSkipTLSVerify: true,
	}
	config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "test-token"}
	config.Contexts["tenant"] = &clientcmdapi.Context{Cluster: "tenant", AuthInfo: "admin"}
	config.CurrentContext = "tenant"

	kubeconfig, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatalf("clientcmd.Write() error = %v", err)
	}
	return kubeconfig
}

func spireServerConfigMapObject(serverConfig string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: spireServerConfigMapName(), Namespace: spireServerNamespace},
		Data:       map[string]string{spireServerConfigKey: serverConfig},
	}
}

func spireServerPod(name string, ready bool, labels map[string]string) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: spireServerNamespace, Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: spireServerContainer}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func testJWT(expires time.Time, marker string) string {
	return testJWTClaims(fmt.Sprintf(`{"exp":%d,"marker":%q}`, expires.Unix(), marker))
}

func testJWTClaims(claims string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(claims))
	return header + "." + payload + ".signature"
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type fakeExecutor struct {
	stdout string
	stderr string
	err    error
	once   sync.Once
}

func (e *fakeExecutor) Stream(options remotecommand.StreamOptions) error {
	return e.write(options)
}

func (e *fakeExecutor) StreamWithContext(_ context.Context, options remotecommand.StreamOptions) error {
	return e.write(options)
}

func (e *fakeExecutor) write(options remotecommand.StreamOptions) error {
	e.once.Do(func() {
		if options.Stdout != nil {
			_, _ = io.WriteString(options.Stdout, e.stdout)
		}
		if options.Stderr != nil {
			_, _ = io.WriteString(options.Stderr, e.stderr)
		}
	})
	return e.err
}

// The verifier pods shell out to kubectl, which cannot use the SPIRE exec
// plugin. AdminKubeconfig must hand back a plain token kubeconfig that keeps the
// published endpoint and CA.
func TestAdminKubeconfigReplacesSPIREExecWithAMintedToken(t *testing.T) {
	wantToken := testJWT(time.Now().Add(time.Hour), "minted")
	factory := newTestTenantClientFactory(t,
		spireServerConfigMapObject(`{"server":{"trust_domain":"test.nke.local"}}`),
		spireServerPod("spire-server-1", true, spireServerSelector()),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: tenantKubeconfigNamespace, Name: "tenant-a-kubeconfig"},
			Data:       map[string][]byte{tenantKubeconfigKey: testSPIREKubeconfig(t, "tenant-a")},
		},
	)
	var gotURL *url.URL
	factory.newExecutor = func(_ string, requestURL *url.URL) (remotecommand.Executor, error) {
		copyURL := *requestURL
		gotURL = &copyURL
		return &fakeExecutor{stdout: wantToken + "\n"}, nil
	}

	rendered, err := factory.AdminKubeconfig(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("AdminKubeconfig() error = %v", err)
	}
	parsed, err := clientcmd.Load(rendered)
	if err != nil {
		t.Fatalf("clientcmd.Load() error = %v", err)
	}

	authInfo := parsed.AuthInfos["admin"]
	if authInfo == nil || authInfo.Token != wantToken {
		t.Fatalf("rendered token = %#v, want the minted JWT", authInfo)
	}
	if authInfo.Exec != nil {
		t.Fatal("rendered kubeconfig still carries an exec plugin")
	}
	cluster := parsed.Clusters["tenant"]
	if cluster == nil || cluster.Server != "https://tenant.example:6443" || string(cluster.CertificateAuthorityData) != "tenant-ca" {
		t.Fatalf("rendered cluster = %#v, want the published endpoint and CA", cluster)
	}
	if want := "spiffe://test.nke.local" + adminSPIFFEPath; !slices.Contains(gotURL.Query()["command"], want) {
		t.Fatalf("AdminKubeconfig() did not mint %q", want)
	}
}

// A non-SPIRE kubeconfig already carries a usable credential and must be handed
// back untouched, so an OIDC-off cluster keeps working.
func TestAdminKubeconfigPassesThroughANonSPIREKubeconfig(t *testing.T) {
	published := testClientKubeconfig(t)
	factory := newTestTenantClientFactory(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantKubeconfigNamespace, Name: "tenant-a-kubeconfig"},
		Data:       map[string][]byte{tenantKubeconfigKey: published},
	})

	rendered, err := factory.AdminKubeconfig(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("AdminKubeconfig() error = %v", err)
	}
	if !bytes.Equal(rendered, published) {
		t.Fatal("AdminKubeconfig() rewrote a kubeconfig that needed no change")
	}
}
