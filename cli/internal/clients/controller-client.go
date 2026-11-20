// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clients wraps the generated controller API client with typed, error-parsed
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
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/internal/auth"
	"github.com/dsx-ai-factory/nvke/cli/internal/config"
	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
	api "github.com/dsx-ai-factory/nvke/controller/api"
)

// ControllerClient is a high-level client for the controller REST API.
type ControllerClient struct {
	raw *api.ClientWithResponses
}

type tokenSource interface {
	Mint(context.Context) (string, error)
}

// New builds a ControllerClient from a SiteConfig.
// The URL should be the site controller base URL (e.g. http://localhost:8080).
// The /api prefix is added automatically so callers never need to include it.
func New(cfg config.SiteConfig) (*ControllerClient, error) {
	var tokens tokenSource
	if cfg.Kubeconfig != "" {
		source, err := auth.NewTokenSource(cfg.Kubeconfig, cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("building controller API authentication: %w", err)
		}
		tokens = source
	}
	return newClient(cfg, tokens)
}

func newClient(cfg config.SiteConfig, tokens tokenSource) (*ControllerClient, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("site URL is empty: run 'nvkectl config set-site --url <url>'")
	}
	if (cfg.ProxyClientCertFile == "") != (cfg.ProxyClientKeyFile == "") {
		return nil, fmt.Errorf("proxy TLS client certificate and key must be set together")
	}
	apiURL, err := url.JoinPath(cfg.URL, "api")
	if err != nil {
		return nil, fmt.Errorf("building API URL: %w", err)
	}
	base, err := proxyTransport(cfg)
	if err != nil {
		return nil, err
	}
	// Outermost, so the proxy mTLS transport stays underneath.
	httpClient := &http.Client{Timeout: 30 * time.Second, Transport: telemetry.Transport(base)}
	opts := []api.ClientOption{api.WithHTTPClient(httpClient)}
	if tokens != nil {
		opts = append(opts, api.WithRequestEditorFn(bearerToken(tokens)))
	}
	raw, err := api.NewClientWithResponses(apiURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("building controller API client: %w", err)
	}
	return &ControllerClient{raw: raw}, nil
}

// proxyTransport returns the transport that presents the site's proxy client
// certificate, or nil when none is configured.
func proxyTransport(cfg config.SiteConfig) (http.RoundTripper, error) {
	if cfg.ProxyClientCertFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.ProxyClientCertFile, cfg.ProxyClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading TLS client cert/key: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	return transport, nil
}

func bearerToken(tokens tokenSource) api.RequestEditorFn {
	return func(ctx context.Context, req *http.Request) error {
		token, err := tokens.Mint(ctx)
		if err != nil {
			return fmt.Errorf("minting controller API bearer token: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return nil
	}
}

// StatusError is an answer from the controller with a status other than the
// one the call expects. Its presence means the request reached the controller
// and was answered; its absence from an error means no answer came back.
type StatusError struct {
	// StatusCode is the HTTP status the controller answered with.
	StatusCode int
	err        error
}

func (e *StatusError) Error() string { return e.err.Error() }

// Messages the controller's authentication and authorization middleware put in
// the {"message": ...} body of a 403.
const (
	notAdmittedPrefix   = "Not admitted"
	notAuthorizedPrefix = "Not authorized"
)

// apiError returns a *StatusError for an answer with status code and status
// line status. Its text is the status line and the body's message, or the raw
// body when it is not the API's {"message": ...} shape. A 401, or a 403 from
// the API's authentication or authorization, also says what the caller can do
// about it.
func apiError(code int, status string, body []byte) error {
	message := strings.TrimSpace(string(body))
	var e api.GenericError
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		message = e.Message
	}
	if hint := refusalHint(code, message); hint != "" {
		// The API's messages end in a full stop; the hint continues the sentence.
		message = strings.TrimSuffix(message, ".") + ": " + hint
	}
	return &StatusError{StatusCode: code, err: fmt.Errorf("%s: %s", status, message)}
}

// refusalHint returns the action that fixes a refusal from the API's
// authentication or authorization, or "" for any other answer. A 403 is matched
// by its message, because a proxy in front of the API can also answer 403.
func refusalHint(code int, message string) string {
	switch {
	case code == http.StatusUnauthorized:
		return "the NVKE API did not accept nvkectl's credential; check the management cluster kubeconfig " +
			"('nvkectl config set-site --kubeconfig <path>') and log in again if it uses OIDC " +
			"(see docs/operate/api-authentication.md)"
	case code != http.StatusForbidden:
		return ""
	case strings.HasPrefix(message, notAdmittedPrefix):
		return "the NVKE API authenticated this identity but admits no such caller; " +
			"ask an NVKE administrator to admit it (see docs/operate/api-authentication.md)"
	case strings.HasPrefix(message, notAuthorizedPrefix):
		return "no role granted to this identity allows this operation; " +
			"ask an NVKE administrator to grant one (see docs/operate/api-authorization.md). " +
			"If nvkectl sent no credential, check the management cluster kubeconfig " +
			"('nvkectl config set-site --kubeconfig <path>')"
	default:
		return ""
	}
}
