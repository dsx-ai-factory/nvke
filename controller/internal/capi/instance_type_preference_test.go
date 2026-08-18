// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNextInPreference(t *testing.T) {
	t.Parallel()

	a := uuid.New()
	b := uuid.New()
	c := uuid.New()

	tests := []struct {
		name       string
		preference []uuid.UUID
		current    string
		wantNext   uuid.UUID
		wantErr    error
	}{
		{
			name:       "advances to next candidate",
			preference: []uuid.UUID{a, b, c},
			current:    a.String(),
			wantNext:   b,
		},
		{
			name:       "advances from middle candidate",
			preference: []uuid.UUID{a, b, c},
			current:    b.String(),
			wantNext:   c,
		},
		{
			name:       "wraps from last back to first",
			preference: []uuid.UUID{a, b, c},
			current:    c.String(),
			wantNext:   a,
		},
		{
			name:       "single-entry list returns itself",
			preference: []uuid.UUID{a},
			current:    a.String(),
			wantNext:   a,
		},
		{
			name:       "current not in list",
			preference: []uuid.UUID{a, b},
			current:    c.String(),
			wantErr:    ErrPreferenceCurrentNotFound,
		},
		{
			name:       "empty preference list",
			preference: nil,
			current:    a.String(),
			wantErr:    ErrPreferenceCurrentNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			next, err := NextInPreference(tc.preference, tc.current)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Equal(t, uuid.Nil, next)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantNext, next)
		})
	}
}
