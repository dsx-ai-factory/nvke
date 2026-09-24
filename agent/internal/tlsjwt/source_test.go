// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tlsjwt

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubSVIDSource struct {
	mu        sync.Mutex
	calls     int
	audiences []string
	svid      string
	err       error
}

func (s *stubSVIDSource) fetch(_ context.Context, audience string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.audiences = append(s.audiences, audience)
	return s.svid, s.err
}

func (s *stubSVIDSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type stubExchanger struct {
	mu       sync.Mutex
	calls    int
	subjects []string
	tokens   []Token
	err      error
}

func (e *stubExchanger) Exchange(_ context.Context, subjectToken string) (Token, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.subjects = append(e.subjects, subjectToken)
	if e.err != nil {
		return Token{}, e.err
	}
	token := e.tokens[min(e.calls-1, len(e.tokens)-1)]
	return token, nil
}

func (e *stubExchanger) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func TestNewCachingTokenSource_RequiresItsCollaborators(t *testing.T) {
	t.Parallel()

	t.Run("nil SVID source", func(t *testing.T) {
		t.Parallel()

		source, err := NewCachingTokenSource(nil, &stubExchanger{})
		assert.Nil(t, source)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "jwt-svid source")
	})

	t.Run("nil exchanger", func(t *testing.T) {
		t.Parallel()

		source, err := NewCachingTokenSource((&stubSVIDSource{}).fetch, nil)
		assert.Nil(t, source)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "token exchanger")
	})
}

func TestToken_RequestsTheControllerAudience(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	svids := &stubSVIDSource{svid: "svid-a"}
	exchanger := &stubExchanger{tokens: []Token{{Value: "dsx-a", Expiry: now.Add(5 * time.Minute)}}}

	source, err := NewCachingTokenSource(svids.fetch, exchanger)
	require.NoError(t, err)
	source.now = func() time.Time { return now }

	token, err := source.Token(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "dsx-a", token)
	assert.Equal(t, []string{ControllerAudience}, svids.audiences)
	assert.Equal(t, []string{"svid-a"}, exchanger.subjects)
}

func TestToken_ReusesACachedTokenUntilItNearsExpiry(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	svids := &stubSVIDSource{svid: "svid-a"}
	exchanger := &stubExchanger{tokens: []Token{
		{Value: "dsx-a", Expiry: start.Add(5 * time.Minute)},
		{Value: "dsx-b", Expiry: start.Add(10 * time.Minute)},
	}}

	source, err := NewCachingTokenSource(svids.fetch, exchanger)
	require.NoError(t, err)

	now := start
	source.now = func() time.Time { return now }

	first, err := source.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "dsx-a", first)
	assert.Equal(t, 1, exchanger.callCount())

	// Well inside the validity window: served from cache.
	now = start.Add(4 * time.Minute)
	cached, err := source.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "dsx-a", cached)
	assert.Equal(t, 1, exchanger.callCount())

	// Inside DefaultRefreshBefore of expiry: refreshed ahead of the boundary.
	now = start.Add(5*time.Minute - DefaultRefreshBefore)
	refreshed, err := source.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "dsx-b", refreshed)
	assert.Equal(t, 2, exchanger.callCount())
	assert.Equal(t, 2, svids.callCount())
}

func TestToken_FailsClosedAndKeepsTheCacheWhenRefreshFails(t *testing.T) {
	t.Parallel()

	type parameters struct {
		svidErr      error
		exchangeErr  error
		tokens       []Token
		wantErr      string
		wantSVIDCall int
	}
	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	tests := map[string]parameters{
		"workload API unavailable": {
			svidErr:      errors.New("no such file or directory"),
			wantErr:      "fetching jwt-svid for audience " + ControllerAudience,
			wantSVIDCall: 1,
		},
		"exchange unavailable": {
			exchangeErr:  errors.New("connection refused"),
			wantErr:      "exchanging jwt-svid for a dsx identity token",
			wantSVIDCall: 1,
		},
		"exchange returns an empty token": {
			tokens:       []Token{{Value: "", Expiry: now.Add(time.Minute)}},
			wantErr:      "empty dsx identity token",
			wantSVIDCall: 1,
		},
		"exchange returns an expired token": {
			tokens:       []Token{{Value: "dsx-a", Expiry: now.Add(-time.Second)}},
			wantErr:      "token too close to expiry at",
			wantSVIDCall: 1,
		},
		"exchange returns a token inside the refresh window": {
			tokens:       []Token{{Value: "dsx-a", Expiry: now.Add(DefaultRefreshBefore / 2)}},
			wantErr:      "token too close to expiry at",
			wantSVIDCall: 1,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			svids := &stubSVIDSource{svid: "svid-a", err: test.svidErr}
			exchanger := &stubExchanger{tokens: test.tokens, err: test.exchangeErr}

			source, err := NewCachingTokenSource(svids.fetch, exchanger)
			require.NoError(t, err)
			source.now = func() time.Time { return now }

			token, err := source.Token(context.Background())
			assert.Empty(t, token)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
			assert.Equal(t, test.wantSVIDCall, svids.callCount())
			// Nothing usable was cached, so the next call retries rather than
			// serving a token the controller would reject.
			assert.Empty(t, source.cached.Value)
		})
	}
}

func TestToken_KeepsServingAfterARefreshFailure(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	svids := &stubSVIDSource{svid: "svid-a"}
	exchanger := &stubExchanger{tokens: []Token{{Value: "dsx-a", Expiry: start.Add(5 * time.Minute)}}}

	source, err := NewCachingTokenSource(svids.fetch, exchanger)
	require.NoError(t, err)
	now := start
	source.now = func() time.Time { return now }

	first, err := source.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "dsx-a", first)

	// A refresh that fails inside the window must not discard a token that is
	// still valid for the controller.
	exchanger.err = errors.New("connection refused")
	now = start.Add(5*time.Minute - DefaultRefreshBefore)
	_, err = source.Token(context.Background())
	require.Error(t, err)

	exchanger.err = nil
	now = start.Add(time.Minute)
	cached, err := source.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "dsx-a", cached)
}

func TestToken_ExchangesOnceForConcurrentCallers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	svids := &stubSVIDSource{svid: "svid-a"}
	exchanger := &stubExchanger{tokens: []Token{{Value: "dsx-a", Expiry: now.Add(5 * time.Minute)}}}

	source, err := NewCachingTokenSource(svids.fetch, exchanger)
	require.NoError(t, err)
	source.now = func() time.Time { return now }

	const callers = 16
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			token, tokenErr := source.Token(context.Background())
			assert.NoError(t, tokenErr)
			assert.Equal(t, "dsx-a", token)
		}()
	}
	group.Wait()

	// A burst of connections must not turn into a burst of exchanges.
	assert.Equal(t, 1, exchanger.callCount())
}
