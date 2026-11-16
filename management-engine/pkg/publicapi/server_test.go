// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package publicapi

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

func TestServeHandlerAuthentication(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tenant := testAuthenticator(t, "nke.nvidia.com", key.Public())
	operator := testAuthenticator(t, "operator.nke.nvidia.com", key.Public())
	tenantToken := mintToken(t, key, "nke.nvidia.com")
	operatorToken := mintToken(t, key, "operator.nke.nvidia.com")

	tests := []struct {
		name          string
		authenticator *authn.Authenticator
		path          string
		token         string
		want          int
		wantPrincipal bool
	}{
		{name: "disabled serves the route", path: "/v1/clusters", want: http.StatusNoContent},
		{name: "health check skips authentication", authenticator: tenant, path: "/healthz", want: http.StatusOK},
		{name: "missing token", authenticator: tenant, path: "/v1/clusters", want: http.StatusUnauthorized},
		{name: "matching audience", authenticator: tenant, path: "/v1/clusters", token: tenantToken, want: http.StatusNoContent, wantPrincipal: true},
		{name: "other audience", authenticator: tenant, path: "/v1/clusters", token: operatorToken, want: http.StatusUnauthorized},
		{name: "operator config accepts its own audience", authenticator: operator, path: "/v1/clusters", token: operatorToken, want: http.StatusNoContent, wantPrincipal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sawPrincipal bool
			mux := http.NewServeMux()
			mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			mux.HandleFunc("GET /v1/clusters", func(w http.ResponseWriter, r *http.Request) {
				_, sawPrincipal = authn.PrincipalFromContext(r.Context())
				w.WriteHeader(http.StatusNoContent)
			})
			server := httptest.NewServer(serveHandler(mux, tt.authenticator))
			t.Cleanup(server.Close)

			request, err := http.NewRequest(http.MethodGet, server.URL+tt.path, nil)
			require.NoError(t, err)
			if tt.token != "" {
				request.Header.Set("Authorization", "Bearer "+tt.token)
			}
			response, err := server.Client().Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, tt.want, response.StatusCode)
			require.Equal(t, tt.wantPrincipal, sawPrincipal)
		})
	}
}

type staticKey struct{ key crypto.PublicKey }

func (s staticKey) KeyFor(context.Context, string) (crypto.PublicKey, error) { return s.key, nil }

func testAuthenticator(t *testing.T, audience string, key crypto.PublicKey) *authn.Authenticator {
	t.Helper()
	verifier, err := authn.NewVerifier(&authn.Config{
		Audiences: []string{audience},
		Issuers: []authn.Issuer{{
			Name:       "issuer",
			Issuer:     "https://issuer.example.com",
			JWKS:       &authn.JWKSKeys{URI: "https://issuer.example.com/keys"},
			Algorithms: []string{"ES256"},
			Admit:      &authn.Admit{Groups: []string{"viewers"}},
		}},
	}, map[string]authn.KeySource{"issuer": staticKey{key}})
	require.NoError(t, err)
	authenticator, err := authn.NewAuthenticator(verifier, authn.ModeEnforcing, slog.Default(), nil)
	require.NoError(t, err)
	return authenticator
}

func mintToken(t *testing.T, key *ecdsa.PrivateKey, audience string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss":    "https://issuer.example.com",
		"aud":    []string{audience},
		"sub":    "alice",
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": []string{"viewers"},
	})
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
}
