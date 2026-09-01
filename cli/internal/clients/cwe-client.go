// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clients wraps the generated CWE API client with typed, error-parsed
// methods. Auth is intentionally a no-op for now — the design for how nkectl
// authenticates with CWE is being decided with the team. The URL is the only
// required config for local development.
package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/NVIDIA/nke/cli/internal/config"
	api "github.com/NVIDIA/nke/controller/api"
)

// devTokenEnvVar, when set, is sent as a static bearer token on every CWE
// request. This is a stopgap for local/dev testing only — it is not the
// TokenRequest-based auth flow described in the SADD, which mints a
// short-lived JWT per call from the management cluster kubeconfig and is
// being implemented separately.
//
// To test against a CWE instance that enforces auth:
//
//	export NKECTL_DEV_TOKEN=$(kubectl create token nke-client -n nke-clients --audience https://nke.tilt.local --duration 1h)
//
// FIXME: remove once real auth lands in nkectl.
const devTokenEnvVar = "NKECTL_DEV_TOKEN"

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
	opts := []api.ClientOption{api.WithHTTPClient(httpClient)}
	if token := os.Getenv(devTokenEnvVar); token != "" {
		opts = append(opts, api.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)
			return nil
		}))
	}
	raw, err := api.NewClientWithResponses(apiURL, opts...)
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
