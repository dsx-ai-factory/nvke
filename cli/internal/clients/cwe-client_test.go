// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
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
