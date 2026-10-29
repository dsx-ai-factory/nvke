// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authztest_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
	"github.com/dsx-ai-factory/nvke/auth/authz/authztest"
)

const (
	operatorTenant = "ops"
	deniedAction   = "deleteEverything"

	// brokenEnv names the broken backend TestBrokenBackend runs the suite on.
	brokenEnv = "AUTHZTEST_BROKEN_BACKEND"
)

// fakeBackend is a minimal backend with a tenant model. The caller's tenant is
// the "tenant" claim. A caller is an operator when its tenant is
// operatorTenant and its issuer is "cluster-sa"; any other issuer naming
// operatorTenant is denied. Its policy denies deniedAction and allows
// everything else.
//
// broken names one way to break it, for TestSuiteCatchesBrokenBackends.
type fakeBackend struct {
	broken string
	cache  sync.Map
}

func (b *fakeBackend) Authorize(_ context.Context, q authz.Query) (authz.Decision, error) {
	tenant, _ := q.Principal.Claims["tenant"].(string)
	trusted := q.Principal.IssuerName == "cluster-sa"
	if tenant == operatorTenant && !trusted && b.broken != "operator from any issuer" {
		return authz.Decision{Outcome: authz.OutcomeDeny}, nil
	}
	caller := authz.Caller{Tenant: tenant, Operator: tenant == operatorTenant}
	if b.broken == "operator needs no tenant" && trusted {
		caller.Operator = true
	}
	if caller.Operator && (b.broken == "operator reports the resource's tenant" ||
		b.broken == "operator reports the created object's tenant" && q.Resource.Scope == authz.ScopeCreate) {
		caller.Tenant = q.Resource.Tenant
	}

	key := b.cacheKey(q)
	if key != "" {
		if d, ok := b.cache.Load(key); ok {
			return d.(authz.Decision), nil
		}
	}
	if b.broken == "policy before the isolation check" && q.Action == deniedAction {
		return authz.Decision{Outcome: authz.OutcomeDeny, Caller: caller}, nil
	}
	d := authz.Decision{Outcome: authz.OutcomeAllow, Caller: caller}
	if refusal := b.isolate(caller, q.Resource); refusal != "" {
		if b.broken == "refusal as an error" {
			return authz.Decision{}, errors.New(string(refusal))
		}
		d.Outcome = refusal
	} else if q.Action == deniedAction {
		d.Outcome = authz.OutcomeDeny
	}
	if key != "" && (d.Outcome == authz.OutcomeAllow || b.broken != "cache of allowed creates keyed by collection") {
		b.cache.Store(key, d)
	}
	return d, nil
}

// cacheKey is the key a broken cache uses, or "" for no cache.
func (b *fakeBackend) cacheKey(q authz.Query) string {
	r := q.Resource
	switch b.broken {
	case "cache keyed without the caller's tenant":
		return strings.Join([]string{q.Principal.Subject, q.Action, r.Type, r.ID, string(r.Scope), r.Tenant}, "|")
	case "cache of allowed creates keyed by collection":
		if r.Scope == authz.ScopeCreate {
			return strings.Join([]string{q.Principal.Subject, q.Action, r.Type, r.ID}, "|")
		}
	}
	return ""
}

// isolate is authz.CheckIsolation, unless a broken mode lets a refusal
// through.
func (b *fakeBackend) isolate(c authz.Caller, r authz.Resource) authz.Outcome {
	refusal := authz.CheckIsolation(c, r)
	if b.broken == "operator needs no tenant" && c.Operator {
		return ""
	}
	cross := refusal == authz.OutcomeNotFound
	letThrough := map[string]bool{
		"prefix match":            cross && strings.HasPrefix(r.Tenant, c.Tenant),
		"isolation for acme only": cross && c.Tenant != "acme",
		"list without a tenant":   refusal == authz.OutcomeNoTenant && r.Scope == authz.ScopeList,
	}
	if letThrough[b.broken] {
		return ""
	}
	return refusal
}

func (*fakeBackend) Ready(context.Context) error { return nil }
func (*fakeBackend) Name() string                { return "fake" }

func caller(issuer, subject string, claims map[string]any) authn.Principal {
	return authn.Principal{IssuerName: issuer, Subject: subject, Claims: claims}
}

func fixture(broken string) authztest.Fixture {
	impostor := caller("keycloak", "mallory", map[string]any{"tenant": operatorTenant})
	operatorNoTenant := caller("cluster-sa", "system:serviceaccount:nvke-ops:no-tenant", nil)
	return authztest.Fixture{
		Authorizer:       &fakeBackend{broken: broken},
		AllowedAction:    "getCluster",
		DeniedAction:     deniedAction,
		TenantA:          "acme",
		TenantB:          "globex",
		InTenantA:        caller("keycloak", "alice", map[string]any{"tenant": "acme"}),
		InTenantB:        caller("keycloak", "carol", map[string]any{"tenant": "globex"}),
		SameSubjectInB:   caller("keycloak", "alice", map[string]any{"tenant": "globex"}),
		Operator:         caller("cluster-sa", "system:serviceaccount:nvke-ops:fleet", map[string]any{"tenant": operatorTenant}),
		NoTenant:         caller("keycloak", "bob", nil),
		Impostor:         &impostor,
		OperatorNoTenant: &operatorNoTenant,
	}
}

func TestFakeBackendConforms(t *testing.T) {
	t.Parallel()

	authztest.Run(t, fixture(""))
}

// TestBrokenBackend runs the suite on the broken backend brokenEnv names. It
// is the child process of TestSuiteCatchesBrokenBackends, and skips otherwise.
func TestBrokenBackend(t *testing.T) {
	broken := os.Getenv(brokenEnv)
	if broken == "" {
		t.Skip("runs only as the child of TestSuiteCatchesBrokenBackends")
	}
	authztest.Run(t, fixture(broken))
}

// TestSuiteCatchesBrokenBackends expects the suite to fail each broken
// backend in one of its cases. Each runs in a child process, since a failing
// suite fails its parent test.
func TestSuiteCatchesBrokenBackends(t *testing.T) {
	t.Parallel()

	for _, broken := range []string{
		"policy before the isolation check",
		"refusal as an error",
		"prefix match",
		"isolation for acme only",
		"list without a tenant",
		"operator from any issuer",
		"operator needs no tenant",
		"operator reports the resource's tenant",
		"operator reports the created object's tenant",
		"cache keyed without the caller's tenant",
		"cache of allowed creates keyed by collection",
	} {
		t.Run(broken, func(t *testing.T) {
			t.Parallel()

			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBrokenBackend$")
			cmd.Env = append(os.Environ(), brokenEnv+"="+broken)
			out, err := cmd.CombinedOutput()
			require.Error(t, err, "the suite passed a backend with %q:\n%s", broken, out)
			assert.Contains(t, string(out), "--- FAIL: TestBrokenBackend/", "no suite case failed:\n%s", out)
		})
	}
}
