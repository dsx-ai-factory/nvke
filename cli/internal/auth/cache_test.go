// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTokenCacheRoundTrip(t *testing.T) {
	const endpoint = "https://nke.example.internal"
	token := cachedToken{
		Value:     "cached-token",
		ExpiresAt: time.Now().Add(tokenLifetime),
	}
	path := filepath.Join(t.TempDir(), "nkectl", "tokens.json")
	cache := &tokenCache{
		path:   path,
		tokens: make(map[string]cachedToken),
	}

	cache.store(endpoint, token)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var loaded map[string]cachedToken
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	got, ok := loaded[endpoint]
	if !ok {
		t.Fatal("cache did not contain the stored endpoint")
	}
	if got.Value != token.Value || !got.ExpiresAt.Equal(token.ExpiresAt) {
		t.Fatalf("cached token = %#v, want %#v", got, token)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Fatalf("cache mode = %#o, want 0600", gotMode)
	}
}
