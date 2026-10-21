// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRegistry(t *testing.T, yaml string) *Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checks.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))
	reg, err := LoadRegistry(path)
	require.NoError(t, err)
	return reg
}

const twoChecks = `
version: 0.0.1-test
checks:
  - id: T-001
    family: test
    severity: blocking
    method: static
    verifies: something
    why: because
  - id: T-002
    family: test
    severity: warn
    method: static
    appliesTo: {variant: [fde]}
    verifies: something else
    why: also because
`

func TestEngineRun(t *testing.T) {
	reg := testRegistry(t, twoChecks)
	pass := func(context.Context, Target, Inputs) (Outcome, error) {
		return Outcome{Status: StatusPass, Evidence: "ok"}, nil
	}
	boom := func(context.Context, Target, Inputs) (Outcome, error) {
		return Outcome{}, errors.New("probe exploded")
	}

	tests := []struct {
		name   string
		impls  map[string]Func
		target Target
		want   map[string]Status
	}{
		{
			name:   "all applicable and passing",
			impls:  map[string]Func{"T-001": pass, "T-002": pass},
			target: Target{Variant: VariantFDE},
			want:   map[string]Status{"T-001": StatusPass, "T-002": StatusPass},
		},
		{
			name:   "appliesTo mismatch is a skip, not a pass",
			impls:  map[string]Func{"T-001": pass, "T-002": pass},
			target: Target{Variant: VariantNonFDE},
			want:   map[string]Status{"T-001": StatusPass, "T-002": StatusSkip},
		},
		{
			name:   "missing implementation is an error, not a silent pass",
			impls:  map[string]Func{"T-001": pass},
			target: Target{Variant: VariantFDE},
			want:   map[string]Status{"T-001": StatusPass, "T-002": StatusError},
		},
		{
			name:   "implementation error is recorded as StatusError",
			impls:  map[string]Func{"T-001": boom, "T-002": pass},
			target: Target{Variant: VariantFDE},
			want:   map[string]Status{"T-001": StatusError, "T-002": StatusPass},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := NewEngine(tt.impls).Run(context.Background(), reg, tt.target, Inputs{})
			require.Len(t, results, len(reg.Checks), "one result per registry entry, in order")
			for _, res := range results {
				assert.Equal(t, tt.want[res.ID], res.Status, res.ID)
				assert.NotEmpty(t, res.Evidence, "%s must carry evidence", res.ID)
			}
		})
	}
}

func TestAppliesToMatches(t *testing.T) {
	tests := []struct {
		name   string
		sel    AppliesTo
		target Target
		want   bool
	}{
		{name: "empty selector matches everything", sel: AppliesTo{}, target: Target{Variant: VariantFDE, Arch: "amd64"}, want: true},
		{name: "variant match", sel: AppliesTo{Variant: []Variant{VariantFDE}}, target: Target{Variant: VariantFDE}, want: true},
		{name: "variant mismatch", sel: AppliesTo{Variant: []Variant{VariantFDE}}, target: Target{Variant: VariantNonFDE}, want: false},
		{name: "all selectors must hold", sel: AppliesTo{Variant: []Variant{VariantFDE}, Arch: []string{"arm64"}}, target: Target{Variant: VariantFDE, Arch: "amd64"}, want: false},
		{name: "ubuntu match", sel: AppliesTo{Ubuntu: []string{"24.04.5", "26.04.1"}}, target: Target{Ubuntu: "26.04.1"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.sel.Matches(tt.target))
		})
	}
}

func TestEngineSkipReasons(t *testing.T) {
	reg := &Registry{Version: "t", Checks: []CheckDef{
		{
			ID: "NA-001", Severity: SeverityBlocking, Method: MethodStatic, Verifies: "v", Why: "w",
			AppliesTo: AppliesTo{Variant: []Variant{VariantFDE}},
		},
		{ID: "GF-001", Severity: SeverityBlocking, Method: MethodGuestfs, Verifies: "v", Why: "w"},
		{ID: "BT-001", Severity: SeverityBlocking, Method: MethodBoot, Verifies: "v", Why: "w"},
		{ID: "IN-001", Severity: SeverityBlocking, Method: MethodStatic, Verifies: "v", Why: "w"},
		{ID: "UX-001", Severity: SeverityBlocking, Method: MethodStatic, Verifies: "v", Why: "w"},
	}}
	impls := map[string]Func{
		"NA-001": func(context.Context, Target, Inputs) (Outcome, error) { return Outcome{Status: StatusPass}, nil },
		"IN-001": func(context.Context, Target, Inputs) (Outcome, error) {
			return Outcome{Status: StatusSkip, Reason: SkipInputMissing, Evidence: "no BOM"}, nil
		},
		"UX-001": func(context.Context, Target, Inputs) (Outcome, error) {
			return Outcome{Status: StatusSkip, Evidence: "shrug"}, nil // no reason given
		},
	}
	results := NewEngine(impls).Run(context.Background(), reg, Target{Variant: VariantNonFDE}, Inputs{})
	byID := map[string]Result{}
	for _, r := range results {
		byID[r.ID] = r
	}

	assert.Equal(t, StatusSkip, byID["NA-001"].Status)
	assert.Equal(t, SkipNotApplicable, byID["NA-001"].Reason, "appliesTo exclusion is genuine non-applicability")
	assert.Equal(t, StatusSkip, byID["GF-001"].Status)
	assert.Equal(t, SkipTierOff, byID["GF-001"].Reason)
	assert.Equal(t, StatusSkip, byID["BT-001"].Status)
	assert.Equal(t, SkipTierOff, byID["BT-001"].Reason)
	assert.Equal(t, StatusSkip, byID["IN-001"].Status)
	assert.Equal(t, SkipInputMissing, byID["IN-001"].Reason)
	assert.Equal(t, StatusError, byID["UX-001"].Status,
		"a skip with no stated reason cannot be policy-evaluated and must surface as an error")
	assert.Contains(t, byID["UX-001"].Evidence, "without a reason")
}

// TestEngineRejectsOutOfVocabularyOutcomes pins the vocabulary boundary: an
// implementation returning a status or skip reason outside the enums yields
// StatusError naming the bogus word, never a copied-through result that a
// consumer's exact-match policy would misread (or a signed verdict would
// launder into "not a fail").
func TestEngineRejectsOutOfVocabularyOutcomes(t *testing.T) {
	reg := testRegistry(t, twoChecks)
	bogusStatus := func(context.Context, Target, Inputs) (Outcome, error) {
		return Outcome{Status: Status("passed"), Evidence: "typo'd vocabulary"}, nil
	}
	bogusReason := func(context.Context, Target, Inputs) (Outcome, error) {
		return Outcome{Status: StatusSkip, Reason: SkipReason("not-applicable-ish"), Evidence: "invented taxonomy"}, nil
	}
	pass := func(context.Context, Target, Inputs) (Outcome, error) {
		return Outcome{Status: StatusPass, Evidence: "ok"}, nil
	}

	results := NewEngine(map[string]Func{"T-001": bogusStatus, "T-002": pass}).Run(context.Background(), reg, Target{Variant: VariantFDE}, Inputs{})
	require.Len(t, results, 2)
	assert.Equal(t, StatusError, results[0].Status)
	assert.Contains(t, results[0].Evidence, `unknown status "passed"`)

	results = NewEngine(map[string]Func{"T-001": bogusReason, "T-002": pass}).Run(context.Background(), reg, Target{Variant: VariantFDE}, Inputs{})
	require.Len(t, results, 2)
	assert.Equal(t, StatusError, results[0].Status)
	assert.Empty(t, results[0].Reason, "a rejected outcome must not carry the invented reason forward")
	assert.Contains(t, results[0].Evidence, `unknown reason "not-applicable-ish"`)
}
