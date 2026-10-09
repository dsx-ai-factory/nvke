// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
)

func TestProbeTokenUsesOnlyTheSuppliedBearerToken(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/apis/authentication.k8s.io/v1/selfsubjectreviews":
			review := authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{UserInfo: authenticationv1.UserInfo{
				Username: "alice", Groups: []string{"readers", "system:authenticated"},
			}}}
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(review)
		case "/allowed":
			writer.WriteHeader(http.StatusOK)
		case "/denied":
			writer.WriteHeader(http.StatusForbidden)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	certificate, err := x509.ParseCertificate(server.Certificate().Raw)
	if err != nil {
		t.Fatalf("ParseCertificate() error = %v", err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	result, err := ProbeToken(context.Background(), server.URL, ca, []byte("test-token"), "/allowed", "/denied")
	if err != nil {
		t.Fatalf("ProbeToken() error = %v", err)
	}
	if result.IdentityStatus != http.StatusCreated || result.Username != "alice" || !slices.Equal(result.Groups, []string{"readers", "system:authenticated"}) || result.AllowedStatus != http.StatusOK || result.DeniedStatus != http.StatusForbidden {
		t.Fatalf("ProbeToken() = %#v", result)
	}
}

func TestProbeTokenDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			var targetRequests, leakedTokens, sourceRequests atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetRequests.Add(1)
				if r.Header.Get("Authorization") == "Bearer example-token" {
					leakedTokens.Add(1)
				}
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(target.Close)
			source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer example-token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				sourceRequests.Add(1)
				http.Redirect(w, r, target.URL+r.URL.Path, code)
			}))
			t.Cleanup(source.Close)
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: source.Certificate().Raw})

			result, err := ProbeToken(t.Context(), source.URL, ca, []byte("example-token"), "/allowed", "/denied")
			require.NoError(t, err)
			require.Equal(t, int32(3), sourceRequests.Load(), "all probes must send the bearer token to the source")
			require.Equal(t, int32(0), targetRequests.Load(), "redirect target received %d bearer credentials", leakedTokens.Load())
			require.Equal(t, code, result.IdentityStatus)
			require.Equal(t, code, result.AllowedStatus)
			require.Equal(t, code, result.DeniedStatus)
			require.Empty(t, result.Username)
			require.Empty(t, result.Groups)
		})
	}
}

func TestProbeTokenRejectsInvalidEndpointsWithoutCredentials(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		endpoint  string
		wantError string
	}{
		{name: "malformed URL", endpoint: "https://alice:example-password@tenant.test/%zz", wantError: "parse tenant endpoint URL: invalid URL"},
		{name: "HTTP URL", endpoint: "http://tenant.test", wantError: "tenant endpoint must be an absolute HTTPS URL"},
		{name: "URL with credentials", endpoint: "https://alice:example-password@tenant.test", wantError: "tenant endpoint must be an absolute HTTPS URL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ProbeToken(t.Context(), test.endpoint, []byte("ca"), []byte("example-token"), "/allowed", "/denied")
			require.EqualError(t, err, test.wantError)
			require.NotContains(t, err.Error(), "example-password")
		})
	}
}

func TestTrustFromKubeconfigCopiesOnlyEndpointAndCA(t *testing.T) {
	t.Parallel()
	data := []byte(`apiVersion: v1
kind: Config
current-context: tenant
clusters:
- name: tenant
  cluster:
    server: https://tenant.test:6443
    certificate-authority-data: Y2E=
contexts:
- name: tenant
  context:
    cluster: tenant
    user: admin
users:
- name: admin
  user:
    token: admin-token
    impersonate: root
`)
	trust, err := TrustFromKubeconfig(data)
	if err != nil {
		t.Fatalf("TrustFromKubeconfig() error = %v", err)
	}
	if trust.Endpoint != "https://tenant.test:6443" || string(trust.CAData) != "ca" {
		t.Fatalf("TrustFromKubeconfig() = %#v", trust)
	}
}

func TestTrustFromKubeconfigRejectsMalformedCredentialsWithoutLeakingValues(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"token", "password"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			credential := "example-" + field
			data := []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\nusers:\n- name: admin\n  user:\n    %s: !!int %s\n", field, credential))
			trust, err := TrustFromKubeconfig(data)
			require.ErrorContains(t, err, "decode tenant kubeconfig")
			require.NotContains(t, err.Error(), credential)
			require.NoError(t, errors.Unwrap(err), "parser errors can contain admin credentials")
			require.Empty(t, trust)
		})
	}
}
