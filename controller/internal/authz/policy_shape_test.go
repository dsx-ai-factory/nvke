// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeBundle(t, `@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);
`+tt.policy, shippedActions(t), `[
 {"uid":{"type":"NKE::Role","id":"reader"},"attrs":{},"parents":[]},
 {"uid":{"type":"NKE::Principal","id":"system:serviceaccount:nke-system:nke-cli"},
  "attrs":{},"parents":[{"type":"NKE::Role","id":"reader"}]}
]`)

			_, err := NewLocal(dir)
			require.Error(t, err, "a rule that can never apply must not load as though it were enforced")
			if tt.shaped {
				assert.Contains(t, err.Error(), "nothing populates entity attributes")
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
		Principal: subjectReader, Issuer: "kube-apiserver", Action: "getCluster",
		ResourceType: "NKE::Cluster", ResourceID: someClusterID, Method: http.MethodGet,
	})
	require.NoError(t, err)
	assert.True(t, decision.Allow)
}

// And the bundle that ships must not trip the check, or every deployment is
// refused on startup.
func TestTheShippedBundleReadsNoEntityAttribute(t *testing.T) {
	_, err := NewLocal(bundleDir(t))
	require.NoError(t, err)
}
