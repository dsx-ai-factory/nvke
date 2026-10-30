// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authz"
)

// TestListTenant checks the tenant a list is filtered to, and that ListTenant
// refuses when there is no valid authorized caller.
func TestListTenant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		caller     *authz.Caller // none on the context if nil
		wantTenant string
		wantAll    bool
		wantErr    bool
	}{
		{name: "no authorized caller", caller: nil, wantErr: true},
		{name: "a tenant caller", caller: &authz.Caller{Tenant: "acme"}, wantTenant: "acme"},
		{name: "an operator", caller: &authz.Caller{Tenant: "nvidia-ops", Operator: true}, wantAll: true},
		// Allowed on a ScopeGlobal resource, then reaching a list.
		{name: "a caller with no tenant", caller: &authz.Caller{}, wantErr: true},
		{name: "an operator with no tenant", caller: &authz.Caller{Operator: true}, wantErr: true},
		{name: "a caller with a malformed tenant", caller: &authz.Caller{Tenant: " acme"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			if tt.caller != nil {
				ctx = authz.ContextWithCaller(ctx, *tt.caller)
			}
			tenant, all, err := authz.ListTenant(ctx)
			if tt.wantErr {
				require.ErrorIs(t, err, authz.ErrNoCaller)
				assert.False(t, all, "a failure never lists every tenant")
				assert.Empty(t, tenant)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantTenant, tenant)
			assert.Equal(t, tt.wantAll, all)
		})
	}
}

// TestCallerFromContextWithNone checks that a context with no authorized
// caller reports none.
func TestCallerFromContextWithNone(t *testing.T) {
	t.Parallel()

	_, ok := authz.CallerFromContext(context.Background())
	assert.False(t, ok)
}
