// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/nke/cli/internal/config"
	api "github.com/NVIDIA/nke/controller/api"
)

type fakeTokenSource struct {
	token string
	err   error
}

func (s fakeTokenSource) Mint(context.Context) (string, error) {
	return s.token, s.err
}

func TestBearerTokenAttachesMintedToken(t *testing.T) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := bearerToken(fakeTokenSource{token: "minted-token"})(request.Context(), request); err != nil {
		t.Fatalf("bearerToken: %v", err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer minted-token" {
		t.Fatalf("Authorization = %q, want Bearer minted-token", got)
	}
}

func TestBearerTokenOmitsHeaderWhenAuthenticationIsDisabled(t *testing.T) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := bearerToken(fakeTokenSource{})(request.Context(), request); err != nil {
		t.Fatalf("bearerToken: %v", err)
	}
	if got := request.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty", got)
	}
}

func TestBearerTokenReturnsMintError(t *testing.T) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}

	err = bearerToken(fakeTokenSource{err: errors.New("denied")})(request.Context(), request)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("bearerToken error = %v, want denied", err)
	}
	if got := request.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q after mint failure, want empty", got)
	}
}

func TestNewRequiresProxyClientCertificateAndKeyTogether(t *testing.T) {
	tests := map[string]config.SiteConfig{
		"certificate only": {
			URL:                 "https://site.example.com",
			ProxyClientCertFile: "/client.crt",
		},
		"key only": {
			URL:                "https://site.example.com",
			ProxyClientKeyFile: "/client.key",
		},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), "must be set together") {
				t.Fatalf("New() error = %v, want incomplete pair error", err)
			}
		})
	}
}

func TestNewConfiguresProxyClientCertificateOnDefaultTransport(t *testing.T) {
	certFile, keyFile := writeClientCertificate(t)
	client, err := New(config.SiteConfig{
		URL:                 "https://site.example.com",
		ProxyClientCertFile: certFile,
		ProxyClientKeyFile:  keyFile,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rawClient, ok := client.raw.ClientInterface.(*api.Client)
	if !ok {
		t.Fatalf("ClientInterface type = %T, want *api.Client", client.raw.ClientInterface)
	}
	httpClient, ok := rawClient.Client.(*http.Client)
	if !ok {
		t.Fatalf("HTTP client type = %T, want *http.Client", rawClient.Client)
	}
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", httpClient.Transport)
	}
	if transport.Proxy == nil || transport.DialContext == nil || !transport.ForceAttemptHTTP2 {
		t.Error("transport does not preserve http.DefaultTransport behavior")
	}
	if transport.TLSClientConfig == nil || len(transport.TLSClientConfig.Certificates) != 1 {
		t.Fatal("transport does not contain the configured client certificate")
	}
}

func writeClientCertificate(t *testing.T) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "nkectl-test-client"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}

	dir := t.TempDir()
	certFile := filepath.Join(dir, "client.crt")
	keyFile := filepath.Join(dir, "client.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	return certFile, keyFile
}
