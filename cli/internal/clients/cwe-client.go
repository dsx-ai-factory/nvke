// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clients wraps the generated CWE API client with typed, error-parsed
// methods, management-cluster authentication, and optional proxy TLS client
// authentication.
package clients

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/dsx-ai-factory/nke/cli/internal/auth"
	"github.com/dsx-ai-factory/nke/cli/internal/config"
	api "github.com/dsx-ai-factory/nke/controller/api"
)

// CWEClient is a high-level client for the CWE REST API.
type CWEClient struct {
	raw *api.ClientWithResponses
}

type tokenSource interface {
	Mint(context.Context) (string, error)
}

// New builds a CWEClient from a SiteConfig.
// The URL should be the site controller base URL (e.g. http://localhost:8080).
// The /api prefix is added automatically so callers never need to include it.
func New(cfg config.SiteConfig) (*CWEClient, error) {
	var tokens tokenSource
	if cfg.Kubeconfig != "" {
		source, err := auth.NewSource(cfg.Kubeconfig, cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("building CWE authentication: %w", err)
		}
		tokens = source
	}
	return newClient(cfg, tokens)
}

func newClient(cfg config.SiteConfig, tokens tokenSource) (*CWEClient, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("site URL is empty: run 'nkectl config set-site --url <url>'")
	}
	if (cfg.ProxyClientCertFile == "") != (cfg.ProxyClientKeyFile == "") {
		return nil, fmt.Errorf("proxy TLS client certificate and key must be set together")
	}
	apiURL, err := url.JoinPath(cfg.URL, "api")
	if err != nil {
		return nil, fmt.Errorf("building API URL: %w", err)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	if cfg.ProxyClientCertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ProxyClientCertFile, cfg.ProxyClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading TLS client cert/key: %w", err)
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		httpClient.Transport = transport
	}
	opts := []api.ClientOption{api.WithHTTPClient(httpClient)}
	if tokens != nil {
		opts = append(opts, api.WithRequestEditorFn(bearerToken(tokens)))
	}
	raw, err := api.NewClientWithResponses(apiURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("building CWE client: %w", err)
	}
	return &CWEClient{raw: raw}, nil
}

func bearerToken(tokens tokenSource) api.RequestEditorFn {
	return func(ctx context.Context, req *http.Request) error {
		token, err := tokens.Mint(ctx)
		if err != nil {
			return fmt.Errorf("minting CWE bearer token: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return nil
	}
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
