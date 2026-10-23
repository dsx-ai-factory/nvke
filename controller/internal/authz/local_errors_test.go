// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"net/http"
	"testing"

	"github.com/cedar-policy/cedar-go/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two guarantees that hold the engine to answering only what it can stand
// behind: an Allow reached while a policy errored is not served, and a Deny that
// errored is still a Deny. Both were once asserted against another engine only,
// so deleting either of these guards left the suite green.

// bundleWith writes a throwaway bundle in which aMapping grants subjectReader
// reader, and loads it. Used for policies the shipped bundle deliberately does
// not contain. The generated action entities are real: an action with no group
// would not resolve, and the point here is the evaluation, not the vocabulary.
func bundleWith(t *testing.T, policies string) *Local {
	t.Helper()

	local, err := NewLocal(writeBundle(t, policies, shippedActions(t), oneReader), testIssuers)
	require.NoError(t, err)
	return local
}

// A forbid whose guard errors is skipped, and the permit it existed to override
// wins — so Cedar answers Allow with the error alongside. Serving that is
// serving a request the policy meant to block.
//
// The guard here compares context.issuer, a populated String, with `<` against
// a Long, which is a type error at evaluation rather than at load. It used to
// read a context key nothing sets instead — context.nothingSetsThis — but
// checkPolicyShape now refuses that at NewLocal, before Authorize ever runs
// (see TestABundleWhosePolicyReadsAnUnpopulatedContextKeyIsRefused in
// policy_shape_test.go), so it no longer reaches this guard. This is the same
// evaluation failure by another route: an engine that answers Allow beside an
// error must not be believed, whatever produced the error.
func TestLocalRefusesAnAllowReachedWhileAPolicyErrored(t *testing.T) {
	local := bundleWith(t, `
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);
forbid(principal, action, resource) when { context.issuer < 1 };
`)

	decision, err := local.Authorize(context.Background(), Query{
		Principal: subjectReader, Issuer: testIssuer, Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
		Claims: subClaim(subjectReader),
	})

	require.Error(t, err, "an Allow reached while the forbid errored must not be served")
	assert.False(t, decision.Allow)
	assert.NotErrorIs(t, err, ErrInvalidQuery,
		"the question was fine; the engine could not answer it")
}

// The mirror: a Deny reached while something errored is still a Deny, and must
// not be turned into an engine error. Otherwise every ordinary refusal against a
// bundle with one erroring policy would report engine_error and an operator
// watching a permissive rollout would see a fault that is not there.
func TestLocalKeepsADenyThatErrored(t *testing.T) {
	// NKE::Role::"reader", not an undeclared role: loadBundle enforces
	// checkRoleVocabulary (validate.go) inside NewLocal itself, so a policy
	// naming a role that oneReader does not declare would refuse to load at
	// all, before Authorize is ever reached -- and that is not what this test
	// is checking. Which role is named here is otherwise incidental: the guard
	// always errors regardless of who it would have matched.
	local := bundleWith(t, `
permit(principal in NKE::Role::"reader", action, resource) when { context.issuer < 1 };
`)

	decision, err := local.Authorize(context.Background(), Query{
		Principal: subjectReader, Issuer: testIssuer, Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
		Claims: subClaim(subjectReader),
	})

	require.NoError(t, err, "a denial that errored is still a denial")
	assert.False(t, decision.Allow)
}

// The Cedar context the engine builds, pinned here because nothing else pins
// it. No shipped policy has a `when` clause reading context, so the conformance
// matrix cannot see it: replacing the record with an empty one left every test
// green.
func TestLocalSendsTheContextRecord(t *testing.T) {
	// Scoped to NKE::Role::"reader", so checkRoleReachability (validate.go)
	// does not refuse oneReader's "reader" role as unnamed by any policy. The
	// mappings grant subjectReader reader from both issuers, so only the
	// context record can tell the two requests apart.
	const otherIssuer = "some-other-idp"
	grant := func(issuer string) string {
		return `{"issuer":"` + issuer + `","claim":{"name":"sub","value":"` + subjectReader + `"},` +
			`"grants":[{"type":"NKE::Role","id":"reader"}]}`
	}
	local, err := NewLocal(writeBundleWithMappings(t, `
permit(principal in NKE::Role::"reader", action, resource)
when { context.issuer == "kube-apiserver" && context.method == "GET" };
`, shippedActions(t), oneReader, "["+grant(testIssuer)+","+grant(otherIssuer)+"]"),
		[]string{testIssuer, otherIssuer})
	require.NoError(t, err)

	allowed, err := local.Authorize(context.Background(), Query{
		Principal: subjectReader, Issuer: testIssuer, Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
		Claims: subClaim(subjectReader),
	})
	require.NoError(t, err)
	assert.True(t, allowed.Allow, "the context record must carry issuer and method")

	// The same request from another issuer must not match, or the issuer is not
	// really in the context.
	other, err := local.Authorize(context.Background(), Query{
		Principal: subjectReader, Issuer: otherIssuer, Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
		Claims: subClaim(subjectReader),
	})
	require.NoError(t, err)
	assert.False(t, other.Allow, "a policy pinning context.issuer must distinguish issuers")
}

// A caller-controlled resource identifier must not be able to shadow an entity
// the bundle declares. The overlay answers before the stored map, so if a
// resource type could ever collide with a Role or Principal uid, a request would
// redefine its own authority.
func TestResourceOverlayCannotShadowABundleEntity(t *testing.T) {
	local, err := NewLocal(shippedBundleWithMappings(t, testRoleMappings), testIssuers)
	require.NoError(t, err)

	// Every resource type the generated table can produce.
	resourceTypes := map[string]bool{CollectionType: true}
	for _, op := range operations {
		if op.ResourceType != "" {
			resourceTypes[op.ResourceType] = true
		}
	}
	for resourceType := range resourceTypes {
		assert.NotEqualf(t, PrincipalType, resourceType,
			"a route resolving to %s would let a caller shadow a principal", resourceType)
		assert.NotEqualf(t, ActionType, resourceType,
			"a route resolving to %s would let a caller shadow an action", resourceType)
		assert.NotEqualf(t, "NKE::Role", resourceType,
			"a route resolving to %s would let a caller shadow a role", resourceType)
	}

	// And no stored entity shares a type with any resource the router produces,
	// so the overlay's shadowing can never reach one.
	for uid := range local.entities {
		assert.NotContainsf(t, resourceTypes, string(uid.Type),
			"bundle entity %s shares a type with a routed resource; a request could shadow it", uid)
	}
}

// Guards the assumption the overlay rests on: it must return the request's
// resource and principal and defer to the bundle for everything else, matching
// how Authorize actually builds a principalAndResourceOverlay for a real
// request.
func TestResourceOverlayDefersToTheBundle(t *testing.T) {
	role := types.NewEntityUID("NKE::Role", "reader")
	base := types.EntityMap{role: types.Entity{UID: role}}
	resource := types.NewEntityUID("NKE::Cluster", "c1")
	principal := types.NewEntityUID(PrincipalType, "requester")

	view := principalAndResourceOverlay{
		base:      base,
		resource:  types.Entity{UID: resource},
		principal: types.Entity{UID: principal},
	}

	got, ok := view.Get(resource)
	require.True(t, ok)
	assert.Equal(t, resource, got.UID)

	got, ok = view.Get(principal)
	require.True(t, ok)
	assert.Equal(t, principal, got.UID)

	got, ok = view.Get(role)
	require.True(t, ok, "the overlay must not hide the bundle")
	assert.Equal(t, role, got.UID)

	_, ok = view.Get(types.NewEntityUID("NKE::Cluster", "absent"))
	assert.False(t, ok, "an entity in neither place must not be invented")
}

// Policy names in a decision come from the @id annotation. They are what appears
// in the log when a request is allowed, and cedar-go's positional "policy0"
// changes meaning the moment a policy is inserted above it — so the name a
// reader can find in the source is the one that must be reported.
func TestLocalReportsAnnotatedPolicyNames(t *testing.T) {
	local, err := NewLocal(shippedBundleWithMappings(t, testRoleMappings), testIssuers)
	require.NoError(t, err)

	decision, err := local.Authorize(context.Background(), Query{
		Principal: subjectReader, Issuer: testIssuer, Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
		Claims: subClaim(subjectReader),
	})
	require.NoError(t, err)
	require.True(t, decision.Allow)
	require.NotEmpty(t, decision.Reasons)

	assert.Contains(t, decision.Reasons, "readers-may-read",
		"the engine must report the @id the source carries")
	for _, reason := range decision.Reasons {
		assert.NotRegexp(t, `^policy\d+$`, reason,
			"a positional policy id is not a name a reader can find in the source")
	}
}
