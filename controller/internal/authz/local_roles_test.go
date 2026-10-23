// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/cedar-policy/cedar-go/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

// The principal overlay: a synthesized principal entity carrying the roles
// roleMappings.json grants the caller, mirroring principalAndResourceOverlay's
// resource half.

// rolesUnionBundle: two independent roles, "reader" (granted to one caller by
// its sub claim) and "auditor" (granted by a groups claim), and a policy that
// requires both -- so a decision it grants is proof the two mappings were
// actually unioned, not that either alone happened to be enough.
func rolesUnionBundle(t *testing.T) string {
	t.Helper()
	return writeBundleWithMappings(t,
		`@id("both-reader-and-auditor-may-read")
permit(principal, action in NKE::Action::"read", resource)
when { principal in NKE::Role::"reader" && principal in NKE::Role::"auditor" };`,
		shippedActions(t),
		`[
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Role","id":"auditor"},"attrs":{},"parents":[]}
]`,
		`[
 {"issuer":"`+testIssuer+`","claim":{"name":"sub","value":"reader-by-sub"},
  "grants":[{"type":"NKE::Role","id":"reader"}]},
 {"issuer":"`+testIssuer+`","claim":{"name":"groups","value":"auditors"},
  "grants":[{"type":"NKE::Role","id":"auditor"}]}
]`)
}

func TestOverlayPrincipalUnionsEveryMatchingMapping(t *testing.T) {
	t.Parallel()

	local, err := NewLocal(rolesUnionBundle(t), testIssuers)
	require.NoError(t, err)

	q := Query{
		Principal: "reader-by-sub", Issuer: testIssuer, Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
		Claims: subClaim("reader-by-sub"),
	}
	both := map[string]any{"sub": "reader-by-sub", "groups": []any{"auditors"}}

	t.Run("the sub mapping alone is not enough", func(t *testing.T) {
		t.Parallel()
		decision, err := local.Authorize(context.Background(), q)
		require.NoError(t, err)
		assert.False(t, decision.Allow, "the policy requires auditor too, which this caller does not hold yet")
	})

	t.Run("the groups mapping alone is not enough either", func(t *testing.T) {
		t.Parallel()
		other := q
		other.Principal = "mapped-auditor-only"
		other.Claims = map[string]any{"sub": "mapped-auditor-only", "groups": []any{"auditors"}}
		decision, err := local.Authorize(context.Background(), other)
		require.NoError(t, err)
		assert.False(t, decision.Allow, "no mapping grants this caller reader")
	})

	t.Run("both mappings together grant it", func(t *testing.T) {
		t.Parallel()
		withBoth := q
		withBoth.Claims = both
		decision, err := local.Authorize(context.Background(), withBoth)
		require.NoError(t, err)
		assert.True(t, decision.Allow, "the roles of both matching mappings must be present in the overlay")
	})

	t.Run("a mapping keyed on another issuer grants nothing", func(t *testing.T) {
		t.Parallel()
		elsewhere := q
		elsewhere.Issuer = "some-other-issuer"
		elsewhere.Claims = both
		decision, err := local.Authorize(context.Background(), elsewhere)
		require.NoError(t, err)
		assert.False(t, decision.Allow, "the same claims from an issuer the entries do not name must not match them")
	})
}

// A grant to one caller is a claim mapping on a claim that identifies it, and
// it holds only for tokens from the issuer the mapping names. Another issuer
// can mint the same sub; its token holds that issuer's grants and no others.
func TestASubMappingGrantsOnlyWithinItsIssuer(t *testing.T) {
	t.Parallel()

	const (
		user    = "user-1"
		other   = "other-idp"
		unnamed = "unmapped-idp"
	)
	local, err := NewLocal(shippedBundleWithMappings(t, `[
 {"issuer":"`+testIssuer+`","claim":{"name":"sub","value":"`+user+`"},"grants":[{"type":"NKE::Role","id":"admin"}]},
 {"issuer":"`+other+`","claim":{"name":"sub","value":"`+user+`"},"grants":[{"type":"NKE::Role","id":"reader"}]}
]`), []string{testIssuer, other, unnamed})
	require.NoError(t, err)

	authorize := func(issuer, action, method string) bool {
		t.Helper()
		decision, err := local.Authorize(context.Background(), Query{
			Principal: user, Issuer: issuer, Action: action,
			ResourceType: "NKE::Node", ResourceID: someNodeID, Method: method,
			Claims: subClaim(user),
		})
		require.NoError(t, err)
		return decision.Allow
	}

	assert.True(t, authorize(testIssuer, "deleteNode", http.MethodDelete),
		"the mapping's own issuer holds admin")
	assert.False(t, authorize(other, "deleteNode", http.MethodDelete),
		"the same sub from another issuer must not hold the first issuer's admin grant")
	assert.True(t, authorize(other, "getNode", http.MethodGet),
		"the other issuer's token holds that issuer's own reader grant")
	assert.False(t, authorize(unnamed, "getNode", http.MethodGet),
		"an issuer no mapping names grants its token nothing, whatever its sub")
}

// Claims reach Cedar only as the role UIDs parentsFor resolves them to. A
// caller whose token carries a claim named like a context key must not be
// able to shadow it: a policy keyed on context.issuer sees the verified
// issuer name, never the claim.
func TestClaimsNeverReachTheCedarContext(t *testing.T) {
	t.Parallel()

	local, err := NewLocal(writeBundleWithMappings(t,
		`@id("attacker-issuer-may-read")
permit(principal, action, resource) when { context.issuer == "attacker" };`,
		shippedActions(t), oneReader, aMapping), testIssuers)
	require.NoError(t, err)

	q := Query{
		Principal: "someone", Issuer: testIssuer, Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
		Claims: map[string]any{ContextKeyIssuer: "attacker"},
	}
	decision, err := local.Authorize(context.Background(), q)
	require.NoError(t, err)
	assert.False(t, decision.Allow, "an issuer claim must not become context.issuer")

	// The policy does grant when the verified issuer is the one it names, so
	// the deny above is the context, not a policy that never fires.
	q.Issuer = "attacker"
	decision, err = local.Authorize(context.Background(), q)
	require.NoError(t, err)
	assert.True(t, decision.Allow)
}

// A mapped parent the loaded bundle does not declare must be dropped from the
// synthesized entity's Parents, not passed into it -- cedar-go's ancestry
// walk checks Parents.Contains directly and never confirms a parent is itself
// a loaded entity. The load refuses such a mapping, so this is the defense in
// depth under it.
func TestOverlayPrincipalDropsAnUndeclaredRole(t *testing.T) {
	t.Parallel()

	local, err := NewLocal(rolesUnionBundle(t), testIssuers)
	require.NoError(t, err)

	uid := types.NewEntityUID(PrincipalType, types.String("nobody-in-particular"))
	entity := overlayPrincipal(local.entities, uid, []types.EntityUID{
		types.NewEntityUID(RoleType, "reader"),
		types.NewEntityUID(RoleType, "typo-operator"),
	})

	hasReal := entity.Parents.Contains(types.NewEntityUID(RoleType, types.String("reader")))
	assert.True(t, hasReal, "a declared role must still be included")

	hasDangling := entity.Parents.Contains(types.NewEntityUID(RoleType, types.String("typo-operator")))
	assert.False(t, hasDangling, "an undeclared role must never reach Parents")
}

// The load refuses a stored principal, and request time ignores one anyway:
// the overlay holds exactly the mapped roles, never a stored entity's parents.
func TestOverlayPrincipalIgnoresAStoredPrincipal(t *testing.T) {
	t.Parallel()

	admin := types.NewEntityUID(RoleType, "admin")
	reader := types.NewEntityUID(RoleType, "reader")
	uid := types.NewEntityUID(PrincipalType, "x")
	base := types.EntityMap{
		admin:  {UID: admin, Parents: types.NewEntityUIDSet(), Attributes: types.NewRecord(nil)},
		reader: {UID: reader, Parents: types.NewEntityUIDSet(), Attributes: types.NewRecord(nil)},
		uid:    {UID: uid, Parents: types.NewEntityUIDSet(admin), Attributes: types.NewRecord(nil)},
	}

	entity := overlayPrincipal(base, uid, []types.EntityUID{reader})
	assert.Equal(t, []types.EntityUID{reader}, slices.Collect(entity.Parents.All()),
		"the overlay's Parents are the mapped roles only")
}

// A mapped role can silence a forbid's negation, and only for the claim value
// the mapping actually names.
//
// only-node-operators-change-node-lifecycle (deploy/authz/policies.cedar)
// forbids deleteNode/deleteNodePool/updateNodePool for everyone except
// nodeOperator. subjectOperator holds "operator" through its sub mapping --
// and operators-may-write already extends to those same three actions -- so
// nodeOperator is the only thing standing between it and deleting nodes.
func TestMappedNodeOperatorSilencesTheForbid(t *testing.T) {
	t.Parallel()

	local, err := NewLocal(shippedBundleWithMappings(t, `[
 {"issuer":"`+testIssuer+`","claim":{"name":"sub","value":"`+subjectOperator+`"},
  "grants":[{"type":"NKE::Role","id":"operator"}]},
 {"issuer":"`+testIssuer+`","claim":{"name":"groups","value":"node-ops"},
  "grants":[{"type":"NKE::Role","id":"nodeOperator"}]}
]`), testIssuers)
	require.NoError(t, err)

	deleteNode := Query{
		Principal: subjectOperator, Issuer: testIssuer, Action: "deleteNode",
		ResourceType: "NKE::Node", ResourceID: someNodeID, Method: http.MethodDelete,
		Claims: subClaim(subjectOperator),
	}

	t.Run("without the mapped role, the forbid holds", func(t *testing.T) {
		t.Parallel()
		decision, err := local.Authorize(context.Background(), deleteNode)
		require.NoError(t, err)
		assert.False(t, decision.Allow)
		assert.Contains(t, decision.Reasons, "only-node-operators-change-node-lifecycle",
			"the forbid refused, not a missing permit")
	})

	t.Run("an unmapped claim value does not leak the grant", func(t *testing.T) {
		t.Parallel()
		q := deleteNode
		q.Claims = map[string]any{"sub": subjectOperator, "groups": []any{"marketing", "nodeOperator"}}
		decision, err := local.Authorize(context.Background(), q)
		require.NoError(t, err)
		assert.False(t, decision.Allow, "a claim value spelled like a role grants nothing unless an entry maps it")
	})

	t.Run("a mapped nodeOperator silences the forbid", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			name   string
			action string
			method string
			rtype  string
			rid    string
		}{
			{"deleteNode", "deleteNode", http.MethodDelete, "NKE::Node", someNodeID},
			{"deleteNodePool", "deleteNodePool", http.MethodDelete, "NKE::NodePool", someNodePoolID},
			{"updateNodePool", "updateNodePool", http.MethodPatch, "NKE::NodePool", someNodePoolID},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				q := Query{
					Principal: subjectOperator, Issuer: testIssuer, Action: tt.action,
					ResourceType: tt.rtype, ResourceID: tt.rid, Method: tt.method,
					Claims: map[string]any{"sub": subjectOperator, "groups": "node-ops"},
				}
				decision, err := local.Authorize(context.Background(), q)
				require.NoError(t, err)
				assert.True(t, decision.Allow, "a mapped nodeOperator must silence only-node-operators-change-node-lifecycle")
			})
		}
	})
}

// End-to-end through the real middleware: the Principal's claims and verified
// service account reach the engine, and the bundle's mappings turn them into
// roles.
func TestMiddlewareCarriesThePrincipalToTheMappings(t *testing.T) {
	t.Parallel()

	local, err := NewLocal(shippedBundleWithMappings(t, `[
 {"issuer":"`+testIssuer+`","serviceAccount":{"namespace":"ci","name":"builder"},
  "grants":[{"type":"NKE::Role","id":"reader"}]},
 {"issuer":"`+testIssuer+`","claim":{"name":"groups","value":"auditors"},
  "grants":[{"type":"NKE::Role","id":"reader"}]},
 {"issuer":"spire","spiffe":{"trustDomain":"nvidia.com","expression":"path == '/ns/nke-system/sa/nke-controller'"},
  "grants":[{"type":"NKE::Role","id":"nodeOperator"}]}
]`), []string{testIssuer, "spire"})
	require.NoError(t, err)

	mux := http.NewServeMux()
	api.HandlerWithOptions(nil, api.StdHTTPServerOptions{BaseRouter: mux, BaseURL: api.BasePath})
	resolver, err := NewResolver(mux)
	require.NoError(t, err)
	middleware, err := NewMiddleware(local, resolver, ModeEnforcing, slog.New(slog.DiscardHandler), nil)
	require.NoError(t, err)

	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	serve := func(method, path string, p *authn.Principal) int {
		r := httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody)
		r = r.WithContext(authn.ContextWithPrincipal(r.Context(), p))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Code
	}
	builderClaims := map[string]any{
		"kubernetes.io": map[string]any{
			"namespace":      "ci",
			"serviceaccount": map[string]any{"name": "builder"},
		},
	}

	t.Run("a bound service account token matching an entry holds its role", func(t *testing.T) {
		t.Parallel()
		code := serve(http.MethodGet, "/api/clusters", &authn.Principal{
			IssuerName: testIssuer, Subject: "system:serviceaccount:ci:builder", Claims: builderClaims,
		})
		assert.Equal(t, http.StatusOK, code)
	})

	t.Run("a claim matching an entry holds its role", func(t *testing.T) {
		t.Parallel()
		code := serve(http.MethodGet, "/api/clusters", &authn.Principal{
			IssuerName: testIssuer, Subject: "someone-else",
			Claims: map[string]any{"groups": []any{"auditors"}},
		})
		assert.Equal(t, http.StatusOK, code)
	})

	t.Run("the same caller without the claim holds nothing", func(t *testing.T) {
		t.Parallel()
		code := serve(http.MethodGet, "/api/clusters", &authn.Principal{
			IssuerName: testIssuer, Subject: "someone-else",
			Claims: map[string]any{"groups": []any{"marketing"}},
		})
		assert.Equal(t, http.StatusForbidden, code)
	})

	t.Run("a kubernetes.io claim that disagrees with the subject holds nothing", func(t *testing.T) {
		t.Parallel()
		code := serve(http.MethodGet, "/api/clusters", &authn.Principal{
			IssuerName: testIssuer, Subject: "spiffe://nvidia.com/ns/ci/sa/builder", Claims: builderClaims,
		})
		assert.Equal(t, http.StatusForbidden, code)
	})

	t.Run("a SPIFFE entry grants its role to a subject its expression accepts", func(t *testing.T) {
		t.Parallel()
		code := serve(http.MethodDelete, "/api/nodes/"+someNodeID, &authn.Principal{
			IssuerName: "spire", Subject: subjectOperator,
		})
		assert.Equal(t, http.StatusOK, code)
	})
}
