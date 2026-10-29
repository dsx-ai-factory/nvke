// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/require"
)

type staticValidator struct {
	token validatedToken
	err   error
}

func (v staticValidator) Validate(string) (validatedToken, error) {
	return v.token, v.err
}

func TestUnit_ExchangeSubject(t *testing.T) {
	t.Parallel()
	type parameters struct {
		region string
		input  string
		want   string
	}
	tests := map[string]parameters{
		"bare metal controller": {
			region: "capd",
			input:  "spiffe://capd.nke.local/tenant/nke/service/nvke-controller/bm/cwe",
			want:   "spiffe://nke.capd.nvke-controller.nvid.id/bm/cwe",
		},
		"DPU suffix is preserved": {
			region: "capd",
			input:  "spiffe://capd.nke.local/tenant/operator/service/dpu-agent/dpu/host-1/process/agent",
			want:   "spiffe://operator.capd.dpu-agent.nvid.id/dpu/host-1/process/agent",
		},
		"az51 node agent maps to the exact node UUID": {
			region: "az51-dev4",
			input:  "spiffe://az51-dev4.nke.local/tenant/tenant-a/service/dsx/bm/45f59791-87df-4e85-9430-e565166ad020",
			want:   "spiffe://tenant-a.az51-dev4.dsx.nvid.id/bm/45f59791-87df-4e85-9430-e565166ad020",
		},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := exchangeSubject(params.region, "nvid.id", spiffeid.RequireFromString(params.input))
			require.NoError(t, err)
			require.Equal(t, params.want, got.String())
		})
	}
}

func TestUnit_ExchangeSubjectRejectsUnsupportedSource(t *testing.T) {
	t.Parallel()
	type parameters struct {
		path    string
		wantErr string
	}
	tests := map[string]parameters{
		"unrelated workload": {path: "/nke-service/nke-controller", wantErr: "invalid local SPIFFE identity path"},
		"legacy node agent":  {path: "/host/45f59791-87df-4e85-9430-e565166ad020", wantErr: "invalid local SPIFFE identity path"},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := exchangeSubject("capd", "nvid.id", spiffeid.RequireFromString("spiffe://capd.nke.local"+params.path))
			require.ErrorContains(t, err, params.wantErr)
		})
	}
}

func TestTokenExchangeRewritesAndResignsValidatedSVID(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	expiry := now.Add(10 * time.Minute)
	server := newTestServer(t, staticValidator{token: validatedToken{
		ID:       spiffeid.RequireFromString("spiffe://capd.nke.local/tenant/nke/service/nvke-controller/bm/cwe"),
		Audience: []string{"tenant-a"},
		Expiry:   expiry,
		Claims: map[string]any{
			"iss":                  "https://local-spire.invalid",
			"sub":                  "spiffe://capd.nke.local/tenant/nke/service/nvke-controller/bm/cwe",
			"aud":                  "tenant-a",
			"exp":                  expiry.Unix(),
			"iat":                  now.Add(-time.Minute).Unix(),
			"jti":                  "source-token",
			"private_source_claim": "must-not-cross-boundary",
		},
	}})
	server.now = func() time.Time { return now }

	form := url.Values{
		"grant_type":         {grantTypeTokenExchange},
		"subject_token":      {"validated-by-test-double"},
		"subject_token_type": {tokenTypeJWT},
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/exchange", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", contentTypeFormEncoded)
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	var exchange tokenExchangeResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &exchange))
	require.Equal(t, tokenTypeJWT, exchange.IssuedTokenType)
	require.Equal(t, "Bearer", exchange.TokenType)
	require.EqualValues(t, 600, exchange.ExpiresIn)

	parsed, err := jwt.ParseSigned(exchange.AccessToken, []jose.SignatureAlgorithm{jose.ES256})
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, parsed.Claims(server.publicKey.Key, &claims))
	require.Equal(t, "https://capd.dsx.nvid.id", claims["iss"])
	require.Equal(t, "spiffe://nke.capd.nvke-controller.nvid.id/bm/cwe", claims["sub"])
	require.Equal(t, "tenant-a", claims["aud"])
	require.Equal(t, "source-token", claims["jti"])
	require.NotContains(t, claims, "private_source_claim")
	require.Equal(t, server.publicKey.KeyID, parsed.Headers[0].KeyID)
}

func TestUnit_TokenExchangeSignsNodeAgentIdentity(t *testing.T) {
	t.Parallel()
	const nodeID = "45f59791-87df-4e85-9430-e565166ad020"
	now := time.Unix(1_800_000_000, 0).UTC()
	server := newTestServer(t, staticValidator{token: validatedToken{
		ID:     spiffeid.RequireFromString("spiffe://capd.nke.local/tenant/nke/service/node-agent/bm/" + nodeID),
		Expiry: now.Add(10 * time.Minute),
		Claims: map[string]any{
			"aud": "nke-controller",
			"exp": now.Add(10 * time.Minute).Unix(),
		},
	}})
	server.now = func() time.Time { return now }

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/exchange", strings.NewReader(validForm("validated-by-test-double").Encode()))
	request.Header.Set("Content-Type", contentTypeFormEncoded)
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	var exchange tokenExchangeResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &exchange))
	parsed, err := jwt.ParseSigned(exchange.AccessToken, []jose.SignatureAlgorithm{jose.ES256})
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, parsed.Claims(server.publicKey.Key, &claims))
	require.Equal(t, "https://capd.dsx.nvid.id", claims["iss"])
	require.Equal(t, "nke-controller", claims["aud"])
	require.Equal(t, "spiffe://nke.capd.node-agent.nvid.id/bm/"+nodeID, claims["sub"])
}

func TestTokenExchangeRejectsInvalidRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		contentType string
		form        url.Values
		validator   staticValidator
		wantStatus  int
		wantCode    string
	}{
		{name: "wrong content type", contentType: "application/json", wantStatus: http.StatusUnsupportedMediaType, wantCode: "invalid_request"},
		{name: "wrong grant", contentType: contentTypeFormEncoded, form: url.Values{"grant_type": {"credentials"}}, wantStatus: http.StatusBadRequest, wantCode: "unsupported_grant_type"},
		{name: "missing token", contentType: contentTypeFormEncoded, form: validForm(""), wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "invalid source SVID", contentType: contentTypeFormEncoded, form: validForm("bad"), validator: staticValidator{err: errors.New("bad signature")}, wantStatus: http.StatusUnauthorized, wantCode: "invalid_grant"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newTestServer(t, test.validator)
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/exchange", strings.NewReader(test.form.Encode()))
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			server.handler().ServeHTTP(response, request)

			require.Equal(t, test.wantStatus, response.Code)
			var exchangeError tokenExchangeError
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &exchangeError))
			require.Equal(t, test.wantCode, exchangeError.Error)
		})
	}
}

func TestDiscoveryPublishesMatchingJWKS(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, staticValidator{})

	discoveryResponse := httptest.NewRecorder()
	server.handler().ServeHTTP(discoveryResponse, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/openid-configuration", http.NoBody))
	require.Equal(t, http.StatusOK, discoveryResponse.Code)
	var discovery map[string]any
	require.NoError(t, json.Unmarshal(discoveryResponse.Body.Bytes(), &discovery))
	require.Equal(t, server.issuer, discovery["issuer"])
	require.Equal(t, server.jwksURL, discovery["jwks_uri"])

	jwksResponse := httptest.NewRecorder()
	server.handler().ServeHTTP(jwksResponse, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/jwks.json", http.NoBody))
	require.Equal(t, http.StatusOK, jwksResponse.Code)
	var keys jose.JSONWebKeySet
	require.NoError(t, json.Unmarshal(jwksResponse.Body.Bytes(), &keys))
	require.Len(t, keys.Keys, 1)
	require.Equal(t, server.publicKey.KeyID, keys.Keys[0].KeyID)
	require.True(t, keys.Keys[0].IsPublic())
}

func validForm(token string) url.Values {
	return url.Values{
		"grant_type":         {grantTypeTokenExchange},
		"subject_token":      {token},
		"subject_token_type": {tokenTypeJWT},
	}
}

func newTestServer(t *testing.T, validator tokenValidator) *exchangeServer {
	t.Helper()
	server, err := newExchangeServer(
		"https://capd.dsx.nvid.id",
		"https://127.0.0.1:30444/.well-known/jwks.json",
		"capd",
		"nvid.id",
		validator,
	)
	require.NoError(t, err)
	return server
}
