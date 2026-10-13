// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
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

func TestProxyTransportConfiguresClientCertificateOnDefaultTransport(t *testing.T) {
	certFile, keyFile := writeClientCertificate(t)
	rt, err := proxyTransport(config.SiteConfig{
		URL:                 "https://site.example.com",
		ProxyClientCertFile: certFile,
		ProxyClientKeyFile:  keyFile,
	})
	if err != nil {
		t.Fatalf("proxyTransport: %v", err)
	}
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", rt)
	}
	if transport.Proxy == nil || transport.DialContext == nil || !transport.ForceAttemptHTTP2 {
		t.Error("transport does not preserve http.DefaultTransport behavior")
	}
	if transport.TLSClientConfig == nil || len(transport.TLSClientConfig.Certificates) != 1 {
		t.Fatal("transport does not contain the configured client certificate")
	}
}

func TestNewSendsTraceparentAndUserAgentWithBearerToken(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"clusters":[]}`))
	}))
	defer srv.Close()
	client, err := newClient(config.SiteConfig{URL: srv.URL}, fakeTokenSource{token: "minted-token"})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	run := telemetry.NewRun("v9.9.9")
	ctx := run.Start(context.Background(), "nvkectl get clusters")
	defer run.Finish(nil)
	if _, err := client.ListClusters(ctx, nil); err != nil {
		t.Fatalf("ListClusters: %v", err)
	}

	if want := "Bearer minted-token"; got.Get("Authorization") != want {
		t.Errorf("Authorization = %q, want %q", got.Get("Authorization"), want)
	}
	if want := "nvkectl/v9.9.9"; got.Get("User-Agent") != want {
		t.Errorf("User-Agent = %q, want %q", got.Get("User-Agent"), want)
	}
	if tp := got.Get("Traceparent"); !strings.HasPrefix(tp, "00-"+run.TraceID()+"-") {
		t.Errorf("traceparent = %q, want trace ID %s", tp, run.TraceID())
	}
}

func TestWrappedTransportStillPresentsTheProxyClientCertificate(t *testing.T) {
	var peerCerts int
	var traceparent string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		peerCerts = len(r.TLS.PeerCertificates)
		traceparent = r.Header.Get("Traceparent")
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()

	certFile, keyFile := writeClientCertificate(t)
	base, err := proxyTransport(config.SiteConfig{ProxyClientCertFile: certFile, ProxyClientKeyFile: keyFile})
	if err != nil {
		t.Fatalf("proxyTransport: %v", err)
	}
	// The test server's own certificate is self-signed; only the client side of
	// the handshake is under test.
	base.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // test server

	run := telemetry.NewRun("v1")
	ctx := run.Start(context.Background(), "nvkectl get clusters")
	defer run.Finish(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: telemetry.Transport(base)}).Do(req)
	if err != nil {
		t.Fatalf("request through the wrapped transport: %v", err)
	}
	_ = resp.Body.Close()

	if peerCerts != 1 {
		t.Errorf("server saw %d client certificates, want 1", peerCerts)
	}
	if !strings.HasPrefix(traceparent, "00-"+run.TraceID()+"-") {
		t.Errorf("traceparent = %q, want trace ID %s", traceparent, run.TraceID())
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
		Subject:      pkix.Name{CommonName: "nvkectl-test-client"},
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
