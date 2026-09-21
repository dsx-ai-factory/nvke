// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tlsjwt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// grantTypeTokenExchange and subjectTokenTypeJWT are the RFC 8693 values the
	// DSX Identity exchange endpoint requires.
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	subjectTokenTypeJWT    = "urn:ietf:params:oauth:token-type:jwt"

	// maxExchangeResponseSize bounds the response body so an endpoint that
	// streams cannot exhaust agent memory during a handshake.
	maxExchangeResponseSize = 1 << 20
)

// Token is a DSX identity token and the instant it stops being valid.
type Token struct {
	Value  string
	Expiry time.Time
}

// Exchanger trades a SPIRE JWT-SVID for a DSX identity token.
type Exchanger interface {
	Exchange(ctx context.Context, subjectToken string) (Token, error)
}

// ExchangeClient calls the DSX Identity RFC 8693 token-exchange endpoint.
type ExchangeClient struct {
	endpoint string
	client   *http.Client
	now      func() time.Time
}

// NewExchangeClient returns a client for the DSX Identity exchange endpoint.
// An empty caBundlePath uses the system trust store; otherwise the endpoint's
// certificate must chain to the supplied bundle, so a misconfigured bundle
// fails the exchange rather than falling back to system roots.
func NewExchangeClient(endpoint, caBundlePath string, timeout time.Duration) (*ExchangeClient, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parsing DSX Identity exchange endpoint: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("DSX Identity exchange endpoint must be an absolute https URL, got %q", endpoint)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("DSX Identity exchange timeout must be positive, got %s", timeout)
	}

	transport, err := exchangeTransport(caBundlePath)
	if err != nil {
		return nil, err
	}

	return &ExchangeClient{
		endpoint: endpoint,
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:      time.Now,
	}, nil
}

func exchangeTransport(caBundlePath string) (http.RoundTripper, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport has an unexpected type")
	}
	transport = transport.Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}

	if caBundlePath == "" {
		return transport, nil
	}

	bundle, err := os.ReadFile(caBundlePath)
	if err != nil {
		return nil, fmt.Errorf("reading DSX Identity CA bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		return nil, fmt.Errorf("DSX Identity CA bundle %s contains no certificates", caBundlePath)
	}
	transport.TLSClientConfig.RootCAs = roots
	return transport, nil
}

// exchangeResponse is the subset of the RFC 8693 response the agent reads.
type exchangeResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// Exchange posts subjectToken to the DSX Identity exchange endpoint and returns
// the DSX identity token it issues. The endpoint takes no audience parameter:
// it preserves the audience of the validated subject token.
func (c *ExchangeClient) Exchange(ctx context.Context, subjectToken string) (Token, error) {
	if subjectToken == "" {
		return Token{}, errors.New("subject token must not be empty")
	}

	form := url.Values{
		"grant_type":         {grantTypeTokenExchange},
		"subject_token":      {subjectToken},
		"subject_token_type": {subjectTokenTypeJWT},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("building DSX Identity exchange request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	issuedAt := c.now()
	response, err := c.client.Do(request)
	if err != nil {
		return Token{}, fmt.Errorf("calling DSX Identity exchange: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxExchangeResponseSize))
	if err != nil {
		return Token{}, fmt.Errorf("reading DSX Identity exchange response: %w", err)
	}
	// The body can carry the subject token back in an error payload, so report
	// the status without it.
	if response.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("DSX Identity exchange returned HTTP %d", response.StatusCode)
	}

	var decoded exchangeResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Token{}, fmt.Errorf("decoding DSX Identity exchange response: %w", err)
	}
	if decoded.AccessToken == "" {
		return Token{}, errors.New("DSX Identity exchange returned no access token")
	}
	if decoded.ExpiresIn <= 0 {
		return Token{}, fmt.Errorf("DSX Identity exchange returned expires_in %d", decoded.ExpiresIn)
	}

	return Token{
		Value:  decoded.AccessToken,
		Expiry: issuedAt.Add(time.Duration(decoded.ExpiresIn) * time.Second),
	}, nil
}
