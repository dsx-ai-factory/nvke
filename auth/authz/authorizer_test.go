// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
)

// validQuery returns a query that passes Validate, for each case to change.
func validQuery() authz.Query {
	return authz.Query{
		Principal: authn.Principal{IssuerName: "keycloak", Subject: "user-1"},
		Action:    "getCluster",
		Resource:  authz.Resource{Type: "Cluster", ID: "c1", Scope: authz.ScopeObject, Tenant: "acme"},
	}
}

func TestQueryValidate(t *testing.T) {
	t.Parallel()

	setScope := func(scope authz.Scope, tenant string) func(*authz.Query) {
		return func(q *authz.Query) { q.Resource.Scope, q.Resource.Tenant = scope, tenant }
	}
	tests := []struct {
		name    string
		mutate  func(*authz.Query)
		wantErr string
	}{
		{name: "an object", mutate: func(*authz.Query) {}},
		{name: "a create naming a tenant", mutate: setScope(authz.ScopeCreate, "acme")},
		{name: "a create naming no tenant", mutate: setScope(authz.ScopeCreate, "")},
		{name: "a list", mutate: setScope(authz.ScopeList, "")},
		{name: "a global resource", mutate: setScope(authz.ScopeGlobal, "")},
		{name: "a SPIFFE subject", mutate: func(q *authz.Query) { q.Principal.Subject = "spiffe://dsx.example/ns/a/sa/b" }},
		{name: "no subject", mutate: func(q *authz.Query) { q.Principal.Subject = "" }, wantErr: "principal subject is empty"},
		{name: "no issuer name", mutate: func(q *authz.Query) { q.Principal.IssuerName = "" }, wantErr: "principal issuer name is empty"},
		{name: "no action", mutate: func(q *authz.Query) { q.Action = "" }, wantErr: "action is empty"},
		{name: "no resource type", mutate: func(q *authz.Query) { q.Resource.Type = "" }, wantErr: "resource type is empty"},
		{name: "no resource ID", mutate: func(q *authz.Query) { q.Resource.ID = "" }, wantErr: "resource ID is empty"},
		{name: "a quote in a resource ID", mutate: func(q *authz.Query) { q.Resource.ID = `c"1` }, wantErr: "resource ID holds U+0022 at byte 1"},
		{
			name:    "a subject over 2048 bytes",
			mutate:  func(q *authz.Query) { q.Principal.Subject = strings.Repeat("a", 2049) },
			wantErr: "principal subject is 2049 bytes, longer than 2048",
		},
		{name: "no scope", mutate: setScope("", "acme"), wantErr: `resource scope "" is not object, create, list or global`},
		{name: "an object with no owning tenant", mutate: setScope(authz.ScopeObject, ""), wantErr: "resource tenant is empty on an object"},
		{name: "a list naming a tenant", mutate: setScope(authz.ScopeList, "acme"), wantErr: "resource tenant is set on scope list"},
		{name: "a global resource naming a tenant", mutate: setScope(authz.ScopeGlobal, "acme"), wantErr: "resource tenant is set on scope global"},
		{name: "a padded owning tenant", mutate: setScope(authz.ScopeObject, " acme"), wantErr: "resource tenant has leading or trailing whitespace"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			q := validQuery()
			tt.mutate(&q)
			err := q.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, authz.ErrInvalidQuery)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}
