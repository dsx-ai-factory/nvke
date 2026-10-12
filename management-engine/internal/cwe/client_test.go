// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cwe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	api "github.com/dsx-ai-factory/nvke/controller/api"
)

func TestTokenRotation(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken := func(value string) {
		t.Helper()
		if err := os.WriteFile(tokenFile, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeToken("example-token-one\n")
	var headers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Get("Authorization"))
		writeJSON(t, w, api.ListClustersResponse{Clusters: []api.Cluster{}})
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, tokenFile)
	for _, token := range []string{"example-token-one", "example-token-two"} {
		writeToken(token)
		if _, err := client.FindByName(t.Context(), "example"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("find with rotating token: %v", err)
		}
	}
	if want := []string{"Bearer example-token-one", "Bearer example-token-two"}; !reflect.DeepEqual(headers, want) {
		t.Errorf("authorization = %v, want %v", headers, want)
	}
	writeToken("example-token\ninjected-header")
	if _, err := client.FindByName(t.Context(), "example"); err == nil || strings.Contains(err.Error(), "example-token") {
		t.Errorf("malformed token should fail without its contents: %v", err)
	}
	if len(headers) != 2 {
		t.Error("request with malformed token reached the server")
	}
}

func TestResponseValidation(t *testing.T) {
	id := uuid.New()
	valid := fmt.Sprintf(`{"id":%q,"name":"expected"}`, id)
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{`},
		{name: "oversized", body: valid + strings.Repeat(" ", maxResponseBytes)},
		{name: "trailing object", body: valid + `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL, "")
			if _, err := client.Get(t.Context(), id.String()); !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("got %v, want invalid response", err)
			}
		})
	}
}

func TestHTTPFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"sensitive server detail"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, "")
	_, err := client.FindByName(t.Context(), "example")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("error = %v, want HTTP %d", err, http.StatusInternalServerError)
	}
	if strings.Contains(err.Error(), "sensitive") {
		t.Error("response details leaked into error")
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(target.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, "")
	err := client.Delete(t.Context(), uuid.NewString())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTemporaryRedirect || reached.Load() {
		t.Fatalf("redirect handling: error %v, followed %v", err, reached.Load())
	}
}

func TestTLSVerificationAndCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, "")
	if _, err := client.FindByName(t.Context(), "example"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("untrusted TLS certificate was accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.FindByName(ctx, "example"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request: %v", err)
	}
}

func TestEndpointConfiguration(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{input: "https://cwe.example", want: "https://cwe.example/api"},
		{input: "http://localhost:8080/", want: "http://localhost:8080/api"},
		{input: "https://cwe.example/api/", want: "https://cwe.example/api"},
		{input: "https://cwe.example/site/api", want: "https://cwe.example/site/api"},
		{input: "https://cwe.example/site/", want: "https://cwe.example/site/api"},
	} {
		t.Run(test.input, func(t *testing.T) {
			client := newTestClient(t, test.input, "")
			if client.Endpoint() != test.want {
				t.Errorf("endpoint = %s, want %s", client.Endpoint(), test.want)
			}
		})
	}
	for _, endpoint := range []string{"", "/api", "ftp://cwe.example", "https:///api", "https://user:REDACTED@cwe.example", "https://cwe.example?token=REDACTED", "https://cwe.example?", "https://cwe.example#fragment"} {
		if _, err := New(Config{Endpoint: endpoint}); err == nil || strings.Contains(err.Error(), "REDACTED") {
			t.Errorf("unsafe endpoint must fail without sensitive details: %v", err)
		}
	}
	if _, err := New(Config{Endpoint: "https://cwe.example", TokenFile: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("missing configured token file should fail at startup")
	}
}

func newTestClient(t *testing.T, endpoint, tokenFile string) *Client {
	t.Helper()
	client, err := New(Config{Endpoint: endpoint, TokenFile: tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
