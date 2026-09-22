// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tlsjwt

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
)

// ControllerAudience is the audience the agent requests for its JWT-SVID and
// which the controller requires to be the token's only audience. The
// controller pins the same string as auth.NodeAgentAudience.
const ControllerAudience = "nke-controller"

// DefaultRefreshBefore is how far ahead of expiry a cached token is replaced,
// leaving room for a handshake that starts just before the boundary.
const DefaultRefreshBefore = 30 * time.Second

// JWTSVIDFetcher is the part of workloadapi.JWTSource the agent uses.
type JWTSVIDFetcher interface {
	FetchJWTSVID(ctx context.Context, params jwtsvid.Params) (*jwtsvid.SVID, error)
}

// FetchSVIDFunc returns a serialized JWT-SVID for audience.
type FetchSVIDFunc func(ctx context.Context, audience string) (string, error)

// WorkloadAPISVIDs adapts a SPIRE Workload API JWT source to FetchSVIDFunc.
func WorkloadAPISVIDs(fetcher JWTSVIDFetcher) FetchSVIDFunc {
	return func(ctx context.Context, audience string) (string, error) {
		svid, err := fetcher.FetchJWTSVID(ctx, jwtsvid.Params{Audience: audience})
		if err != nil {
			return "", err
		}
		if svid == nil {
			return "", errors.New("workload api returned no jwt-svid")
		}
		return svid.Marshal(), nil
	}
}

// TokenSource hands out the DSX identity token for the authentication record.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// CachingTokenSource fetches a JWT-SVID for the controller audience, exchanges
// it for a DSX identity token, and reuses that token until it is close to
// expiring. Refreshes are serialized so a burst of connections triggers one
// exchange rather than one per connection.
type CachingTokenSource struct {
	fetchSVID     FetchSVIDFunc
	exchanger     Exchanger
	audience      string
	refreshBefore time.Duration
	now           func() time.Time

	mu     sync.Mutex
	cached Token
}

// NewCachingTokenSource returns a token source over the workload API and the
// DSX Identity exchange.
func NewCachingTokenSource(fetchSVID FetchSVIDFunc, exchanger Exchanger) (*CachingTokenSource, error) {
	if fetchSVID == nil {
		return nil, errors.New("node-agent token source requires a jwt-svid source")
	}
	if exchanger == nil {
		return nil, errors.New("node-agent token source requires a token exchanger")
	}
	return &CachingTokenSource{
		fetchSVID:     fetchSVID,
		exchanger:     exchanger,
		audience:      ControllerAudience,
		refreshBefore: DefaultRefreshBefore,
		now:           time.Now,
	}, nil
}

// Token returns a cached DSX identity token, refreshing it first when none is
// cached or the cached one is within refreshBefore of expiring. Every failure
// path returns an error and leaves the cache untouched, so a failed refresh
// cannot replace a usable token with an unusable one.
func (s *CachingTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cached.Value != "" && s.now().Before(s.cached.Expiry.Add(-s.refreshBefore)) {
		return s.cached.Value, nil
	}

	svid, err := s.fetchSVID(ctx, s.audience)
	if err != nil {
		return "", fmt.Errorf("fetching jwt-svid for audience %s: %w", s.audience, err)
	}
	if svid == "" {
		return "", fmt.Errorf("workload api returned an empty jwt-svid for audience %s", s.audience)
	}

	token, err := s.exchanger.Exchange(ctx, svid)
	if err != nil {
		return "", fmt.Errorf("exchanging jwt-svid for a dsx identity token: %w", err)
	}
	if token.Value == "" {
		return "", errors.New("token exchange returned an empty dsx identity token")
	}
	if !s.now().Add(s.refreshBefore).Before(token.Expiry) {
		return "", fmt.Errorf("token exchange returned a token too close to expiry at %s", token.Expiry.UTC())
	}

	s.cached = token
	return token.Value, nil
}
