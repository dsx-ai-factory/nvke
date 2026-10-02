// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTokenPassesPersonalKeyThrough(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()

	src := newTokenSource(srv.Client(), "nvapi-example", "0123", srv.URL)
	token, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if token != "nvapi-example" {
		t.Errorf("token = %q, want the key itself", token)
	}
	if calls.Load() != 0 {
		t.Error("an nvapi- key needs no exchange")
	}
}

func TestTokenExchangesLegacyKey(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("$oauthtoken:legacy-key"))
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		if got := r.URL.Query().Get("scope"); got != "group/ngc:0123" {
			t.Errorf("scope = %q, want the org's group", got)
		}
		if got := r.URL.Query().Get("service"); got != "ngc" {
			t.Errorf("service = %q, want ngc", got)
		}
		_, _ = w.Write([]byte(`{"token":"exchanged"}`))
	}))
	defer srv.Close()

	src := newTokenSource(srv.Client(), "legacy-key", "0123", srv.URL)
	for range 2 {
		token, err := src.Token(context.Background())
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		if token != "exchanged" {
			t.Errorf("token = %q, want the exchanged one", token)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("exchanged %d times, want the token reused", calls.Load())
	}
}

func TestTokenExchangeFailureDoesNotEchoTheReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("secret-bearing reply"))
	}))
	defer srv.Close()

	_, err := newTokenSource(srv.Client(), "legacy-key", "0123", srv.URL).Token(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret-bearing") || strings.Contains(err.Error(), "legacy-key") {
		t.Errorf("error leaks the credential exchange: %v", err)
	}
}

func TestDefaultAuthURL(t *testing.T) {
	if got := defaultAuthURL(DefaultEndpoint); got != authURL {
		t.Errorf("production endpoint paired with %s", got)
	}
	if got := defaultAuthURL("https://api.stg.ngc.nvidia.com"); got != stagingAuthURL {
		t.Errorf("staging endpoint paired with %s", got)
	}
}
