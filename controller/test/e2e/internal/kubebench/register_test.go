// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubebench

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validEntry = `
  - id: "1.2.11"
    title: Image pull policy
    reason: Pre-loaded images must start without a registry.
`

func TestLoadRegister(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr []string
		check   func(t *testing.T, reg Register)
	}{
		{
			name: "a valid entry loads",
			yaml: "exceptions:" + validEntry,
			check: func(t *testing.T, reg Register) {
				t.Helper()
				require.Len(t, reg.Exceptions, 1)
				assert.Equal(t, Exception{
					ID:     "1.2.11",
					Title:  "Image pull policy",
					Reason: "Pre-loaded images must start without a registry.",
				}, reg.Exceptions[0])
			},
		},
		{
			name:    "an unknown field is an error",
			yaml:    "exceptions:" + validEntry + "    approver: someone\n",
			wantErr: []string{"approver"},
		},
		{
			name: "a missing title or reason is an error naming the id",
			yaml: `
exceptions:
  - id: "4.2.9"
`,
			wantErr: []string{"4.2.9", "title", "reason"},
		},
		{
			name: "an id that is not dotted numbers is an error",
			yaml: `
exceptions:
  - id: "1.2.x"
    title: t
    reason: r
`,
			wantErr: []string{`"1.2.x"`, "dotted"},
		},
		{
			name: "an unquoted id that YAML reads as a number is an error",
			yaml: `
exceptions:
  - id: 5.1
    title: t
    reason: r
`,
			wantErr: []string{"5.1", "quote"},
		},
		{
			name: "two entries for one check are an error",
			yaml: `
exceptions:
  - id: "5.1.1"
    title: t
    reason: r
  - id: "5.1.1"
    title: t
    reason: r
`,
			wantErr: []string{"5.1.1", "more than once"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg, err := LoadRegister([]byte(tc.yaml))
			if len(tc.wantErr) > 0 {
				require.Error(t, err)
				for _, fragment := range tc.wantErr {
					assert.Contains(t, err.Error(), fragment)
				}
				return
			}
			require.NoError(t, err)
			tc.check(t, reg)
		})
	}
}

// TestDefaultRegister makes every edit to exceptions.yaml pass the same checks
// as a hand-written register, under make test.
func TestDefaultRegister(t *testing.T) {
	t.Parallel()

	reg, err := DefaultRegister()
	require.NoError(t, err)
	assert.NotEmpty(t, reg.Exceptions)
}

// YAML reads an unquoted 5.10 as the number 5.1, so the error cannot echo the
// ID as written and must not suggest a different check.
func TestLoadRegisterUnquotedIDDoesNotSuggestAnotherCheck(t *testing.T) {
	t.Parallel()

	_, err := LoadRegister([]byte(`
exceptions:
  - id: 5.10
    title: t
    reason: r
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceptions[0]")
	assert.Contains(t, err.Error(), "number")
	assert.Contains(t, err.Error(), "quote")
	assert.NotContains(t, err.Error(), `id: "5.1"`)
}
