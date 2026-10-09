// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package probe

import (
	"context"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	testoidc "github.com/dsx-ai-factory/nvke/controller/api/testutil/oidc"
)

func TestNewTokenOnlyConfigHasNoCredentialFallback(t *testing.T) {
	issuer, err := testoidc.Start(testoidc.Config{Audience: "kubernetes"})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = issuer.Close() })
	config, err := NewTokenOnlyConfig(Request{Endpoint: issuer.IssuerURL(), CAPEM: issuer.CAPEM(), Token: "test-token"})
	if err != nil {
		t.Fatalf("NewTokenOnlyConfig() error = %v", err)
	}
	if config.BearerToken != "test-token" || config.BearerTokenFile != "" || config.Username != "" || config.Password != "" {
		t.Fatalf("unexpected credentials: %+v", config)
	}
	if config.AuthProvider != nil || config.ExecProvider != nil || config.Impersonate.UserName != "" {
		t.Fatalf("config has credential fallback: %+v", config)
	}
	if config.TLSClientConfig.CertData != nil || config.TLSClientConfig.KeyData != nil || config.TLSClientConfig.Insecure {
		t.Fatalf("config has client certificate or insecure TLS: %+v", config.TLSClientConfig)
	}
	proxy, err := config.Proxy(&http.Request{})
	if err != nil || proxy != nil {
		t.Fatalf("Proxy() = %v, %v", proxy, err)
	}
}

func TestAdminFixtureUsesNarrowRBAC(t *testing.T) {
	client := fake.NewSimpleClientset()
	fixture, err := PrepareAdminFixture(context.Background(), client, "oidc-reference", "alice")
	if err != nil {
		t.Fatalf("PrepareAdminFixture() error = %v", err)
	}
	role, err := client.RbacV1().Roles(fixture.Allowed.Namespace).Get(context.Background(), fixtureName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Role: %v", err)
	}
	if len(role.Rules) != 1 || len(role.Rules[0].Resources) != 1 || role.Rules[0].Resources[0] != "configmaps" {
		t.Fatalf("Role rules = %+v", role.Rules)
	}
	binding, err := client.RbacV1().RoleBindings(fixture.Allowed.Namespace).Get(context.Background(), fixtureName, metav1.GetOptions{})
	if err != nil || len(binding.Subjects) != 1 || binding.Subjects[0].Name != "alice" {
		t.Fatalf("RoleBinding = %+v, error = %v", binding, err)
	}
}
