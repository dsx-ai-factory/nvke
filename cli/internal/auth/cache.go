// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type cachedToken struct {
	Value     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (t cachedToken) usableAt(now time.Time) bool {
	return t.Value != "" && t.ExpiresAt.After(now.Add(tokenRenewBefore))
}

type tokenCache struct {
	path   string
	tokens map[string]cachedToken
}

func loadTokenCache() *tokenCache {
	cache := &tokenCache{tokens: make(map[string]cachedToken)}
	dir, err := os.UserCacheDir()
	if err != nil {
		return cache
	}
	cache.path = filepath.Join(dir, "nvkectl", "tokens.json")
	data, err := os.ReadFile(cache.path)
	if err == nil {
		var tokens map[string]cachedToken
		if json.Unmarshal(data, &tokens) == nil && tokens != nil {
			cache.tokens = tokens
		}
	}
	return cache
}

func (c *tokenCache) store(endpoint string, token cachedToken) {
	c.tokens[endpoint] = token
	if c.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(c.tokens)
	if err != nil {
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(c.path), ".tokens-*")
	if err != nil {
		return
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return
	}
	if err := temp.Close(); err != nil {
		return
	}
	_ = os.Rename(tempPath, c.path)
}
