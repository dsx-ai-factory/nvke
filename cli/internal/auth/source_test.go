// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The controller namespaces authConfigNamespaces searches, by name, so a
// reordering or a typo there fails here rather than on a site.
const (
	greenfieldNamespace = "nke-controller"
	legacyNamespace     = "cloud-workflow-engine"
)

func TestTokenMintsForConfiguredAudience(t *testing.T) {
	const configuredAudience = "https://nke.example.internal"

	client := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      authConfigMap,
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

	token, err := (&Source{client: client}).Mint(context.Background())
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
	source := &Source{
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
		ObjectMeta: metav1.ObjectMeta{Name: authConfigMap, Namespace: legacyNamespace},
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
	source := &Source{client: client, endpoint: endpoint, cache: cache}

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
		ObjectMeta: metav1.ObjectMeta{Name: authConfigMap, Namespace: legacyNamespace},
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
	source := &Source{
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

	token, err := (&Source{client: client}).Mint(t.Context())
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
				ObjectMeta: metav1.ObjectMeta{Name: authConfigMap, Namespace: legacyNamespace},
			},
		},
		{
			name: "empty audience list",
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: authConfigMap, Namespace: legacyNamespace},
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

			if _, err := (&Source{client: client}).Mint(t.Context()); err == nil {
				t.Fatal("Mint returned no error")
			}
		})
	}
}

func TestAudienceSearchesBothControllerNamespaces(t *testing.T) {
	authConfig := func(namespace, audience string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: authConfigMap, Namespace: namespace},
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
			name:         "legacy site",
			objects:      []runtime.Object{authConfig(legacyNamespace, "https://legacy.example")},
			wantAudience: "https://legacy.example",
			wantEnabled:  true,
		},
		{
			name: "both names present uses the greenfield name",
			objects: []runtime.Object{
				authConfig(legacyNamespace, "https://legacy.example"),
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
			audience, enabled, err := (&Source{client: client}).audience(t.Context())
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
		ObjectMeta: metav1.ObjectMeta{Name: authConfigMap, Namespace: legacyNamespace},
		Data:       map[string]string{authConfigKey: "audiences:\n  - https://legacy.example\n"},
	})
	client.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == greenfieldNamespace {
			return true, nil, fmt.Errorf("forbidden")
		}
		return false, nil, nil
	})

	if _, _, err := (&Source{client: client}).audience(t.Context()); err == nil {
		t.Fatal("audience returned no error for a failed read of the greenfield namespace")
	}
}
