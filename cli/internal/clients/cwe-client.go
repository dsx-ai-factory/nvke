// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clients wraps the generated CWE API client with typed, error-parsed
// methods. Auth is intentionally a no-op for now — the design for how nkectl
// authenticates with CWE is being decided with the team. The URL is the only
// required config for local development.
package clients

import (
	"encoding/json"
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
// FIXME: auth is not yet implemented. A future change will add --kubeconfig
// support to set-site, storing the management cluster kubeconfig in SiteConfig;
// that kubeconfig will be used to mint a short-lived JWT attached as a bearer
// token on every request.
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

// apiError extracts a human-readable message from a non-2xx response body.
// The CWE API returns {"message": "..."} for all error responses (GenericError).
// Falls back to the raw body if the body isn't a GenericError.
func apiError(status string, body []byte) error {
	var e api.GenericError
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return fmt.Errorf("%s: %s", status, e.Message)
	}
	return fmt.Errorf("%s: %s", status, body)
}
