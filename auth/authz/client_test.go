// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// modes holds each mode, for tests that run in both.
var modes = []authn.Mode{authn.ModeEnforcing, authn.ModePermissive}

// newClient returns a Client over backend in mode, and what it counts.
func newClient(t *testing.T, backend authz.Authorizer, mode authn.Mode) (*authz.Client, *decisions) {
	t.Helper()
	var counted decisions
	client, err := authz.NewClient(backend, mode, slog.New(slog.DiscardHandler), counted.record)
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

	_, err := authz.NewClient(nil, authn.ModeEnforcing, nil, nil)
	require.ErrorContains(t, err, "backend is nil")

	_, err = authz.NewClient((*stubBackend)(nil), authn.ModeEnforcing, nil, nil)
	assert.ErrorContains(t, err, "backend is a nil *authz_test.stubBackend")
}

// TestClientCheck checks Check's answer, counted outcome and caller for each
// kind of caller and target, in each mode. Both modes count the same outcome;
// only the error and the context differ.
func TestClientCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		backend     authz.Authorizer // the fakeBackend if nil
		principal   *authn.Principal
		target      authz.Target
		wantErr     error // nil for an allow
		wantOutcome authz.Outcome

		// wantCaller is the Caller on the context: on an allow, and on a
		// permissive refusal with a valid Caller. nil for none.
		wantCaller *authz.Caller
	}{
		{
			name: "no principal", principal: nil,
			target:  authz.Target{Action: allowedAction, Resource: cluster("acme")},
			wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeUnauthenticated,
		},
		{
			name: "allow", principal: inAcme,
			target:      authz.Target{Action: allowedAction, Resource: cluster("acme")},
			wantOutcome: authz.OutcomeAllow, wantCaller: &authz.Caller{Tenant: "acme"},
		},
		{
			name: "deny", principal: inAcme,
			target:  authz.Target{Action: deniedAction, Resource: cluster("acme")},
			wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeDeny, wantCaller: &authz.Caller{Tenant: "acme"},
		},
		{
			name: "another tenant's object", principal: inAcme,
			target:  authz.Target{Action: allowedAction, Resource: cluster("globex")},
			wantErr: authz.ErrNotFound, wantOutcome: authz.OutcomeNotFound, wantCaller: &authz.Caller{Tenant: "acme"},
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
			wantErr: authz.ErrDenied, wantOutcome: authz.OutcomeUnauthenticated,
		},
		{
			name: "an operator on a missing object", principal: operator,
			target:  authz.Target{Action: allowedAction, Resource: cluster(""), Missing: true},
			wantErr: authz.ErrNotFound, wantOutcome: authz.OutcomeNotFound,
		},
		{
			name: "an operator on another tenant's object", principal: operator,
			target:      authz.Target{Action: allowedAction, Resource: cluster("acme")},
			wantOutcome: authz.OutcomeAllow, wantCaller: &authz.Caller{Tenant: "nvidia-ops", Operator: true},
		},
	}
	for _, tt := range tests {
		for _, mode := range modes {
			t.Run(tt.name+", "+string(mode), func(t *testing.T) {
				t.Parallel()

				backend := tt.backend
				if backend == nil {
					backend = &fakeBackend{}
				}
				client, counted := newClient(t, backend, mode)
				ctx, err := check(client, tt.principal, tt.target)
				assert.Equal(t, []string{string(tt.wantOutcome) + " " + tt.target.Action}, counted.all())

				got, ok := authz.CallerFromContext(ctx)
				if tt.wantErr == nil || mode == authn.ModePermissive {
					require.NoError(t, err)
					assert.Equal(t, tt.wantCaller != nil, ok, "the caller on the context")
					if tt.wantCaller != nil {
						assert.Equal(t, *tt.wantCaller, got)
					}
					return
				}
				require.ErrorIs(t, err, tt.wantErr)
				assert.False(t, ok, "a refusal puts no caller on the context")
				// A refusal is the bare sentinel; failing closed wraps ErrDenied.
				if tt.wantOutcome == authz.OutcomeError || tt.wantOutcome == authz.OutcomeUnauthenticated {
					assert.NotSame(t, tt.wantErr, err)
				} else {
					assert.Same(t, tt.wantErr, err)
				}
			})
		}
	}
}

// TestClientCheckListTenant checks the tenant a list is filtered to after
// Check: every tenant for any request a permissive Client lets through, and
// the caller's tenant after an enforcing allow.
func TestClientCheckListTenant(t *testing.T) {
	t.Parallel()

	list := authz.Resource{Type: "Collection", ID: "clusters", Scope: authz.ScopeList}
	tests := []struct {
		name       string
		mode       authn.Mode
		principal  *authn.Principal
		action     string
		wantTenant string
		wantAll    bool
		wantErr    error
	}{
		{name: "enforcing, allowed", mode: authn.ModeEnforcing, principal: inAcme, action: allowedAction, wantTenant: "acme"},
		{name: "enforcing, refused", mode: authn.ModeEnforcing, principal: inAcme, action: deniedAction, wantErr: authz.ErrNoCaller},
		{name: "permissive, allowed", mode: authn.ModePermissive, principal: inAcme, action: allowedAction, wantAll: true},
		{name: "permissive, refused", mode: authn.ModePermissive, principal: inAcme, action: deniedAction, wantAll: true},
		{name: "permissive, no principal", mode: authn.ModePermissive, action: allowedAction, wantAll: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, _ := newClient(t, &fakeBackend{}, tt.mode)
			ctx, _ := check(client, tt.principal, authz.Target{Action: tt.action, Resource: list})
			tenant, all, err := authz.ListTenant(ctx)
			require.ErrorIs(t, err, tt.wantErr)
			assert.Equal(t, tt.wantTenant, tenant)
			assert.Equal(t, tt.wantAll, all)
		})
	}
}

// TestClientCheckHidesAnEarlierPermissiveMark checks that an enforcing allow
// lists only the caller's tenant, even on a context a permissive Check passed.
func TestClientCheckHidesAnEarlierPermissiveMark(t *testing.T) {
	t.Parallel()

	permissive, _ := newClient(t, &fakeBackend{}, authn.ModePermissive)
	enforcing, _ := newClient(t, &fakeBackend{}, authn.ModeEnforcing)
	list := authz.Target{Action: allowedAction, Resource: authz.Resource{Type: "Collection", ID: "clusters", Scope: authz.ScopeList}}

	ctx, err := check(permissive, inAcme, list)
	require.NoError(t, err)
	ctx, err = enforcing.Check(ctx, list)
	require.NoError(t, err)
	tenant, all, err := authz.ListTenant(ctx)
	require.NoError(t, err)
	assert.Equal(t, "acme", tenant)
	assert.False(t, all)
}

// TestNewClientEnforcesUnlessPermissive checks that every mode but
// authn.ModePermissive enforces.
func TestNewClientEnforcesUnlessPermissive(t *testing.T) {
	t.Parallel()

	for _, mode := range []authn.Mode{"", authn.Mode("Permissive")} {
		client, _ := newClient(t, &fakeBackend{}, mode)
		_, err := check(client, inAcme, authz.Target{Action: deniedAction, Resource: cluster("acme")})
		assert.ErrorIs(t, err, authz.ErrDenied, "mode %q", mode)
	}
}

// TestClientCheckLogs checks the level and message Check logs a refusal or a
// missing principal with, in each mode.
func TestClientCheckLogs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mode      authn.Mode
		principal *authn.Principal
		want      string
	}{
		{name: "a refusal, enforcing", mode: authn.ModeEnforcing, principal: inAcme, want: `level=INFO msg="refused request"`},
		{name: "no principal, enforcing", mode: authn.ModeEnforcing, principal: nil, want: `level=ERROR msg="failed to authorize request"`},
		{name: "a refusal, permissive", mode: authn.ModePermissive, principal: inAcme, want: `level=WARN msg="authorization would have refused this request"`},
		{name: "no principal, permissive", mode: authn.ModePermissive, principal: nil, want: `level=WARN msg="authorization would have refused this request"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer
			client, err := authz.NewClient(&fakeBackend{}, tt.mode, slog.New(slog.NewTextHandler(&logs, nil)), nil)
			require.NoError(t, err)
			_, _ = check(client, tt.principal, authz.Target{Action: deniedAction, Resource: cluster("acme")})
			assert.Contains(t, logs.String(), tt.want)
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

			client, counted := newClient(t, unreachable{t}, authn.ModeEnforcing)
			// A valid query, so that only Check keeps it from the backend.
			_, err := check(client, p, authz.Target{Action: allowedAction, Resource: cluster("acme"), Missing: true})
			assert.Same(t, authz.ErrNotFound, err)
			assert.Equal(t, []string{"not_found " + allowedAction}, counted.all())
		})
	}
}

// refusalRecord runs one refused Check as p with a JSON logger, and returns
// the one record it wrote.
func refusalRecord(t *testing.T, backend authz.Authorizer, p *authn.Principal, target authz.Target) map[string]any {
	t.Helper()
	var log bytes.Buffer
	client, err := authz.NewClient(backend, authn.ModeEnforcing, slog.New(slog.NewJSONHandler(&log, nil)), nil)
	require.NoError(t, err)
	_, err = check(client, p, target)
	require.Error(t, err)

	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	require.Len(t, lines, 1, "a refused Check writes one record")
	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &record))
	return record
}

// TestClientCheckLogsTheCallerAndTheResource checks that every record Check
// writes names the resource's type and ID, and the caller's subject when there
// is one.
func TestClientCheckLogsTheCallerAndTheResource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		backend    authz.Authorizer // the fakeBackend if nil
		principal  *authn.Principal
		target     authz.Target
		wantTenant string // "" skips the tenant check
	}{
		{
			name: "deny", principal: inAcme,
			target: authz.Target{Action: deniedAction, Resource: cluster("acme")}, wantTenant: "acme",
		},
		{
			name: "another tenant's object", principal: inAcme,
			target: authz.Target{Action: allowedAction, Resource: cluster("globex")}, wantTenant: "acme",
		},
		{
			name: "no tenant", principal: noTenant,
			target: authz.Target{Action: allowedAction, Resource: cluster("acme")},
		},
		{
			name: "an invalid query", principal: inAcme,
			target: authz.Target{Action: "", Resource: cluster("acme")},
		},
		{
			name: "a backend error", backend: &stubBackend{err: errors.New("policy store unreachable")}, principal: inAcme,
			target: authz.Target{Action: allowedAction, Resource: cluster("acme")},
		},
		{
			name:      "a missing object",
			principal: inAcme,
			target:    authz.Target{Action: allowedAction, Resource: cluster(""), Missing: true},
		},
		{
			name:   "no principal",
			target: authz.Target{Action: allowedAction, Resource: cluster("acme")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backend := tt.backend
			if backend == nil {
				backend = &fakeBackend{}
			}
			record := refusalRecord(t, backend, tt.principal, tt.target)
			if tt.principal == nil {
				assert.NotContains(t, record, "subject")
			} else {
				assert.Equal(t, tt.principal.IssuerName, record["issuer"])
				assert.Equal(t, tt.principal.Subject, record["subject"])
			}
			assert.Equal(t, "Cluster", record["resource_type"])
			assert.Equal(t, "c-1", record["resource_id"])
			if tt.wantTenant != "" {
				assert.Equal(t, tt.wantTenant, record["tenant"])
			}
		})
	}
}

// TestClientCheckDoesNotLogAnInvalidResourceID checks that a refusal log
// replaces an ID that Query.Validate refuses, on every path that logs it.
func TestClientCheckDoesNotLogAnInvalidResourceID(t *testing.T) {
	t.Parallel()

	for idName, id := range map[string]string{"oversized": strings.Repeat("x", 4096), "empty": ""} {
		resource := cluster("acme")
		resource.ID = id
		for name, tt := range map[string]struct {
			principal *authn.Principal
			target    authz.Target
		}{
			"backend":        {inAcme, authz.Target{Action: allowedAction, Resource: resource}},
			"missing object": {inAcme, authz.Target{Action: allowedAction, Resource: resource, Missing: true}},
			"no principal":   {nil, authz.Target{Action: allowedAction, Resource: resource}},
		} {
			t.Run(idName+", "+name, func(t *testing.T) {
				t.Parallel()
				record := refusalRecord(t, &fakeBackend{}, tt.principal, tt.target)
				assert.Equal(t, "<invalid>", record["resource_id"])
			})
		}
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

	client, err := authz.NewClient(&stubBackend{readyErr: errors.New("no policy loaded")}, authn.ModeEnforcing, nil, nil)
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

			client, err := authz.NewClient(&stubBackend{decision: tt.decision, err: tt.err}, authn.ModeEnforcing, nil, nil)
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

			client, err := authz.NewClient(&stubBackend{decision: authz.Decision{Outcome: authz.OutcomeAllow, Caller: tt.caller}}, authn.ModeEnforcing, nil, nil)
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
	client, err := authz.NewClient(backend, authn.ModeEnforcing, nil, nil)
	require.NoError(t, err)

	q := validQuery()
	q.Resource.ID = ""
	d, err := authz.Authorize(client, t.Context(), q)
	require.ErrorIs(t, err, authz.ErrDenied)
	require.ErrorIs(t, err, authz.ErrInvalidQuery)
	assert.Equal(t, authz.OutcomeDeny, d.Outcome)
	assert.Zero(t, backend.calls.Load(), "the backend was asked")
}
