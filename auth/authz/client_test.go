// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
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

// Authorize returns the stub's fixed decision and error, and counts the call.
func (s *stubBackend) Authorize(context.Context, authz.Query) (authz.Decision, error) {
	s.calls.Add(1)
	return s.decision, s.err
}

func (s *stubBackend) Ready(context.Context) error { return s.readyErr }
func (*stubBackend) Name() string                  { return "stub" }

const (
	allowedAction = "getCluster"
	deniedAction  = "deleteCluster"
	opsIssuer     = "ops"
)

// fakeBackend reads the caller's tenant from the "tenant" claim, trusts the
// "operator" claim only from opsIssuer, applies the isolation check, and
// allows every action but deniedAction.
type fakeBackend struct{}

// Authorize runs the shared isolation check, then denies deniedAction and
// allows everything else.
func (*fakeBackend) Authorize(_ context.Context, q authz.Query) (authz.Decision, error) {
	tenant, _ := q.Principal.Claims["tenant"].(string)
	operator, _ := q.Principal.Claims["operator"].(bool)
	caller := authz.Caller{Tenant: tenant, Operator: operator && q.Principal.IssuerName == opsIssuer}
	if outcome := authz.CheckIsolation(caller, q.Resource); outcome != "" {
		return authz.Decision{Outcome: outcome, Caller: caller}, nil
	}
	if q.Action == deniedAction {
		return authz.Decision{Outcome: authz.OutcomeDeny, Caller: caller, Reasons: []string{"no-delete"}}, nil
	}
	return authz.Decision{Outcome: authz.OutcomeAllow, Caller: caller, Reasons: []string{"allow-all"}}, nil
}

func (*fakeBackend) Ready(context.Context) error { return nil }
func (*fakeBackend) Name() string                { return "fake" }

// principal returns a principal from issuer with subject, a tenant claim
// unless tenant is empty, and an operator claim.
func principal(issuer, subject, tenant string, operator bool) *authn.Principal {
	claims := map[string]any{"operator": operator}
	if tenant != "" {
		claims["tenant"] = tenant
	}
	return &authn.Principal{IssuerName: issuer, Subject: subject, Claims: claims}
}

var (
	inAcme   = principal("idp", "alice", "acme", false)
	noTenant = principal("idp", "nobody", "", false)
	operator = principal(opsIssuer, "ops-1", "nvidia-ops", true)
)

// cluster returns cluster c-1, owned by tenant.
func cluster(tenant string) authz.Resource {
	return authz.Resource{Type: "Cluster", ID: "c-1", Scope: authz.ScopeObject, Tenant: tenant}
}

// decisions records what a Client counted.
type decisions struct {
	mu   sync.Mutex
	seen []string
}

// record counts one decision. It is the Client's Recorder.
func (d *decisions) record(outcome authz.Outcome, action string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, string(outcome)+" "+action)
}

// all returns every decision counted so far, in order.
func (d *decisions) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// newClient returns a Client over backend, and what it counts.
func newClient(t *testing.T, backend authz.Authorizer) (*authz.Client, *decisions) {
	t.Helper()
	var counted decisions
	client, err := authz.NewClient(backend, slog.New(slog.DiscardHandler), counted.record)
	require.NoError(t, err)
	return client, &counted
}

// check runs one Check as p, nil for no principal.
func check(client *authz.Client, p *authn.Principal, target authz.Target) (context.Context, error) {
	ctx := context.Background()
	if p != nil {
		ctx = authn.ContextWithPrincipal(ctx, p)
	}
	return client.Check(ctx, target)
}

// TestNewClientRefusesANilBackend checks that NewClient refuses a nil backend,
// including a typed nil.
func TestNewClientRefusesANilBackend(t *testing.T) {
	t.Parallel()

	_, err := authz.NewClient(nil, nil, nil)
	require.ErrorContains(t, err, "backend is nil")

	_, err = authz.NewClient((*stubBackend)(nil), nil, nil)
	assert.ErrorContains(t, err, "backend is a nil *authz_test.stubBackend")
}

// TestClientCheck checks Check's answer, counted outcome and caller for each
// kind of caller and target.
func TestClientCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		backend     authz.Authorizer // the fakeBackend if nil
		principal   *authn.Principal
		target      authz.Target
		wantErr     error // nil for an allow
		wantOutcome authz.Outcome
		wantCaller  authz.Caller
	}{
		{
			name: "no principal", principal: nil,
			target:  authz.Target{Action: allowedAction, Resource: cluster("acme")},
			wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeError,
		},
		{
			name: "allow", principal: inAcme,
			target:      authz.Target{Action: allowedAction, Resource: cluster("acme")},
			wantOutcome: authz.OutcomeAllow, wantCaller: authz.Caller{Tenant: "acme"},
		},
		{
			name: "deny", principal: inAcme,
			target:  authz.Target{Action: deniedAction, Resource: cluster("acme")},
			wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeDeny,
		},
		{
			name: "another tenant's object", principal: inAcme,
			target:  authz.Target{Action: allowedAction, Resource: cluster("globex")},
			wantErr: authz.ErrNotFound, wantOutcome: authz.OutcomeNotFound,
		},
		{
			name: "no tenant", principal: noTenant,
			target:  authz.Target{Action: allowedAction, Resource: cluster("acme")},
			wantErr: authz.ErrNoTenant, wantOutcome: authz.OutcomeNoTenant,
		},
		{
			name: "an invalid query", principal: inAcme,
			target:  authz.Target{Action: "", Resource: cluster("acme")},
			wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeError,
		},
		{
			name: "a backend error", backend: &stubBackend{err: errors.New("policy store unreachable")}, principal: inAcme,
			target:  authz.Target{Action: allowedAction, Resource: cluster("acme")},
			wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeError,
		},
		{
			name: "a missing object, no principal", principal: nil,
			target:  authz.Target{Action: allowedAction, Resource: cluster(""), Missing: true},
			wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeError,
		},
		{
			name: "an operator on a missing object", principal: operator,
			target:  authz.Target{Action: allowedAction, Resource: cluster(""), Missing: true},
			wantErr: authz.ErrNotFound, wantOutcome: authz.OutcomeNotFound,
		},
		{
			name: "an operator on another tenant's object", principal: operator,
			target:      authz.Target{Action: allowedAction, Resource: cluster("acme")},
			wantOutcome: authz.OutcomeAllow, wantCaller: authz.Caller{Tenant: "nvidia-ops", Operator: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backend := tt.backend
			if backend == nil {
				backend = &fakeBackend{}
			}
			client, counted := newClient(t, backend)
			ctx, err := check(client, tt.principal, tt.target)
			assert.Equal(t, []string{string(tt.wantOutcome) + " " + tt.target.Action}, counted.all())

			got, ok := authz.CallerFromContext(ctx)
			if tt.wantErr == nil {
				require.NoError(t, err)
				assert.True(t, ok, "an allow puts the caller on the context")
				assert.Equal(t, tt.wantCaller, got)
				return
			}
			require.ErrorIs(t, err, tt.wantErr)
			assert.False(t, ok, "a refusal puts no caller on the context")
			// A refusal is the bare sentinel; failing closed wraps ErrDenied.
			if tt.wantOutcome == authz.OutcomeError {
				assert.NotSame(t, tt.wantErr, err)
			} else {
				assert.Same(t, tt.wantErr, err)
			}
		})
	}
}

// unreachable fails the test if the Client asks it for a decision.
type unreachable struct{ t *testing.T }

// Authorize fails the test: Check must not reach the backend.
func (b unreachable) Authorize(context.Context, authz.Query) (authz.Decision, error) {
	b.t.Error("the backend was asked about a missing object")
	return authz.Decision{}, errors.New("unreachable")
}

func (unreachable) Ready(context.Context) error { return nil }
func (unreachable) Name() string                { return "unreachable" }

// TestClientCheckDoesNotAskAboutAMissingObject checks that a missing object
// gets ErrNotFound without the backend being asked, for every kind of caller.
func TestClientCheckDoesNotAskAboutAMissingObject(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]*authn.Principal{"a tenant caller": inAcme, "no tenant": noTenant, "an operator": operator} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, counted := newClient(t, unreachable{t})
			// A valid query, so that only Check keeps it from the backend.
			_, err := check(client, p, authz.Target{Action: allowedAction, Resource: cluster("acme"), Missing: true})
			assert.Same(t, authz.ErrNotFound, err)
			assert.Equal(t, []string{"not_found " + allowedAction}, counted.all())
		})
	}
}

// TestClientWithNoBackendRefuses checks that a nil or empty Client refuses
// every request instead of panicking.
func TestClientWithNoBackendRefuses(t *testing.T) {
	t.Parallel()

	for name, client := range map[string]*authz.Client{"a nil client": nil, "an empty client": {}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := check(client, inAcme, authz.Target{Action: allowedAction, Resource: cluster("acme")})
			require.ErrorIs(t, err, authz.ErrDenied)
			assert.ErrorContains(t, client.Ready(t.Context()), "client has no backend")
		})
	}
}

// TestClientReady checks that Ready reports the backend's error.
func TestClientReady(t *testing.T) {
	t.Parallel()

	client, err := authz.NewClient(&stubBackend{readyErr: errors.New("no policy loaded")}, nil, nil)
	require.NoError(t, err)
	assert.ErrorContains(t, client.Ready(t.Context()), "stub: no policy loaded")
}

// TestAuthorize checks that authorize allows only on OutcomeAllow, and fails
// closed on a backend error, an unknown outcome or a bad caller tenant.
func TestAuthorize(t *testing.T) {
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
		{name: "the error outcome Check counts", decision: decision(authz.OutcomeError), wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeDeny},
		{name: "an empty outcome", decision: authz.Decision{}, wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeDeny},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, err := authz.NewClient(&stubBackend{decision: tt.decision, err: tt.err}, nil, nil)
			require.NoError(t, err)
			d, err := authz.Authorize(client, t.Context(), validQuery())
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
func TestAuthorizeChecksTheCallerTenantOnAnAllow(t *testing.T) {
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

			client, err := authz.NewClient(&stubBackend{decision: authz.Decision{Outcome: authz.OutcomeAllow, Caller: tt.caller}}, nil, nil)
			require.NoError(t, err)
			d, err := authz.Authorize(client, t.Context(), tt.query)
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

// TestAuthorizeRefusesAnInvalidQuery checks that an invalid query is refused
// before the backend is asked.
func TestAuthorizeRefusesAnInvalidQuery(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{decision: authz.Decision{Outcome: authz.OutcomeAllow}}
	client, err := authz.NewClient(backend, nil, nil)
	require.NoError(t, err)

	q := validQuery()
	q.Resource.ID = ""
	d, err := authz.Authorize(client, t.Context(), q)
	require.ErrorIs(t, err, authz.ErrDenied)
	require.ErrorIs(t, err, authz.ErrInvalidQuery)
	assert.Equal(t, authz.OutcomeDeny, d.Outcome)
	assert.Zero(t, backend.calls.Load(), "the backend was asked")
}
