// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServerPublishesHTTPSDiscoveryAndSignsWithJWKSKey(t *testing.T) {
	server, err := Start(Config{ListenAddress: "0.0.0.0:0", AdvertiseHost: "127.0.0.1", Audience: "kubernetes"})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	client := trustedClient(t, server.CAPEM())
	discovery := getJSON[struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}](t, client, server.IssuerURL()+"/.well-known/openid-configuration")
	if discovery.Issuer != server.IssuerURL() || discovery.JWKSURI != server.IssuerURL()+"/keys" {
		t.Fatalf("discovery = %+v", discovery)
	}

	keys := getJSON[struct {
		Keys []map[string]string `json:"keys"`
	}](t, client, discovery.JWKSURI)
	if len(keys.Keys) != 1 || keys.Keys[0]["alg"] != "ES256" {
		t.Fatalf("JWKS = %+v", keys)
	}
	token, err := server.Sign(TokenSpec{Claims: map[string]any{"sub": "alice", "groups": []string{"operators"}}})
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	claims := verifyToken(t, token, keys.Keys[0])
	if claims["iss"] != server.IssuerURL() || claims["aud"].([]any)[0] != server.Audience() || claims["sub"] != "alice" {
		t.Fatalf("claims = %+v", claims)
	}

	untrusted, err := server.Sign(TokenSpec{UntrustedKey: true})
	if err != nil {
		t.Fatalf("Sign(untrusted) error = %v", err)
	}
	if tokenVerifies(untrusted, keys.Keys[0]) {
		t.Fatal("token signed by unpublished key verified with JWKS key")
	}
}

func TestSignRejectsRegisteredClaimInClaimsMap(t *testing.T) {
	server, err := Start(Config{Audience: "kubernetes"})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if _, err := server.Sign(TokenSpec{Claims: map[string]any{"iss": "replacement"}}); err == nil {
		t.Fatal("Sign() accepted registered claim in Claims")
	}
}

func trustedClient(t *testing.T, caPEM []byte) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("CAPEM() did not return a certificate")
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}}
}

func getJSON[T any](t *testing.T, client *http.Client, url string) T {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("create GET %s: %v", url, err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", url, response.StatusCode)
	}
	var value T
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return value
}

func verifyToken(t *testing.T, token string, jwk map[string]string) map[string]any {
	t.Helper()
	if !tokenVerifies(token, jwk) {
		t.Fatal("JWT signature did not verify with JWKS key")
	}
	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal JWT claims: %v", err)
	}
	return claims
}

func tokenVerifies(token string, jwk map[string]string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	xBytes, xErr := base64.RawURLEncoding.DecodeString(jwk["x"])
	yBytes, yErr := base64.RawURLEncoding.DecodeString(jwk["y"])
	signature, signatureErr := base64.RawURLEncoding.DecodeString(parts[2])
	if xErr != nil || yErr != nil || signatureErr != nil || len(signature) != 64 {
		return false
	}
	point := append([]byte{4}, xBytes...)
	point = append(point, yBytes...)
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:]))
}
