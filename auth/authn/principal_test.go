// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

func TestPrincipalContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		ctx    func() context.Context
		want   *authn.Principal
		wantOK bool
	}{
		{
			name:   "no principal on a bare context",
			ctx:    context.Background,
			wantOK: false,
		},
		{
			// The load-bearing guard: a typed nil in the interface would otherwise
			// read as present.
			name: "a nil principal is reported as absent",
			ctx: func() context.Context {
				return authn.ContextWithPrincipal(context.Background(), nil)
			},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := authn.PrincipalFromContext(tt.ctx())
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// A string key of the same spelling must not reach the stored value.
func TestPrincipalContextKeyIsNotAString(t *testing.T) {
	t.Parallel()

	ctx := authn.ContextWithPrincipal(context.Background(), &authn.Principal{IssuerName: "spire"})
	require.Nil(t, ctx.Value("principal"))
	require.Nil(t, ctx.Value("auth.principal"))

	got, ok := authn.PrincipalFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, "spire", got.IssuerName)
}

// ServiceAccount is the one parse of a bound token's identity that admission
// and the authorization bundle's role mappings share. It requires the nested
// kubernetes.io claim and the subject to name the same account, so a token
// admitted on one identity cannot carry a claim naming another.
func TestPrincipalServiceAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		p      *authn.Principal
		want   authn.ServiceAccount
		wantOK bool
	}{
		{
			name:   "a bound token whose claim and subject agree",
			p:      &authn.Principal{Subject: "system:serviceaccount:ci:builder", Claims: ServiceAccountClaims("ci", "builder")},
			want:   authn.ServiceAccount{Namespace: "ci", Name: "builder"},
			wantOK: true,
		},
		{
			// A JWT-SVID admitted through admit.spiffe, carrying a forged
			// kubernetes.io claim that names a service account.
			name: "a SPIFFE subject carrying a kubernetes.io claim",
			p:    &authn.Principal{Subject: "spiffe://example.org/ns/ci/sa/builder", Claims: ServiceAccountClaims("ci", "builder")},
		},
		{
			name: "a claim naming a different account than the subject",
			p:    &authn.Principal{Subject: "system:serviceaccount:ci:builder", Claims: ServiceAccountClaims("ci", "admin")},
		},
		{
			name: "a claim naming a different namespace than the subject",
			p:    &authn.Principal{Subject: "system:serviceaccount:ci:builder", Claims: ServiceAccountClaims("prod", "builder")},
		},
		{
			name: "no kubernetes.io claim",
			p:    &authn.Principal{Subject: "system:serviceaccount:ci:builder", Claims: map[string]any{}},
		},
		{
			name: "a malformed kubernetes.io claim",
			p:    &authn.Principal{Subject: "system:serviceaccount:ci:builder", Claims: map[string]any{"kubernetes.io": "ci/builder"}},
		},
		{
			name: "an empty name in the claim",
			p:    &authn.Principal{Subject: "system:serviceaccount:ci:", Claims: ServiceAccountClaims("ci", "")},
		},
		{
			name: "a nil principal",
			p:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := tt.p.ServiceAccount()
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestServiceAccountMatches(t *testing.T) {
	t.Parallel()

	sa := authn.ServiceAccount{Namespace: "ci", Name: "deploy-bot"}
	name := "deploy-bot"
	other := "someone-else"

	assert.True(t, sa.Matches("ci", nil), "a nil name matches any name in the namespace")
	assert.True(t, sa.Matches("ci", &name))
	assert.False(t, sa.Matches("ci", &other))
	assert.False(t, sa.Matches("default", nil))
	assert.False(t, sa.Matches("default", &name))
}
