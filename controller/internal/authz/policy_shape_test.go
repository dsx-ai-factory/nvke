// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The one shape in this bundle that fails open.
//
// Every other policy mistake denies: a typo'd action matches nothing, an
// undeclared entity holds no role. A rule that reads an entity attribute is
// different — it reads as a restriction and applies to nothing, so a forbid
// written that way grants exactly what it was written to refuse.

func TestABundleWhosePolicyReadsAResourceAttributeIsRefused(t *testing.T) {
	tests := []struct {
		name   string
		policy string

		// shaped is false for a policy this cedar-go refuses to parse at all.
		// The guarantee under test is that such a bundle does not load; which
		// of the two refusals it earns is cedar-go's business, not ours.
		shaped bool
	}{
		{
			// The shape the strict validator accepts. Measured before this
			// existed: it loads, is ready, and allows the write it forbids.
			name: "a forbid guarded by has",
			policy: `forbid(principal, action in NKE::Action::"write", resource)
			 when { resource has site && resource.site == "prod" };`,
			shaped: true,
		},
		{
			// The shape that errors at evaluation. Refused here rather than
			// left to the errored-Allow guard downstream.
			name: "a forbid reading the attribute directly",
			policy: `forbid(principal, action in NKE::Action::"write", resource)
			 when { resource.site == "prod" };`,
			shaped: true,
		},
		{
			name: "a permit reading a principal attribute",
			policy: `permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource)
			 when { principal has sites && principal.sites.contains("nke-1") };`,
			shaped: true,
		},
		{
			// Buried where a check that enumerated node types by hand would
			// have to have thought of if/then/else and negation to find it.
			name: "an attribute read nested in other operators",
			policy: `permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource)
			 when { !(if resource has site then true else false) };`,
			shaped: true,
		},
		{
			// Tags are a second attribute namespace, populated by nothing here
			// for the same reason. This cedar-go's text parser does not accept
			// the syntax, so the refusal comes from there — checkPolicyShape
			// covers the node types anyway, for a bundle built some other way.
			name: "a tag read",
			policy: `permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource)
			 when { resource hasTag "site" };`,
			shaped: false,
		},
		{
			// The receiver is a literal rather than a variable. An adversarial
			// review found this one: it is the same fail-open rule with the
			// same consequence, and a check written as a list of forbidden
			// receivers did not see it.
			name: "an entity literal receiver",
			policy: `forbid(principal, action, resource)
			 when { NKE::Cluster::"11111111-1111-1111-1111-111111111111" has site };`,
			shaped: true,
		},
		{
			name: "an entity literal receiver, read directly",
			policy: `forbid(principal, action, resource)
			 when { NKE::Cluster::"11111111-1111-1111-1111-111111111111".site == "prod" };`,
			shaped: true,
		},
		{
			// Neither a variable nor a literal. The receivers that can name an
			// entity are an open set, which is why the rule is written as what
			// context allows rather than as what entities forbid.
			name: "a receiver that is neither variable nor literal",
			policy: `forbid(principal, action, resource)
			 when { (if 1 == 1 then resource else resource) has site };`,
			shaped: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeBundle(t, `@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);
`+tt.policy, shippedActions(t), oneReader)

			_, err := NewLocal(dir, testIssuers)
			require.Error(t, err, "a rule that can never apply must not load as though it were enforced")
			if tt.shaped {
				assert.Contains(t, err.Error(), "context is the only thing whose attributes are populated")
			}
		})
	}
}

// context is the exception, and has to stay one: it is the only part of the
// request this engine populates, and the shipped schema declares both its keys.
func TestAPolicyMayReadTheContext(t *testing.T) {
	local := bundleWith(t, `
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource)
when { context.issuer == "kube-apiserver" && context.method == "GET" };
`)

	decision, err := local.Authorize(t.Context(), Query{
		Principal: subjectReader, Issuer: testIssuer, Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
		Claims: subClaim(subjectReader),
	})
	require.NoError(t, err)
	assert.True(t, decision.Allow)
}

// And the bundle that ships must not trip the check, or every deployment is
// refused on startup.
func TestTheShippedBundleReadsNoEntityAttribute(t *testing.T) {
	_, err := NewLocal(shippedBundleWithMappings(t, testRoleMappings), testIssuers)
	require.NoError(t, err)
}

// The error has to name the receiver, not only the attribute: an author who
// wrote a literal needs to be told which expression to change, and "site" alone
// does not say.
func TestTheRefusalNamesTheReceiver(t *testing.T) {
	for _, tt := range []struct{ name, policy, want string }{
		{"a variable", `forbid(principal, action, resource) when { resource has site };`, "resource.site"},
		{
			"a literal",
			`forbid(principal, action, resource) when { NKE::Cluster::"abc" has site };`,
			`NKE::Cluster::"abc".site`,
		},
		{
			"an expression",
			`forbid(principal, action, resource) when { (if 1 == 1 then resource else resource) has site };`,
			"an expression.site",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeBundle(t, `@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);
`+tt.policy, shippedActions(t), oneReader)
			_, err := NewLocal(dir, testIssuers)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// A second fail-open shape, one level inside the first: rootedAtContext alone
// only asks whether an expression reaches the context variable, not whether
// the specific key is one Local sets. `context` is rooted at context and reads
// clean by that test alone, but nothing populates a key nobody wrote to the
// record in local.go, so `context has neverSet` is false at every evaluation
// for the same reason a bare `resource has site` is: the shape this file
// exists to catch, relocated.
func TestABundleWhosePolicyReadsAnUnpopulatedContextKeyIsRefused(t *testing.T) {
	dir := writeBundle(t, `@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);
forbid(principal, action in NKE::Action::"write", resource)
 when { context has neverSet && context.neverSet == "prod" };`,
		shippedActions(t), oneReader)

	_, err := NewLocal(dir, testIssuers)
	require.Error(t, err, "a forbid keyed off a context attribute nothing sets must not load as though it were enforced")
	assert.Contains(t, err.Error(), "context.neverSet")
	assert.Contains(t, err.Error(), "context only has issuer and method populated")
}

// The two keys Local does set stay readable — this is the case the fix must
// not break.
func TestAPolicyMayReadThePopulatedContextKeys(t *testing.T) {
	for _, key := range []string{ContextKeyIssuer, ContextKeyMethod} {
		t.Run(key, func(t *testing.T) {
			dir := writeBundle(t, `@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource)
 when { context has `+key+` };`,
				shippedActions(t), oneReader)
			_, err := NewLocal(dir, testIssuers)
			require.NoError(t, err)
		})
	}
}

// The allowlist in policy_shape.go and the record local.go's Authorize builds
// have to name the same keys, or this bug comes back one level deeper: a third
// ContextKeyX added and populated in local.go but not added to
// isPopulatedContextKey's switch would make checkPolicyShape refuse a policy
// that reads a key that actually is safe now — the false-positive direction —
// or, if the switch is edited independently of local.go, accept a key nothing
// sets — this bug, again. Neither list is generated from the other, so this
// test reads local.go's own source and fails if the keys it puts in the
// context record and the keys isPopulatedContextKey accepts ever diverge,
// rather than trusting that a future editor remembers to touch both.
func TestPopulatedContextKeysMatchesLocal(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "local.go", nil, 0)
	require.NoError(t, err, "local.go must parse for this test to assert anything about it")

	var recordKeys []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewRecord" {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				ident, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				recordKeys = append(recordKeys, ident.Name)
			}
		}
		return true
	})

	require.NotEmpty(t, recordKeys, "expected to find the RecordMap literal Authorize builds in local.go")

	byName := map[string]string{"ContextKeyIssuer": ContextKeyIssuer, "ContextKeyMethod": ContextKeyMethod}
	for _, identName := range recordKeys {
		value, known := byName[identName]
		require.Truef(t, known,
			"local.go's context record sets %s, which this test does not know the value of yet — "+
				"add it to byName above so it can be checked against isPopulatedContextKey", identName)
		assert.Truef(t, isPopulatedContextKey(value),
			"local.go populates context key %q (%s) but isPopulatedContextKey refuses it — "+
				"a policy reading it would be wrongly rejected as fail-open", value, identName)
	}
}

// context is read through an attribute chain in the shipped schema's shape, and
// a chain rooted at context is still context.
func TestAPolicyMayReadThroughTheContext(t *testing.T) {
	local := bundleWith(t, `
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource)
when { context has issuer && context.issuer == "kube-apiserver" };
`)

	decision, err := local.Authorize(t.Context(), Query{
		Principal: subjectReader, Issuer: testIssuer, Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
		Claims: subClaim(subjectReader),
	})
	require.NoError(t, err)
	assert.True(t, decision.Allow)
}
