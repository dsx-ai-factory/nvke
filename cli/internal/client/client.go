// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package client wraps the generated CWE API client with typed, error-parsed
// methods. Auth is intentionally a no-op for now — the design for how nkectl
// authenticates with CWE is being decided with the team. The URL is the only
// required config for local development.
package client

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/NVIDIA/nke/cli/internal/config"
	api "github.com/NVIDIA/nke/controller/api"
)

// CWEClient is a high-level client for the CWE REST API.
type CWEClient struct {
	raw *api.ClientWithResponses
}

// New builds a CWEClient from a SiteConfig.
// The URL should be the site controller base URL (e.g. http://localhost:8080).
// The /api prefix is added automatically so callers never need to include it.
// FIXME: NKX-14307 — add mTLS support: accept cert/key paths from SiteConfig so
// operators can authenticate via Teleport app certs (tsh apps login → --cert/--key).
func New(cfg config.SiteConfig) (*CWEClient, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("site URL is empty: run 'nkectl config set-site --url <url>'")
	}
	apiURL, err := url.JoinPath(cfg.URL, "api")
	if err != nil {
		return nil, fmt.Errorf("building API URL: %w", err)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	raw, err := api.NewClientWithResponses(apiURL, api.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("building CWE client: %w", err)
	}
	return &CWEClient{raw: raw}, nil
}
