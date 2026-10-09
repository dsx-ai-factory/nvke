// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package oidc provides an in-process OIDC issuer for tests with no Kubernetes dependencies.
package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	algorithm, keyID     = "ES256", "nke-oidc-test"
	defaultListenAddress = "127.0.0.1:0"
	defaultTokenLifetime = 10 * time.Minute
)

// Config controls the listener and values advertised to clients.
type Config struct {
	ListenAddress, AdvertiseHost, Audience string
}

// TokenSpec controls one token's audience, private claims, and signing key.
type TokenSpec struct {
	Audience     []string
	Claims       map[string]any
	UntrustedKey bool
}

// Server is an ephemeral HTTPS OIDC issuer.
type Server struct {
	issuerURL, audience string
	caPEM               []byte
	trusted, untrusted  *ecdsa.PrivateKey
	http                *http.Server
	listener            net.Listener
	closeOnce           sync.Once
	closeErr            error
}

// Start creates an HTTPS issuer with runtime-generated TLS and signing keys.
func Start(config Config) (*Server, error) {
	if strings.TrimSpace(config.Audience) == "" {
		return nil, errors.New("audience is required")
	}
	address := config.ListenAddress
	if address == "" {
		address = defaultListenAddress
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	host := config.AdvertiseHost
	if host == "" {
		host, _, err = net.SplitHostPort(listener.Addr().String())
		if err != nil {
			_ = listener.Close()
			return nil, fmt.Errorf("read listener address: %w", err)
		}
	}
	ip := net.ParseIP(host)
	if host == "" || ip != nil && ip.IsUnspecified() {
		_ = listener.Close()
		return nil, errors.New("advertise host must identify a reachable host")
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("read listener port: %w", err)
	}
	issuerURL := "https://" + net.JoinHostPort(host, port)

	trusted, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	untrusted, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("generate untrusted signing key: %w", err)
	}
	certificate, caPEM, err := newTLSCertificate(host)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	server := &Server{
		issuerURL: issuerURL,
		audience:  config.Audience,
		caPEM:     caPEM,
		trusted:   trusted,
		untrusted: untrusted,
		listener:  listener,
	}
	server.http = &http.Server{
		Handler:           server.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{certificate},
		},
	}
	go func() {
		_ = server.http.Serve(tls.NewListener(listener, server.http.TLSConfig))
	}()
	return server, nil
}

// IssuerURL returns the exact issuer claim and discovery URL base.
func (s *Server) IssuerURL() string { return s.issuerURL }

// CAPEM returns a copy of the CA certificate that signed the HTTPS certificate.
func (s *Server) CAPEM() []byte { return slices.Clone(s.caPEM) }

// Audience returns the configured default audience.
func (s *Server) Audience() string { return s.audience }

// Sign creates an ES256 JWT valid for ten minutes. An empty audience uses the server's default.
func (s *Server) Sign(spec TokenSpec) (string, error) {
	for _, name := range []string{"iss", "aud", "iat", "nbf", "exp"} {
		if _, found := spec.Claims[name]; found {
			return "", fmt.Errorf("registered claim %q cannot be set in Claims", name)
		}
	}
	now := time.Now().UTC()
	if len(spec.Audience) == 0 {
		spec.Audience = []string{s.audience}
	}

	claims := make(map[string]any, len(spec.Claims)+4)
	for name, value := range spec.Claims {
		claims[name] = value
	}
	claims["iss"] = s.issuerURL
	claims["aud"] = spec.Audience
	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(defaultTokenLifetime).Unix()
	key := s.trusted
	if spec.UntrustedKey {
		key = s.untrusted
	}
	return signJWT(key, claims)
}

// Close stops the issuer. It is safe to call more than once.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.http.Close()
	})
	return s.closeErr
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", getOnly(func(response http.ResponseWriter) {
		writeJSON(response, map[string]any{
			"issuer": s.issuerURL, "jwks_uri": s.issuerURL + "/keys",
			"response_types_supported": []string{"id_token"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{algorithm},
		})
	}))
	mux.HandleFunc("/keys", getOnly(func(response http.ResponseWriter) {
		writeJSON(response, map[string]any{"keys": []any{publicJWK(&s.trusted.PublicKey)}})
	}))
	return mux
}

func getOnly(next func(http.ResponseWriter)) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			response.Header().Set("Allow", "GET, HEAD")
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(response)
	}
}

func writeJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		http.Error(response, "encode response", http.StatusInternalServerError)
	}
}

func publicJWK(key *ecdsa.PublicKey) map[string]string {
	point, _ := key.Bytes()
	return map[string]string{
		"kty": "EC", "use": "sig", "alg": algorithm, "kid": keyID, "crv": "P-256",
		"x": base64.RawURLEncoding.EncodeToString(point[1:33]),
		"y": base64.RawURLEncoding.EncodeToString(point[33:]),
	}
}

func signJWT(key *ecdsa.PrivateKey, claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": algorithm, "kid": keyID, "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("marshal JWT header: %w", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal JWT claims: %w", err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	r, ss, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}
	signature := append(r.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func newTLSCertificate(host string) (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate TLS CA key: %w", err)
	}
	now := time.Now().UTC()
	caSerial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate TLS CA certificate serial: %w", err)
	}
	ca := &x509.Certificate{SerialNumber: caSerial, Subject: pkix.Name{CommonName: "NKE OIDC test CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("create TLS CA certificate: %w", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("parse TLS CA certificate: %w", err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate TLS server key: %w", err)
	}
	leafSerial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate TLS server certificate serial: %w", err)
	}
	leaf := &x509.Certificate{SerialNumber: leafSerial, Subject: pkix.Name{CommonName: host}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(host); ip != nil {
		leaf.IPAddresses = []net.IP{ip}
	} else {
		leaf.DNSNames = []string{host}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCertificate, &serverKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("create TLS server certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("marshal TLS server key: %w", err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	return certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}
