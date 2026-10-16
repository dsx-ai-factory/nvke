// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

func TestIMDSServer_HandleIdentity(t *testing.T) {
	t.Parallel()

	type parameters struct {
		metadataHeader string
		audience       string
		wantStatus     int
		verify         func(*testing.T, *imdsServer, *httptest.ResponseRecorder)
	}
	tests := map[string]parameters{
		"issues a signed host identity": {
			metadataHeader: "true",
			audience:       "spiffe://controller-capi.nvke.local/spire/server",
			wantStatus:     http.StatusOK,
			verify: func(t *testing.T, server *imdsServer, response *httptest.ResponseRecorder) {
				t.Helper()
				parsed, err := jwt.ParseSigned(response.Body.String(), []jose.SignatureAlgorithm{jose.ES256})
				require.NoError(t, err)
				var claims map[string]any
				require.NoError(t, parsed.Claims(server.publicKey.Key, &claims))
				require.Equal(t, "https://ds9.dsx.nvid.id", claims["iss"])
				require.Equal(t, "spiffe://operator-1.ds9.dsx.nvid.id/bm/host-6d99cbd08fc6c99c", claims["sub"])
				require.Equal(t, "spiffe://controller-capi.nvke.local/spire/server", claims["aud"])
				require.Equal(t, "text/plain", response.Header().Get("Content-Type"))
			},
		},
		"rejects a missing metadata header": {
			wantStatus: http.StatusForbidden,
		},
		"rejects a missing audience": {
			metadataHeader: "true",
			wantStatus:     http.StatusBadRequest,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := newTestIMDSServer(t)
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/latest/meta-data/identity?aud="+url.QueryEscape(params.audience), http.NoBody)
			request.RemoteAddr = "192.0.2.10:12345"
			request.Header.Set("Metadata", params.metadataHeader)
			response := httptest.NewRecorder()

			server.handler().ServeHTTP(response, request)

			require.Equal(t, params.wantStatus, response.Code)
			if params.verify != nil {
				params.verify(t, server, response)
			}
		})
	}
}

func TestIMDSServer_HandleJWKS(t *testing.T) {
	t.Parallel()
	server := newTestIMDSServer(t)
	response := httptest.NewRecorder()

	server.handler().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/jwks.json", http.NoBody))

	require.Equal(t, http.StatusOK, response.Code)
	var keys jose.JSONWebKeySet
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &keys))
	require.Len(t, keys.Keys, 1)
	require.Equal(t, server.publicKey.KeyID, keys.Keys[0].KeyID)
	require.True(t, keys.Keys[0].IsPublic())
}

func newTestIMDSServer(t *testing.T) *imdsServer {
	t.Helper()
	server, err := newIMDSServer(serverOptions{
		issuer: "https://ds9.dsx.nvid.id", tenant: "operator-1", region: "ds9", partition: "nvid.id",
	})
	require.NoError(t, err)
	server.now = func() time.Time { return time.Unix(1_800_000_000, 0).UTC() }
	return server
}
