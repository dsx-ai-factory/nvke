// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// The controller namespaces authConfigNamespaces searches, by name, so a
// reordering or a typo there fails here rather than on a site.
const (
	greenfieldNamespace = "nvke-controller"
	previousNamespace   = "nke-controller"
	legacyNamespace     = "cloud-workflow-engine"
)

func TestTokenMintsForConfiguredAudience(t *testing.T) {
	const configuredAudience = "https://nke.example.internal"

	client := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      authConfigMaps[0],
			Namespace: legacyNamespace,
		},
		Data: map[string]string{
			authConfigKey: "audiences:\n  - " + configuredAudience + "\n",
		},
	})
	client.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateActionImpl)
		if !ok || create.Subresource != "token" {
			return false, nil, nil
		}
		if create.Namespace != tokenNamespace || create.Name != tokenServiceAccount {
			return true, nil, fmt.Errorf("minted for %s/%s", create.Namespace, create.Name)
		}
		request, ok := create.Object.(*authnv1.TokenRequest)
		if !ok {
			return true, nil, fmt.Errorf("request type is %T", create.Object)
		}
		if len(request.Spec.Audiences) != 1 || request.Spec.Audiences[0] != configuredAudience {
			return true, nil, fmt.Errorf("audiences are %v", request.Spec.Audiences)
		}
		if request.Spec.ExpirationSeconds == nil || *request.Spec.ExpirationSeconds != int64(tokenLifetime.Seconds()) {
			return true, nil, fmt.Errorf("expirationSeconds is %v", request.Spec.ExpirationSeconds)
		}
		return true, &authnv1.TokenRequest{
			Status: authnv1.TokenRequestStatus{Token: "minted-token"},
		}, nil
	})

	token, err := (&TokenSource{client: client}).Mint(context.Background())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token != "minted-token" {
		t.Fatalf("token = %q, want minted-token", token)
	}
}

func TestTokenUsesCachedToken(t *testing.T) {
	const endpoint = "https://nke.example.internal"
	cache := &tokenCache{tokens: map[string]cachedToken{
		endpoint: {
			Value:     "cached-token",
			ExpiresAt: time.Now().Add(tokenRenewBefore + time.Minute),
		},
	}}
	client := fake.NewClientset()
	source := &TokenSource{
		client:   client,
		endpoint: endpoint,
		cache:    cache,
	}

	token, err := source.Mint(t.Context())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token != "cached-token" {
		t.Fatalf("token = %q, want cached-token", token)
	}
	if len(client.Actions()) != 0 {
		t.Fatalf("cached token made Kubernetes API calls: %v", client.Actions())
	}
}

func TestTokenRenewsAndCachesNearExpiryToken(t *testing.T) {
	const endpoint = "https://nke.example.internal"
	mintedExpiry := time.Now().Add(tokenLifetime)
	client := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: authConfigMaps[0], Namespace: legacyNamespace},
		Data:       map[string]string{authConfigKey: "audiences:\n  - " + endpoint + "\n"},
	})
	client.PrependReactor("create", "serviceaccounts", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authnv1.TokenRequest{
			Status: authnv1.TokenRequestStatus{
				Token:               "minted-token",
				ExpirationTimestamp: metav1.NewTime(mintedExpiry),
			},
		}, nil
	})
	cache := &tokenCache{tokens: map[string]cachedToken{
		endpoint: {
			Value:     "old-token",
			ExpiresAt: time.Now().Add(tokenRenewBefore),
		},
	}}
	source := &TokenSource{client: client, endpoint: endpoint, cache: cache}

	token, err := source.Mint(t.Context())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token != "minted-token" {
		t.Fatalf("token = %q, want minted-token", token)
	}
	stored := cache.tokens[endpoint]
	if stored.Value != "minted-token" || !stored.ExpiresAt.Equal(mintedExpiry) {
		t.Fatalf("cached token = %#v, want minted token expiring at %v", stored, mintedExpiry)
	}
}

func TestTokenReusesMintedTokenInMemory(t *testing.T) {
	const configuredAudience = "https://nke.example.internal"
	client := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: authConfigMaps[0], Namespace: legacyNamespace},
		Data:       map[string]string{authConfigKey: "audiences:\n  - " + configuredAudience + "\n"},
	})
	mints := 0
	client.PrependReactor("create", "serviceaccounts", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		mints++
		return true, &authnv1.TokenRequest{
			Status: authnv1.TokenRequestStatus{
				Token:               "minted-token",
				ExpirationTimestamp: metav1.NewTime(time.Now().Add(tokenLifetime)),
			},
		}, nil
	})
	source := &TokenSource{
		client: client,
		cache:  &tokenCache{tokens: make(map[string]cachedToken)},
	}

	for range 2 {
		token, err := source.Mint(t.Context())
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if token != "minted-token" {
			t.Fatalf("token = %q, want minted-token", token)
		}
	}
	if mints != 1 {
		t.Fatalf("mint calls = %d, want 1", mints)
	}
}

func TestTokenReturnsNoCredentialWhenAuthenticationIsDisabled(t *testing.T) {
	client := fake.NewClientset()

	token, err := (&TokenSource{client: client}).Mint(t.Context())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token != "" {
		t.Fatalf("token = %q, want empty", token)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "serviceaccounts" {
			t.Fatal("Token tried to mint a credential without an auth ConfigMap")
		}
	}
}

func TestTokenRequiresConfiguredAudience(t *testing.T) {
	tests := []struct {
		name      string
		configMap *corev1.ConfigMap
	}{
		{
			name: "missing auth config key",
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: authConfigMaps[0], Namespace: legacyNamespace},
			},
		},
		{
			name: "empty audience list",
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: authConfigMaps[0], Namespace: legacyNamespace},
				Data:       map[string]string{authConfigKey: "audiences: []\n"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset()
			if tt.configMap != nil {
				if _, err := client.CoreV1().ConfigMaps(legacyNamespace).
					Create(t.Context(), tt.configMap, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := (&TokenSource{client: client}).Mint(t.Context()); err == nil {
				t.Fatal("Mint returned no error")
			}
		})
	}
}

func TestAudienceSearchesBothControllerNamespaces(t *testing.T) {
	authConfig := func(namespace, audience string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: authConfigMaps[0], Namespace: namespace},
			Data:       map[string]string{authConfigKey: "audiences:\n  - " + audience + "\n"},
		}
	}

	tests := []struct {
		name         string
		objects      []runtime.Object
		wantAudience string
		wantEnabled  bool
	}{
		{
			name:         "greenfield site",
			objects:      []runtime.Object{authConfig(greenfieldNamespace, "https://greenfield.example")},
			wantAudience: "https://greenfield.example",
			wantEnabled:  true,
		},
		{
			name:         "site installed before the nvke rename",
			objects:      []runtime.Object{authConfig(previousNamespace, "https://previous.example")},
			wantAudience: "https://previous.example",
			wantEnabled:  true,
		},
		{
			name:         "legacy site",
			objects:      []runtime.Object{authConfig(legacyNamespace, "https://legacy.example")},
			wantAudience: "https://legacy.example",
			wantEnabled:  true,
		},
		{
			name: "every name present uses the greenfield name",
			objects: []runtime.Object{
				authConfig(legacyNamespace, "https://legacy.example"),
				authConfig(previousNamespace, "https://previous.example"),
				authConfig(greenfieldNamespace, "https://greenfield.example"),
			},
			wantAudience: "https://greenfield.example",
			wantEnabled:  true,
		},
		{
			name:        "neither name leaves authentication off",
			wantEnabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.objects...)
			audience, enabled, err := (&TokenSource{client: client}).audience(t.Context())
			if err != nil {
				t.Fatalf("audience: %v", err)
			}
			if audience != tt.wantAudience || enabled != tt.wantEnabled {
				t.Fatalf("audience = %q, enabled = %t; want %q, %t", audience, enabled, tt.wantAudience, tt.wantEnabled)
			}
		})
	}
}

// The chart's default ConfigMap name moved to nvke-auth-config; a site that
// pinned apiAuth.configMap.name keeps nke-auth-config. The CLI reads either,
// so one nvkectl works against both.
func TestAudienceSearchesBothConfigMapNames(t *testing.T) {
	authConfig := func(name, audience string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: greenfieldNamespace},
			Data:       map[string]string{authConfigKey: "audiences:\n  - " + audience + "\n"},
		}
	}

	tests := []struct {
		name         string
		objects      []runtime.Object
		wantAudience string
		wantEnabled  bool
	}{
		{
			name:         "greenfield name",
			objects:      []runtime.Object{authConfig("nvke-auth-config", "https://greenfield.example")},
			wantAudience: "https://greenfield.example",
			wantEnabled:  true,
		},
		{
			name:         "pinned legacy name",
			objects:      []runtime.Object{authConfig("nke-auth-config", "https://legacy.example")},
			wantAudience: "https://legacy.example",
			wantEnabled:  true,
		},
		{
			name: "both names present uses the greenfield name",
			objects: []runtime.Object{
				authConfig("nke-auth-config", "https://legacy.example"),
				authConfig("nvke-auth-config", "https://greenfield.example"),
			},
			wantAudience: "https://greenfield.example",
			wantEnabled:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.objects...)
			audience, enabled, err := (&TokenSource{client: client}).audience(t.Context())
			if err != nil {
				t.Fatalf("audience: %v", err)
			}
			if audience != tt.wantAudience || enabled != tt.wantEnabled {
				t.Fatalf("audience = %q, enabled = %t; want %q, %t", audience, enabled, tt.wantAudience, tt.wantEnabled)
			}
		})
	}
}

// Only NotFound moves on to the next namespace. Any other failure is reported,
// so a denied read cannot turn into an unauthenticated request.
func TestAudienceReportsNonNotFoundErrors(t *testing.T) {
	client := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: authConfigMaps[0], Namespace: legacyNamespace},
		Data:       map[string]string{authConfigKey: "audiences:\n  - https://legacy.example\n"},
	})
	client.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == greenfieldNamespace {
			return true, nil, fmt.Errorf("forbidden")
		}
		return false, nil, nil
	})

	if _, _, err := (&TokenSource{client: client}).audience(t.Context()); err == nil {
		t.Fatal("audience returned no error for a failed read of the greenfield namespace")
	}
}

// forbid returns a reactor that denies get on exactly the named ConfigMaps, the
// way RBAC scoped to resourceNames does, and passes everything else through.
func forbid(names ...string) k8stesting.ReactionFunc {
	denied := make(map[string]bool, len(names))
	for _, n := range names {
		denied[n] = true
	}
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetActionImpl)
		if !ok || !denied[get.Name] {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "configmaps"}, get.Name, fmt.Errorf("not permitted"),
		)
	}
}

// A site pinned to the legacy name may grant get on that name alone. The denial
// of the greenfield name is then expected, and must not stop the search before
// the name the kubeconfig can actually read.
func TestAudienceFallsBackWhenTheGreenfieldNameIsForbidden(t *testing.T) {
	client := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nke-auth-config", Namespace: greenfieldNamespace},
		Data:       map[string]string{authConfigKey: "audiences:\n  - https://pinned.example\n"},
	})
	client.PrependReactor("get", "configmaps", forbid("nvke-auth-config"))

	audience, enabled, err := (&TokenSource{client: client}).audience(t.Context())
	if err != nil {
		t.Fatalf("audience: %v", err)
	}
	if audience != "https://pinned.example" || !enabled {
		t.Fatalf("audience = %q, enabled = %t; want https://pinned.example, true", audience, enabled)
	}
}

// The other half of that: when nothing is readable, the denial is the answer.
// Reporting "authentication is off" would send a bare request that the
// controller rejects, turning an RBAC problem into a confusing 401.
func TestAudienceReportsForbiddenWhenNothingIsReadable(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("get", "configmaps", forbid("nvke-auth-config", "nke-auth-config"))

	_, enabled, err := (&TokenSource{client: client}).audience(t.Context())
	if err == nil {
		t.Fatal("audience treated a denied read as authentication being off")
	}
	if enabled {
		t.Fatal("audience reported authentication as enabled without a ConfigMap")
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("audience lost the Forbidden cause: %v", err)
	}
}

func TestUsesOIDCLogin(t *testing.T) {
	tests := []struct {
		name string
		exec *clientcmdapi.ExecConfig
		want bool
	}{
		{name: "nil"},
		{name: "teleport", exec: &clientcmdapi.ExecConfig{Command: "tsh", Args: []string{"kube", "credentials"}}},
		{name: "eks", exec: &clientcmdapi.ExecConfig{Command: "aws", Args: []string{"eks", "get-token"}}},
		{name: "gke", exec: &clientcmdapi.ExecConfig{Command: "gke-gcloud-auth-plugin"}},
		{
			name: "kubectl oidc-login",
			exec: &clientcmdapi.ExecConfig{Command: "kubectl", Args: []string{"oidc-login", "get-token"}},
			want: true,
		},
		{
			name: "kubectl-oidc_login",
			exec: &clientcmdapi.ExecConfig{Command: "/usr/local/bin/kubectl-oidc_login", Args: []string{"get-token"}},
			want: true,
		},
		{name: "name substring", exec: &clientcmdapi.ExecConfig{Command: "not-oidc-login"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usesOIDCLogin(tt.exec); got != tt.want {
				t.Fatalf("usesOIDCLogin() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestUseExecToken(t *testing.T) {
	oidc := &clientcmdapi.ExecConfig{Command: "kubectl", Args: []string{"oidc-login", "get-token"}}
	tests := []struct {
		name string
		cfg  *rest.Config
		want bool
	}{
		{name: "oidc only", cfg: &rest.Config{ExecProvider: oidc}, want: true},
		{name: "oidc plus token", cfg: &rest.Config{ExecProvider: oidc, BearerToken: "static"}},
		{name: "oidc plus basic", cfg: &rest.Config{ExecProvider: oidc, Username: "u", Password: "p"}},
		{name: "oidc plus cert", cfg: &rest.Config{
			ExecProvider:    oidc,
			TLSClientConfig: rest.TLSClientConfig{CertData: []byte("c"), KeyData: []byte("k")},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := useExecToken(tt.cfg); got != tt.want {
				t.Fatalf("useExecToken() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestMintUsesOIDCLoginToken(t *testing.T) {
	const want = "exec-plugin-token"
	source := newExecSource(t, execSourceOpts{
		args:       []string{"oidc-login", "get-token"},
		credential: `{"kind":"ExecCredential","apiVersion":"client.authentication.k8s.io/v1","status":{"token":"` + want + `"}}`,
	})
	if source.restConfig == nil {
		t.Fatal("expected oidc-login token source")
	}
	source.client = fake.NewClientset()

	token, err := source.Mint(t.Context())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token != want {
		t.Fatalf("token = %q, want %q", token, want)
	}
	if actions := source.client.(*fake.Clientset).Actions(); len(actions) != 0 {
		t.Fatalf("oidc-login mint made Kubernetes API calls: %v", actions)
	}
}

func TestMintUsesTokenRequestForExecBlockers(t *testing.T) {
	const staticToken = "static-k8s-token"
	const execToken = "exec-plugin-token"
	tests := []struct {
		name string
		opts execSourceOpts
	}{
		{
			name: "teleport client cert",
			opts: execSourceOpts{
				credential: `{"kind":"ExecCredential","apiVersion":"client.authentication.k8s.io/v1","status":{"clientCertificateData":"dummy-cert","clientKeyData":"dummy-key"}}`,
			},
		},
		{
			name: "static token plus oidc-login",
			opts: execSourceOpts{
				args:       []string{"oidc-login", "get-token"},
				credential: `{"kind":"ExecCredential","apiVersion":"client.authentication.k8s.io/v1","status":{"token":"` + execToken + `"}}`,
				token:      staticToken,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := newExecSource(t, tt.opts)
			if source.restConfig != nil {
				t.Fatal("expected TokenRequest path")
			}
			source.client = fake.NewClientset()

			token, err := source.Mint(t.Context())
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			if token != "" {
				t.Fatalf("token = %q, want empty (auth ConfigMap absent)", token)
			}
		})
	}
}

type execSourceOpts struct {
	args       []string
	credential string
	token      string
}

func newExecSource(t *testing.T, opts execSourceOpts) *TokenSource {
	t.Helper()
	dir := t.TempDir()
	command := filepath.Join(dir, "exec-helper.sh")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s' '"+opts.credential+"'\n"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["site"] = &clientcmdapi.Cluster{
		Server:                "https://127.0.0.1:6443",
		InsecureSkipTLSVerify: true,
	}
	cfg.AuthInfos["user"] = &clientcmdapi.AuthInfo{
		Token: opts.token,
		Exec: &clientcmdapi.ExecConfig{
			APIVersion:      "client.authentication.k8s.io/v1",
			Command:         command,
			Args:            opts.args,
			InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
		},
	}
	cfg.Contexts["user"] = &clientcmdapi.Context{Cluster: "site", AuthInfo: "user"}
	cfg.CurrentContext = "user"
	kcPath := filepath.Join(dir, "kubeconfig")
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := os.WriteFile(kcPath, data, 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	source, err := NewTokenSource(kcPath, "https://controller.example")
	if err != nil {
		t.Fatalf("NewTokenSource: %v", err)
	}
	return source
}
