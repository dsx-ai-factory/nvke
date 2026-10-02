// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	authURL        = "https://authn.nvidia.com/token"
	stagingAuthURL = "https://stg.auth.ngc.nvidia.com/token"

	// tokenReuse is shorter than any lifetime NGC has been seen to issue, so
	// a token is never presented stale. An exchange is one small request.
	tokenReuse = 5 * time.Minute
)

// defaultAuthURL pairs the token service with the API endpoint: a staging
// token is rejected by production and the reverse.
func defaultAuthURL(endpoint string) string {
	if strings.Contains(endpoint, ".stg.") {
		return stagingAuthURL
	}
	return authURL
}

// tokenSource turns an NGC key into a bearer token for the Forge API.
type tokenSource struct {
	client  *http.Client
	key     string
	org     string
	authURL string
	staging bool

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newTokenSource(client *http.Client, key, org, authURL string) *tokenSource {
	return &tokenSource{
		client:  client,
		key:     strings.TrimSpace(key),
		org:     org,
		authURL: authURL,
		staging: authURL == stagingAuthURL,
	}
}

// Token returns a bearer token. A personal or service key beginning nvapi-
// is accepted by the API directly; a legacy key is exchanged for a token
// scoped to the org.
func (s *tokenSource) Token(ctx context.Context) (string, error) {
	if strings.HasPrefix(s.key, "nvapi-") {
		return s.key, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expires) {
		return s.token, nil
	}

	scope := "group/ngc:" + s.org
	if s.staging {
		scope = "group/ngc-stg"
	}
	query := url.Values{"service": {"ngc"}, "scope": {scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.authURL+"?"+query.Encode(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("building the token request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("$oauthtoken:"+s.key)))

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchanging the API key for a token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading the token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body is not echoed: it is an auth service's reply to a
		// credential, and a run log is not where that belongs.
		return "", fmt.Errorf("exchanging the API key for a token: %s", resp.Status)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("the token response carried no token")
	}
	s.token, s.expires = out.Token, time.Now().Add(tokenReuse)
	return s.token, nil
}
