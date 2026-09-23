// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type serverOptions struct {
	issuer    string
	tenant    string
	region    string
	partition string
}

type imdsServer struct {
	options   serverOptions
	signer    jose.Signer
	publicKey jose.JSONWebKey
	now       func() time.Time
}

func newIMDSServer(options serverOptions) (*imdsServer, error) {
	if options.issuer == "" || options.tenant == "" || options.region == "" || options.partition == "" {
		return nil, fmt.Errorf("issuer, tenant, region, and partition are required")
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	publicKey := jose.JSONWebKey{Key: &privateKey.PublicKey, Algorithm: string(jose.ES256), Use: "sig"}
	thumbprint, err := publicKey.Thumbprint(crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("calculate signing key thumbprint: %w", err)
	}
	publicKey.KeyID = base64.RawURLEncoding.EncodeToString(thumbprint)
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: privateKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), publicKey.KeyID),
	)
	if err != nil {
		return nil, fmt.Errorf("create JWT signer: %w", err)
	}
	return &imdsServer{options: options, signer: signer, publicKey: publicKey, now: time.Now}, nil
}

func (s *imdsServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /latest/meta-data/identity", s.handleIdentity)
	mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKS)
	mux.HandleFunc("GET /live", handleHealthy)
	mux.HandleFunc("GET /ready", handleHealthy)
	return mux
}

func (s *imdsServer) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Metadata") != "true" {
		http.Error(w, "Metadata header must be true", http.StatusForbidden)
		return
	}
	audience := r.URL.Query().Get("aud")
	if audience == "" {
		http.Error(w, "aud query parameter is required", http.StatusBadRequest)
		return
	}

	host := r.RemoteAddr
	if parsedHost, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = parsedHost
	}
	digest := sha256.Sum256([]byte(host))
	subject := fmt.Sprintf(
		"spiffe://%s.%s.dsx.%s/bm/host-%x",
		s.options.tenant,
		s.options.region,
		s.options.partition,
		digest[:8],
	)
	now := s.now()
	token, err := jwt.Signed(s.signer).Claims(map[string]any{
		"aud": audience,
		"exp": now.Add(5 * time.Minute).Unix(),
		"iat": now.Unix(),
		"iss": s.options.issuer,
		"sub": subject,
	}).Serialize()
	if err != nil {
		http.Error(w, "failed to sign host identity", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(token))
}

func (s *imdsServer) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	if err := json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{s.publicKey}}); err != nil {
		http.Error(w, "failed to encode signing keys", http.StatusInternalServerError)
	}
}

func handleHealthy(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
