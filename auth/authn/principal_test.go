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
