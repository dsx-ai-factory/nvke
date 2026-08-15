// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package auth_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/auth"
)

func TestPrincipalContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		ctx    func() context.Context
		want   *auth.Principal
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
				return auth.ContextWithPrincipal(context.Background(), nil)
			},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := auth.PrincipalFromContext(tt.ctx())
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// A string key of the same spelling must not reach the stored value.
func TestPrincipalContextKeyIsNotAString(t *testing.T) {
	t.Parallel()

	ctx := auth.ContextWithPrincipal(context.Background(), &auth.Principal{IssuerName: "spire"})
	require.Nil(t, ctx.Value("principal"))
	require.Nil(t, ctx.Value("auth.principal"))

	got, ok := auth.PrincipalFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, "spire", got.IssuerName)
}
