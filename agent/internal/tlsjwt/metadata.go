// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tlsjwt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// metadataIdentityPath is the DSX metadata-service endpoint that returns
	// an already exchanged DSX identity token for the requested audience.
	metadataIdentityPath = "/latest/meta-data/identity"

	// maxMetadataResponseSize bounds the token body read during a handshake.
	maxMetadataResponseSize = 64 << 10
)

// MetadataClient fetches DSX identity tokens from the DSX metadata service,
// which a VM reaches at 169.254.169.254.
type MetadataClient struct {
	endpoint string
	client   *http.Client
}

// NewMetadataClient returns a client for the DSX metadata service at baseURL,
// for example http://169.254.169.254.
func NewMetadataClient(baseURL string, timeout time.Duration) (*MetadataClient, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing dsx metadata service url: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("dsx metadata service url must be an absolute http(s) url with no path, got %q", baseURL)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("dsx metadata service timeout must be positive, got %s", timeout)
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default http transport has an unexpected type")
	}
	transport = transport.Clone()
	// The metadata service is only reachable from the VM's own pod network, so
	// a node-wide HTTP_PROXY must not carry this request.
	transport.Proxy = nil

	parsed.Path = metadataIdentityPath
	return &MetadataClient{
		endpoint: parsed.String(),
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Fetch returns the DSX identity token the metadata service issues for audience.
func (c *MetadataClient) Fetch(ctx context.Context, audience string) (Token, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"?"+url.Values{"aud": {audience}}.Encode(), nil)
	if err != nil {
		return Token{}, fmt.Errorf("building dsx metadata service request: %w", err)
	}
	request.Header.Set("Metadata", "true")
	request.Header.Set("Accept", "text/plain")

	response, err := c.client.Do(request)
	if err != nil {
		return Token{}, fmt.Errorf("calling dsx metadata service: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataResponseSize))
	if err != nil {
		return Token{}, fmt.Errorf("reading dsx metadata service response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("dsx metadata service returned http %d", response.StatusCode)
	}

	value := strings.TrimSpace(string(body))
	if value == "" {
		return Token{}, errors.New("dsx metadata service returned an empty dsx identity token")
	}
	expiry, err := unverifiedExpiry(value)
	if err != nil {
		return Token{}, fmt.Errorf("reading dsx metadata service token expiry: %w", err)
	}
	return Token{Value: value, Expiry: expiry}, nil
}

// unverifiedExpiry reads the exp claim without verifying the signature. The
// value only schedules the refresh; the controller verifies the token.
func unverifiedExpiry(token string) (time.Time, error) {
	parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing token: %w", err)
	}
	expiry, err := parsed.Claims.GetExpirationTime()
	if err != nil {
		return time.Time{}, fmt.Errorf("reading token exp claim: %w", err)
	}
	if expiry == nil {
		return time.Time{}, errors.New("token has no exp claim")
	}
	return expiry.Time, nil
}

// NewMetadataTokenSource returns a caching token source over the DSX metadata
// service. The service performs the DSX Identity exchange itself, so no SPIRE
// agent or exchange endpoint is needed on the node.
func NewMetadataTokenSource(client *MetadataClient, audience string) (*CachingTokenSource, error) {
	if client == nil {
		return nil, errors.New("node-agent token source requires a dsx metadata service client")
	}
	if err := validateAudience(audience); err != nil {
		return nil, err
	}
	return newCachingTokenSource(func(ctx context.Context) (Token, error) {
		token, err := client.Fetch(ctx, audience)
		if err != nil {
			return Token{}, fmt.Errorf("fetching dsx identity token for audience %s from the metadata service: %w", audience, err)
		}
		return token, nil
	}), nil
}
