// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cedar-policy/cedar-go/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/auth"
)

// The principal overlay (design-authz-claim-based-roles.md, §3.3): a
// synthesized principal entity carrying roles.json's static parents unioned
// with Query.ClaimRoles, mirroring resourceOverlay.

// rolesUnionBundle: two independent roles, "reader" (static, via roles.json)
// and "auditor" (claim-only), and a policy that requires both -- so a
// decision it grants is proof the two sources were actually unioned, not
// that either alone happened to be enough (design §6's
// entity_duplicates_test.go-style union test).
func rolesUnionBundle(t *testing.T) string {
	t.Helper()
	return writeBundle(t,
		`@id("both-reader-and-auditor-may-read")
permit(principal, action in NKE::Action::"read", resource)
when { principal in NKE::Role::"reader" && principal in NKE::Role::"auditor" };`,
		shippedActions(t),
		`[
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Role","id":"auditor"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Principal","id":"static-reader-only"},
  "attrs":{},"parents":[{"type":"NKE::Role","id":"reader"}]}
]`)
}

func TestOverlayPrincipalUnionsStaticAndClaimDerivedRoles(t *testing.T) {
	t.Parallel()

	local, err := NewLocal(rolesUnionBundle(t))
	require.NoError(t, err)

	q := Query{
		Principal: "static-reader-only", Issuer: "kube-apiserver", Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
	}

	t.Run("the static role alone is not enough", func(t *testing.T) {
		t.Parallel()
		decision, err := local.Authorize(context.Background(), q)
		require.NoError(t, err)
		assert.False(t, decision.Allow, "the policy requires auditor too, which this principal does not hold yet")
	})

	t.Run("the claim-derived role alone is not enough either", func(t *testing.T) {
		t.Parallel()
		other := q
		other.Principal = "claim-auditor-only"
		other.ClaimRoles = []string{"auditor"}
		decision, err := local.Authorize(context.Background(), other)
		require.NoError(t, err)
		assert.False(t, decision.Allow, "this principal has no static reader parent in roles.json")
	})

	t.Run("static reader plus claim-derived auditor together grant it", func(t *testing.T) {
		t.Parallel()
		both := q
		both.ClaimRoles = []string{"auditor"}
		decision, err := local.Authorize(context.Background(), both)
		require.NoError(t, err)
		assert.True(t, decision.Allow, "the union of roles.json's static parent and Query.ClaimRoles must both be present in the overlay")
	})
}

// A claim-derived role name the loaded bundle does not declare must be
// dropped from the synthesized entity's Parents, not passed into it --
// cedar-go's ancestry walk checks Parents.Contains directly and never
// confirms a parent is itself a loaded entity (design §3.3, §5, §6).
func TestOverlayPrincipalDropsAnUndeclaredRole(t *testing.T) {
	t.Parallel()

	local, err := NewLocal(rolesUnionBundle(t))
	require.NoError(t, err)

	uid := types.NewEntityUID(PrincipalType, types.String("nobody-in-particular"))
	entity := overlayPrincipal(local.entities, uid, []string{"reader", "typo-operator"})

	hasReal := entity.Parents.Contains(types.NewEntityUID(RoleType, types.String("reader")))
	assert.True(t, hasReal, "a declared role must still be included")

	hasDangling := entity.Parents.Contains(types.NewEntityUID(RoleType, types.String("typo-operator")))
	assert.False(t, hasDangling, "an undeclared role name must never reach Parents")
}

// The overlay's parent set is a new, immutable types.EntityUIDSet: base's own
// Parents must never be reused or mutated, because base (l.entities) is
// shared across every concurrent request (design §3.3, run under -race).
func TestOverlayPrincipalDoesNotMutateTheBaseEntity(t *testing.T) {
	t.Parallel()

	local, err := NewLocal(rolesUnionBundle(t))
	require.NoError(t, err)

	uid := types.NewEntityUID(PrincipalType, types.String("static-reader-only"))
	before, ok := local.entities.Get(uid)
	require.True(t, ok)
	beforeCount := before.Parents.Len()

	done := make(chan struct{})
	for _, roles := range [][]string{{"auditor"}, {"reader"}, nil, {"typo-operator"}} {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 25 {
				_ = overlayPrincipal(local.entities, uid, roles)
			}
		}()
	}
	for range 4 {
		<-done
	}

	after, ok := local.entities.Get(uid)
	require.True(t, ok)
	assert.Equal(t, beforeCount, after.Parents.Len(), "the stored entity's own Parents must be unchanged")
}

// The §2/§5 review-scrutiny case, proven both directions: a claim-derived
// role can silence a forbid's negation, and only for the claim value the
// mapping actually targets.
//
// only-node-operators-change-node-lifecycle (deploy/authz/policies.cedar)
// forbids deleteNode/deleteNodePool/updateNodePool for everyone except
// nodeOperator. subjectOperator already holds "operator" -- which
// operators-may-write already extends to those same three actions -- so
// nodeOperator is the only thing standing between it and deleting nodes.
func TestClaimDerivedNodeOperatorSilencesTheForbid(t *testing.T) {
	t.Parallel()

	local, err := NewLocal(bundleDir(t))
	require.NoError(t, err)

	deleteNode := Query{
		Principal: subjectOperator, Issuer: "kube-apiserver", Action: "deleteNode",
		ResourceType: "NKE::Node", ResourceID: someNodeID, Method: http.MethodDelete,
	}

	t.Run("without the claim-derived role, the forbid holds", func(t *testing.T) {
		t.Parallel()
		decision, err := local.Authorize(context.Background(), deleteNode)
		require.NoError(t, err)
		assert.False(t, decision.Allow)
	})

	t.Run("an unrelated or unmapped claim role does not leak the grant", func(t *testing.T) {
		t.Parallel()
		q := deleteNode
		q.ClaimRoles = []string{"marketing"}
		decision, err := local.Authorize(context.Background(), q)
		require.NoError(t, err)
		assert.False(t, decision.Allow)
	})

	t.Run("a claim-granted nodeOperator silences the forbid", func(t *testing.T) {
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
					Principal: subjectOperator, Issuer: "kube-apiserver", Action: tt.action,
					ResourceType: tt.rtype, ResourceID: tt.rid, Method: tt.method,
					ClaimRoles: []string{"nodeOperator"},
				}
				decision, err := local.Authorize(context.Background(), q)
				require.NoError(t, err)
				assert.True(t, decision.Allow, "a claim-derived nodeOperator must silence only-node-operators-change-node-lifecycle the same way a roles.json grant would")
			})
		}
	})
}

// End-to-end through the real middleware: auth.Principal.ClaimRoles reaches
// Query.ClaimRoles with no mapping logic on the authz side at all (design
// §3.2's "Middleware does nothing but copy it").
func TestMiddlewareCopiesClaimRolesFromThePrincipal(t *testing.T) {
	t.Parallel()

	local, err := NewLocal(bundleDir(t))
	require.NoError(t, err)

	mux := http.NewServeMux()
	api.HandlerWithOptions(nil, api.StdHTTPServerOptions{BaseRouter: mux, BaseURL: api.BasePath})
	resolver, err := NewResolver(mux)
	require.NoError(t, err)
	middleware, err := NewMiddleware(local, resolver, ModeEnforcing, slog.New(slog.DiscardHandler), nil)
	require.NoError(t, err)

	var reached bool
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/nodes/"+someNodeID, http.NoBody)
	r = r.WithContext(auth.ContextWithPrincipal(r.Context(), &auth.Principal{
		IssuerName: "kube-apiserver",
		Issuer:     "https://kubernetes.default.svc.cluster.local",
		Subject:    subjectOperator,
		ClaimRoles: []string{"nodeOperator"},
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusOK, rec.Code, "Principal.ClaimRoles must reach the engine through the middleware with no authz-side mapping")
	assert.True(t, reached)
}
