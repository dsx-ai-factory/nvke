// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authz"
)

// stubBackend answers every query with one fixed Decision and error, and
// counts the calls.
type stubBackend struct {
	decision authz.Decision
	err      error
	readyErr error
	calls    atomic.Int32
}

func (s *stubBackend) Authorize(context.Context, authz.Query) (authz.Decision, error) {
	s.calls.Add(1)
	return s.decision, s.err
}

func (s *stubBackend) Ready(context.Context) error { return s.readyErr }
func (*stubBackend) Name() string                  { return "stub" }

func TestNewClientRefusesANilBackend(t *testing.T) {
	t.Parallel()

	_, err := authz.NewClient(nil)
	require.ErrorContains(t, err, "backend is nil")

	_, err = authz.NewClient((*stubBackend)(nil))
	assert.ErrorContains(t, err, "backend is a nil *authz_test.stubBackend")
}

func TestClientAuthorize(t *testing.T) {
	t.Parallel()

	backendErr := errors.New("policy store unreachable")
	decision := func(o authz.Outcome) authz.Decision {
		return authz.Decision{Outcome: o, Caller: authz.Caller{Tenant: "acme"}, Reasons: []string{"p1"}}
	}
	sentinels := []error{authz.ErrDenied, authz.ErrNotFound, authz.ErrNoTenant, authz.ErrInvalidQuery}
	tests := []struct {
		name        string
		decision    authz.Decision
		err         error
		wantErr     error // nil for an allow
		wantOutcome authz.Outcome
	}{
		{name: "allow", decision: decision(authz.OutcomeAllow), wantOutcome: authz.OutcomeAllow},
		{name: "deny", decision: decision(authz.OutcomeDeny), wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeDeny},
		{name: "not found", decision: decision(authz.OutcomeNotFound), wantErr: authz.ErrNotFound, wantOutcome: authz.OutcomeNotFound},
		{name: "no tenant", decision: decision(authz.OutcomeNoTenant), wantErr: authz.ErrNoTenant, wantOutcome: authz.OutcomeNoTenant},
		{
			name:        "a backend error with an allow",
			decision:    decision(authz.OutcomeAllow),
			err:         backendErr,
			wantErr:     authz.ErrDenied,
			wantOutcome: authz.OutcomeDeny,
		},
		{
			name:        "a backend error wrapping ErrNotFound",
			decision:    decision(authz.OutcomeAllow),
			err:         fmt.Errorf("lookup: %w", authz.ErrNotFound),
			wantErr:     authz.ErrDenied,
			wantOutcome: authz.OutcomeDeny,
		},
		{
			name:        "a backend error wrapping ErrInvalidQuery",
			decision:    decision(authz.OutcomeAllow),
			err:         fmt.Errorf("lookup: %w", authz.ErrInvalidQuery),
			wantErr:     authz.ErrDenied,
			wantOutcome: authz.OutcomeDeny,
		},
		{name: "an unknown outcome", decision: decision("permit"), wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeDeny},
		{name: "an empty outcome", decision: authz.Decision{}, wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeDeny},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, err := authz.NewClient(&stubBackend{decision: tt.decision, err: tt.err})
			require.NoError(t, err)
			d, err := client.Authorize(t.Context(), validQuery())
			assert.Equal(t, tt.wantOutcome, d.Outcome)
			assert.Equal(t, tt.decision.Caller, d.Caller, "the backend's Caller reaches the API")
			assert.Equal(t, tt.decision.Reasons, d.Reasons, "the backend's Reasons reach the logs")
			if tt.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			for _, sentinel := range sentinels {
				assert.Equal(t, errors.Is(tt.wantErr, sentinel), errors.Is(err, sentinel), "a refusal wraps exactly one sentinel: %v", sentinel)
			}
			if errors.Is(tt.err, backendErr) {
				assert.ErrorIs(t, err, backendErr, "the backend's error stays in the chain, for logs")
			}
		})
	}
}

// The API records Caller.Tenant as a create's owner and filters a list by it,
// so an allow on a tenant's resource must carry a valid caller tenant.
func TestClientChecksTheCallerTenantOnAnAllow(t *testing.T) {
	t.Parallel()

	global := validQuery()
	global.Resource = authz.Resource{Type: "Release", ID: "r1", Scope: authz.ScopeGlobal}
	tests := []struct {
		name    string
		query   authz.Query
		caller  authz.Caller
		wantErr error
	}{
		{name: "an object, with the caller's tenant", query: validQuery(), caller: authz.Caller{Tenant: "acme"}},
		{name: "an object, with no caller tenant", query: validQuery(), wantErr: authz.ErrDenied},
		{name: "an object, with a malformed caller tenant", query: validQuery(), caller: authz.Caller{Tenant: "acme\n"}, wantErr: authz.ErrDenied},
		{name: "a global resource, with no caller tenant", query: global},
		{name: "a global resource, with a malformed caller tenant", query: global, caller: authz.Caller{Tenant: "acme\n"}, wantErr: authz.ErrDenied},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, err := authz.NewClient(&stubBackend{decision: authz.Decision{Outcome: authz.OutcomeAllow, Caller: tt.caller}})
			require.NoError(t, err)
			d, err := client.Authorize(t.Context(), tt.query)
			if tt.wantErr == nil {
				require.NoError(t, err)
				assert.Equal(t, authz.OutcomeAllow, d.Outcome)
				return
			}
			require.ErrorIs(t, err, tt.wantErr)
			assert.Equal(t, authz.OutcomeDeny, d.Outcome)
		})
	}
}

func TestClientRefusesAnInvalidQuery(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{decision: authz.Decision{Outcome: authz.OutcomeAllow}}
	client, err := authz.NewClient(backend)
	require.NoError(t, err)

	q := validQuery()
	q.Resource.ID = ""
	d, err := client.Authorize(t.Context(), q)
	require.ErrorIs(t, err, authz.ErrDenied)
	require.ErrorIs(t, err, authz.ErrInvalidQuery)
	assert.Equal(t, authz.OutcomeDeny, d.Outcome)
	assert.Zero(t, backend.calls.Load(), "the backend was asked")
}

func TestClientWithNoBackendRefuses(t *testing.T) {
	t.Parallel()

	for name, client := range map[string]*authz.Client{"a nil client": nil, "an empty client": {}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			d, err := client.Authorize(t.Context(), validQuery())
			require.ErrorIs(t, err, authz.ErrDenied)
			assert.Equal(t, authz.OutcomeDeny, d.Outcome)
			assert.ErrorContains(t, client.Ready(t.Context()), "client has no backend")
		})
	}
}

func TestClientReady(t *testing.T) {
	t.Parallel()

	client, err := authz.NewClient(&stubBackend{readyErr: errors.New("no policy loaded")})
	require.NoError(t, err)
	assert.ErrorContains(t, client.Ready(t.Context()), "stub: no policy loaded")
}
