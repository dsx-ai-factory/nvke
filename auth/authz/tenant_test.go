// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/auth/authz"
)

func TestCheckTenantID(t *testing.T) {
	t.Parallel()

	atLimit := strings.Repeat("a", 256)
	tests := []struct {
		name    string
		id      string
		wantErr string
	}{
		{name: "a plain ID", id: "acme"},
		{name: "an inner space", id: "ac me"},
		{name: "a UUID", id: "72f988bf-86f1-41af-91ab-2d7cd011db47"},
		{name: "non-ASCII letters", id: "\u00e9quipe"},
		{name: "exactly 256 bytes", id: atLimit},
		{name: "257 bytes", id: atLimit + "a", wantErr: "is 257 bytes, longer than 256"},
		{name: "empty", id: "", wantErr: "is empty"},
		{name: "padded", id: " acme", wantErr: "has leading or trailing whitespace"},
		{name: "a newline", id: "ac\nme", wantErr: "holds U+000A at byte 2"},
		{name: "a zero-width space", id: "\u200bacme", wantErr: "holds U+200B at byte 0"},
		{name: "a replacement character", id: "ac\ufffdme", wantErr: "holds U+FFFD"},
		{name: "invalid UTF-8", id: "ac\xffme", wantErr: "is not valid UTF-8"},
		{name: "a quote", id: `ac"me`, wantErr: "holds U+0022"},
		{name: "a backslash", id: `ac\me`, wantErr: "holds U+005C"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := authz.CheckTenantID(tt.id)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// The conformance suite covers CheckIsolation through a backend. These cases
// are the inputs a backend's own checks would stop first.
func TestCheckIsolation(t *testing.T) {
	t.Parallel()

	inA := authz.Caller{Tenant: "a"}
	objectInA := authz.Resource{Type: "Cluster", ID: "1", Scope: authz.ScopeObject, Tenant: "a"}
	objectInB := authz.Resource{Type: "Cluster", ID: "2", Scope: authz.ScopeObject, Tenant: "b"}

	tests := []struct {
		name     string
		caller   authz.Caller
		resource authz.Resource
		want     authz.Outcome // empty when the request goes on to policy
	}{
		{name: "same tenant", caller: inA, resource: objectInA},
		{name: "cross tenant", caller: inA, resource: objectInB, want: authz.OutcomeNotFound},
		{name: "operator, cross tenant", caller: authz.Caller{Tenant: "ops", Operator: true}, resource: objectInB},
		{name: "operator, no tenant", caller: authz.Caller{Operator: true}, resource: objectInB, want: authz.OutcomeNoTenant},
		{
			name:     "an invalid caller tenant",
			caller:   authz.Caller{Tenant: "a\n"},
			resource: authz.Resource{Type: "Collection", ID: "clusters", Scope: authz.ScopeList},
			want:     authz.OutcomeNoTenant,
		},
		{
			name:     "an object with no owning tenant",
			caller:   inA,
			resource: authz.Resource{Type: "Cluster", ID: "2", Scope: authz.ScopeObject},
			want:     authz.OutcomeNotFound,
		},
		{
			name:     "a global resource naming a tenant",
			caller:   inA,
			resource: authz.Resource{Type: "Cluster", ID: "2", Scope: authz.ScopeGlobal, Tenant: "b"},
			want:     authz.OutcomeNotFound,
		},
		{
			name:     "a malformed caller tenant, on a global resource",
			caller:   authz.Caller{Tenant: "a\n"},
			resource: authz.Resource{Type: "Release", ID: "r1", Scope: authz.ScopeGlobal},
			want:     authz.OutcomeNoTenant,
		},
		{
			name:     "no caller tenant, on a global resource",
			resource: authz.Resource{Type: "Release", ID: "r1", Scope: authz.ScopeGlobal},
		},
		{
			name:     "no scope",
			caller:   authz.Caller{Tenant: "ops", Operator: true},
			resource: authz.Resource{Type: "Cluster", ID: "2", Tenant: "b"},
			want:     authz.OutcomeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, authz.CheckIsolation(tt.caller, tt.resource))
		})
	}
}
