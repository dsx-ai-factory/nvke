// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package authztest holds the conformance suite every authz backend must pass.
// It proves a backend applies the isolation check.
package authztest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
)

// Fixture is a backend set up for the suite. Its policy must allow
// AllowedAction and deny DeniedAction, to every caller on every resource, so
// the suite can tell the isolation check from policy.
type Fixture struct {
	Authorizer authz.Authorizer

	AllowedAction string
	DeniedAction  string

	// TenantA and TenantB are two different valid tenant IDs.
	TenantA string
	TenantB string

	// InTenantA and InTenantB are callers in those tenants, neither an
	// operator. The suite runs every tenant case from both, so a backend
	// can't isolate one tenant and not the other.
	InTenantA authn.Principal
	InTenantB authn.Principal

	// SameSubjectInB has InTenantA's issuer and subject, with a token for
	// TenantB. A backend that caches decisions must key them by the caller's
	// tenant, not only by subject.
	SameSubjectInB authn.Principal

	// Operator is a caller who may act in any tenant. Its own tenant is
	// neither TenantA nor TenantB, so an allow on TenantB's object comes from
	// Caller.Operator and not from a tenant match.
	Operator authn.Principal

	// NoTenant is a caller with no tenant.
	NoTenant authn.Principal

	// Impostor claims to be an operator from a source the backend doesn't
	// trust for operators. The backend refuses it with an outcome. A backend
	// with no such caller sets NoImpostor to say why instead.
	Impostor   *authn.Principal
	NoImpostor string

	// OperatorNoTenant comes from a source the backend trusts for operators,
	// with no tenant. A backend with no such caller sets NoOperatorNoTenant
	// to say why instead.
	OperatorNoTenant   *authn.Principal
	NoOperatorNoTenant string
}

const (
	// refused accepts any refusal outcome other than allow.
	refused authz.Outcome = "any refusal"
)

// role says which Decision.Caller an allow must report.
type role int

const (
	inA role = iota
	inB
	operator
	noTenant
	other
)

type testCase struct {
	name     string
	caller   authn.Principal
	role     role
	action   string
	resource authz.Resource
	want     authz.Outcome
}

// Run asserts that f.Authorizer applies the isolation check and reports the
// caller's tenancy on an allow. Most cases run in parallel, after Run returns,
// so release the backend with t.Cleanup, not defer. Under -race the suite
// also checks the backend is safe for concurrent use.
func Run(t *testing.T, f Fixture) {
	t.Helper()
	checkFixture(t, f)
	require.NoError(t, f.Authorizer.Ready(t.Context()), "Ready")
	require.NotEmpty(t, f.Authorizer.Name(), "Name")

	// These run in order, so a backend that caches decisions answers the
	// later questions from its cache.
	t.Run("a cached answer stays with its tenants", func(t *testing.T) {
		a, b := f.TenantA, f.TenantB
		for _, tc := range []testCase{
			{caller: f.InTenantA, role: inA, resource: object("Cluster", "shared", a), want: authz.OutcomeAllow},
			{caller: f.InTenantA, role: inA, resource: object("Cluster", "shared", b), want: authz.OutcomeNotFound},
			{caller: f.SameSubjectInB, role: inB, resource: object("Cluster", "shared", a), want: authz.OutcomeNotFound},
			{caller: f.SameSubjectInB, role: inB, resource: object("Cluster", "shared", b), want: authz.OutcomeAllow},
			{caller: f.InTenantA, role: inA, resource: create(a), want: authz.OutcomeAllow},
			{caller: f.InTenantA, role: inA, resource: create(b), want: authz.OutcomeNotFound},
			{caller: f.InTenantA, role: inA, resource: create(""), want: authz.OutcomeAllow},
			{caller: f.SameSubjectInB, role: inB, resource: create(a), want: authz.OutcomeNotFound},
			{caller: f.NoTenant, role: noTenant, resource: collection(authz.ScopeGlobal), want: authz.OutcomeAllow},
			{caller: f.NoTenant, role: noTenant, resource: collection(authz.ScopeList), want: authz.OutcomeNoTenant},
		} {
			tc.action = f.AllowedAction
			check(t, f, tc)
		}
	})

	for _, tc := range cases(f) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check(t, f, tc)
		})
	}
}

func checkFixture(t *testing.T, f Fixture) {
	t.Helper()
	require.NotNil(t, f.Authorizer, "Fixture.Authorizer")
	require.NotEmpty(t, f.AllowedAction, "Fixture.AllowedAction")
	require.NotEmpty(t, f.DeniedAction, "Fixture.DeniedAction")
	require.NotEqual(t, f.TenantA, f.TenantB, "Fixture.TenantA and Fixture.TenantB must differ")
	for _, id := range []string{f.TenantA, f.TenantB} {
		require.NoError(t, authz.CheckTenantID(id), "tenant %q", id)
	}
	require.Equal(t, f.InTenantA.IssuerName, f.SameSubjectInB.IssuerName, "Fixture.SameSubjectInB's issuer")
	require.Equal(t, f.InTenantA.Subject, f.SameSubjectInB.Subject, "Fixture.SameSubjectInB's subject")
	require.NotEqual(t, f.Impostor == nil, f.NoImpostor == "",
		"set exactly one of Fixture.Impostor and Fixture.NoImpostor")
	require.NotEqual(t, f.OperatorNoTenant == nil, f.NoOperatorNoTenant == "",
		"set exactly one of Fixture.OperatorNoTenant and Fixture.NoOperatorNoTenant")
}

func object(typ, id, tenant string) authz.Resource {
	return authz.Resource{Type: typ, ID: id, Scope: authz.ScopeObject, Tenant: tenant}
}

func create(tenant string) authz.Resource {
	return authz.Resource{Type: "Collection", ID: "clusters", Scope: authz.ScopeCreate, Tenant: tenant}
}

func collection(scope authz.Scope) authz.Resource {
	return authz.Resource{Type: "Collection", ID: "clusters", Scope: scope}
}

func cases(f Fixture) []testCase {
	allow, deny := f.AllowedAction, f.DeniedAction
	var cs []testCase
	for _, side := range []struct {
		name       string
		caller     authn.Principal
		role       role
		own, other string
	}{
		{name: "tenant A", caller: f.InTenantA, role: inA, own: f.TenantA, other: f.TenantB},
		{name: "tenant B", caller: f.InTenantB, role: inB, own: f.TenantB, other: f.TenantA},
	} {
		own, otherTenant := side.own, side.other
		sideCases := []testCase{
			{name: "same tenant", action: allow, resource: object("Cluster", "o1", own), want: authz.OutcomeAllow},
			{name: "same tenant, denied action", action: deny, resource: object("Cluster", "o1", own), want: authz.OutcomeDeny},
			{name: "cross tenant", action: allow, resource: object("Cluster", "x1", otherTenant), want: authz.OutcomeNotFound},
			{name: "cross tenant, another type", action: allow, resource: object("NodePool", "x1", otherTenant), want: authz.OutcomeNotFound},
			{name: "cross tenant, denied action", action: deny, resource: object("Cluster", "x1", otherTenant), want: authz.OutcomeNotFound},
			{name: "create naming its own tenant", action: allow, resource: create(own), want: authz.OutcomeAllow},
			{name: "create naming no tenant", action: allow, resource: create(""), want: authz.OutcomeAllow},
			{name: "create in another tenant", action: allow, resource: create(otherTenant), want: authz.OutcomeNotFound},
			{name: "a list", action: allow, resource: collection(authz.ScopeList), want: authz.OutcomeAllow},
		}
		// Tenant IDs compare byte for byte: no prefix, suffix or case match.
		for _, near := range []string{own + "x", "x" + own, strings.ToUpper(own), strings.ToLower(own), own[:len(own)-1]} {
			if near == own || authz.CheckTenantID(near) != nil {
				continue
			}
			sideCases = append(sideCases,
				testCase{name: "an object in " + near, action: allow, resource: object("Cluster", "n1", near), want: authz.OutcomeNotFound},
				testCase{name: "create in " + near, action: allow, resource: create(near), want: authz.OutcomeNotFound},
			)
		}
		for _, tc := range sideCases {
			tc.name = side.name + ", " + tc.name
			tc.caller, tc.role = side.caller, side.role
			cs = append(cs, tc)
		}
	}

	b := f.TenantB
	cs = append(cs,
		testCase{name: "operator, cross tenant", caller: f.Operator, role: operator, action: allow, resource: object("Cluster", "b1", b), want: authz.OutcomeAllow},
		testCase{name: "operator, denied action", caller: f.Operator, role: operator, action: deny, resource: object("Cluster", "b1", b), want: authz.OutcomeDeny},
		testCase{name: "operator, create in another tenant", caller: f.Operator, role: operator, action: allow, resource: create(b), want: authz.OutcomeAllow},
		testCase{name: "operator, create naming no tenant", caller: f.Operator, role: operator, action: allow, resource: create(""), want: authz.OutcomeAllow},
		testCase{name: "operator, a list", caller: f.Operator, role: operator, action: allow, resource: collection(authz.ScopeList), want: authz.OutcomeAllow},

		testCase{name: "no tenant, an object", caller: f.NoTenant, role: noTenant, action: allow, resource: object("Cluster", "a1", f.TenantA), want: authz.OutcomeNoTenant},
		testCase{name: "no tenant, denied action", caller: f.NoTenant, role: noTenant, action: deny, resource: object("Cluster", "a1", f.TenantA), want: authz.OutcomeNoTenant},
		testCase{name: "no tenant, create", caller: f.NoTenant, role: noTenant, action: allow, resource: create(""), want: authz.OutcomeNoTenant},
		testCase{name: "no tenant, a list", caller: f.NoTenant, role: noTenant, action: allow, resource: collection(authz.ScopeList), want: authz.OutcomeNoTenant},
		testCase{name: "no tenant, a global resource", caller: f.NoTenant, role: noTenant, action: allow, resource: collection(authz.ScopeGlobal), want: authz.OutcomeAllow},
	)
	for _, refusedCaller := range []struct {
		name string
		p    *authn.Principal
	}{
		{name: "impostor", p: f.Impostor},
		{name: "operator with no tenant", p: f.OperatorNoTenant},
	} {
		if refusedCaller.p == nil {
			continue
		}
		p := *refusedCaller.p
		cs = append(cs,
			testCase{name: refusedCaller.name + ", cross tenant", caller: p, role: other, action: allow, resource: object("Cluster", "b1", b), want: refused},
			testCase{name: refusedCaller.name + ", create", caller: p, role: other, action: allow, resource: create(""), want: refused},
			testCase{name: refusedCaller.name + ", a list", caller: p, role: other, action: allow, resource: collection(authz.ScopeList), want: refused},
		)
	}
	return cs
}

func check(t *testing.T, f Fixture, tc testCase) {
	t.Helper()
	q := authz.Query{Principal: tc.caller, Action: tc.action, Resource: tc.resource}
	require.NoError(t, q.Validate(), "the suite built an invalid query")
	d, err := f.Authorizer.Authorize(t.Context(), q)

	require.NoError(t, err, "a refusal is an outcome, not an error")
	if tc.want == refused {
		assert.NotEqual(t, authz.OutcomeAllow, d.Outcome)
		return
	}
	assert.Equal(t, tc.want, d.Outcome)
	if d.Outcome == authz.OutcomeAllow {
		checkCaller(t, f, tc.role, d.Caller)
	}
}

// checkCaller asserts the tenancy an allow reports, which the API uses to
// filter a list and record a create's owner.
func checkCaller(t *testing.T, f Fixture, r role, got authz.Caller) {
	t.Helper()
	switch r {
	case inA:
		assert.Equal(t, authz.Caller{Tenant: f.TenantA}, got)
	case inB:
		assert.Equal(t, authz.Caller{Tenant: f.TenantB}, got)
	case operator:
		assert.True(t, got.Operator, "the operator's Decision.Caller.Operator")
		assert.NotContains(t, []string{"", f.TenantA, f.TenantB}, got.Tenant, "the operator's own tenant")
	case noTenant:
		assert.Equal(t, authz.Caller{}, got)
	case other:
	}
}
