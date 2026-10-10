// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluate(t *testing.T) {
	removal := change{
		Breaking:   true,
		ChangeText: "property_removed",
		Path:       "$.components.schemas['Cluster']",
		Property:   "required",
		Original:   "upgrading",
	}
	addition := change{
		ChangeText: "object_added",
		Path:       "$.components.schemas['Cluster']",
		Property:   "lastUpgradedAt",
	}
	waiverFor := func(c change) waiver {
		return waiver{
			Change:   c.ChangeText,
			Path:     c.Path,
			Property: c.Property,
			Reason:   "the field never shipped",
			Owner:    "Terry Howe",
			Review:   "2027-01-01",
		}
	}

	tests := []struct {
		name     string
		changes  []change
		waivers  []waiver
		unwaived int
		waived   int
		unused   int
	}{
		{
			name:    "no changes at all passes",
			changes: nil,
		},
		{
			name:    "additive changes alone pass",
			changes: []change{addition},
		},
		{
			name:     "an unwaived breaking change fails",
			changes:  []change{addition, removal},
			unwaived: 1,
		},
		{
			name:    "an exactly matching waiver clears it",
			changes: []change{addition, removal},
			waivers: []waiver{waiverFor(removal)},
			waived:  1,
		},
		{
			name:     "a waiver on a different path does not match",
			changes:  []change{removal},
			waivers:  []waiver{func() waiver { w := waiverFor(removal); w.Path = "$.components.schemas['Node']"; return w }()},
			unwaived: 1,
			unused:   1,
		},
		{
			name:     "a waiver on a different change kind does not match",
			changes:  []change{removal},
			waivers:  []waiver{func() waiver { w := waiverFor(removal); w.Change = "object_removed"; return w }()},
			unwaived: 1,
			unused:   1,
		},
		{
			name:     "a waiver on a different property does not match",
			changes:  []change{removal},
			waivers:  []waiver{func() waiver { w := waiverFor(removal); w.Property = "enum"; return w }()},
			unwaived: 1,
			unused:   1,
		},
		{
			// A waiver that no longer matches anything has outlived its reason.
			// It fails even though nothing is breaking, so stale entries cannot
			// accumulate silently.
			name:    "a waiver matching nothing fails a clean diff",
			changes: []change{addition},
			waivers: []waiver{waiverFor(removal)},
			unused:  1,
		},
		{
			// A waiver never matches a change the engine did not call
			// breaking, so it is reported as unused rather than quietly
			// consumed.
			name:    "a waiver cannot be spent on an additive change",
			changes: []change{addition},
			waivers: []waiver{waiverFor(addition)},
			unused:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluate(tt.changes, tt.waivers, "controller/api/v0.0.28")
			assert.Len(t, got.Unwaived, tt.unwaived, "unwaived")
			assert.Len(t, got.Waived, tt.waived, "waived")
			assert.Len(t, got.Unused, tt.unused, "unused")
		})
	}
}

// A waiver that matches nothing fails, unless the baseline being compared
// against is a later release than the one it names: the tag moved past it, so
// the break has shipped and the entry is spent rather than wrong. Only that one
// relation expires it -- a baseline is caller-supplied, and anything that
// cannot be ordered against a published release tag must keep failing.
func TestEvaluateExpiresWaiversWrittenAgainstAnOlderBaseline(t *testing.T) {
	const current = "controller/api/v0.0.28"
	tests := []struct {
		name     string
		baseline string
		unused   int
		expired  int
	}{
		{name: "an older baseline expires", baseline: "controller/api/v0.0.22", expired: 1},
		{name: "ordering is numeric, not lexical", baseline: "controller/api/v0.0.9", expired: 1},
		{name: "the current baseline still fails", baseline: current, unused: 1},
		{name: "no recorded baseline still fails", baseline: "", unused: 1},
		{name: "a later baseline still fails", baseline: "controller/api/v0.0.29", unused: 1},
		{name: "a prerelease does not order", baseline: "controller/api/v0.0.22-rc1", unused: 1},
		{name: "a commit sha does not order", baseline: "9c7c2d09a8c2180b0d33a38b0809b0132f68d665", unused: 1},
		{name: "a fully-qualified ref does not order", baseline: "refs/tags/controller/api/v0.0.22", unused: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := waiver{Change: "property_removed", Path: "$.x", Baseline: tt.baseline}
			got := evaluate(nil, []waiver{w}, current)
			assert.Len(t, got.Unused, tt.unused, "unused")
			assert.Len(t, got.Expired, tt.expired, "expired")
		})
	}
}

// Checking against an older release is a supported mode (--baseline,
// API_COMPAT_BASELINE). It must not expire the waivers written for the current
// one: that is the run a reviewer reaches for, and it is the wrong run in which
// to stop enforcing the matches-nothing rule.
func TestEvaluateDoesNotExpireWaiversWhenCheckingAgainstAnOlderRelease(t *testing.T) {
	w := waiver{Change: "property_removed", Path: "$.x", Baseline: "controller/api/v0.0.28"}
	got := evaluate(nil, []waiver{w}, "controller/api/v0.0.22")
	assert.Len(t, got.Unused, 1)
	assert.Empty(t, got.Expired)
}

// An expired waiver is only expired when it matches nothing. One that still
// covers a break is used, whatever baseline it was written against.
func TestEvaluateKeepsAMatchingWaiverRegardlessOfBaseline(t *testing.T) {
	c := change{Breaking: true, ChangeText: "property_removed", Path: "$.x"}
	w := waiver{Change: c.ChangeText, Path: c.Path, Baseline: "controller/api/v0.0.22"}
	got := evaluate([]change{c}, []waiver{w}, "controller/api/v0.0.28")
	assert.Len(t, got.Waived, 1)
	assert.Empty(t, got.Expired)
	assert.Empty(t, got.Unused)
}

func TestParseWaivers(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    int
		wantErr string
	}{
		{
			name: "an empty list is valid",
			yaml: "waivers: []\n",
		},
		{
			name: "comments only are valid",
			yaml: "# nothing waived\n",
		},
		{
			name: "a complete waiver parses",
			yaml: `waivers:
  - change: property_removed
    path: $.components.schemas['Cluster']
    property: required
    reason: the field never shipped
    owner: Terry Howe
    review: "2027-01-01"
`,
			want: 1,
		},
		{
			name: "an empty property is allowed",
			yaml: `waivers:
  - change: object_removed
    path: $.components
    property: ""
    reason: the field never shipped
    owner: Terry Howe
    review: "2027-01-01"
`,
			want: 1,
		},
		{
			name: "a missing owner is rejected",
			yaml: `waivers:
  - change: property_removed
    path: $.components
    reason: the field never shipped
    review: "2027-01-01"
`,
			wantErr: "owner is required",
		},
		{
			name: "a missing reason is rejected",
			yaml: `waivers:
  - change: property_removed
    path: $.components
    owner: Terry Howe
    review: "2027-01-01"
`,
			wantErr: "reason is required",
		},
		{
			name: "a non-date review is rejected",
			yaml: `waivers:
  - change: property_removed
    path: $.components
    reason: the field never shipped
    owner: Terry Howe
    review: soon
`,
			wantErr: "is not a YYYY-MM-DD date",
		},
		{
			// A misspelled key would otherwise make the waiver a silent no-op,
			// failing the gate with no hint as to why.
			name: "an unknown key is rejected",
			yaml: `waivers:
  - change: property_removed
    pathh: $.components
    reason: the field never shipped
    owner: Terry Howe
    review: "2027-01-01"
`,
			wantErr: "field pathh not found",
		},
		{
			name:    "malformed yaml is rejected",
			yaml:    "waivers: [\n",
			wantErr: "parsing the waiver file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseWaivers([]byte(tt.yaml))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, got, tt.want)
		})
	}
}

// The committed waiver file is the one the gate actually reads. A typo in it
// would only surface on the next spec change, which is the worst possible time.
func TestCommittedWaiverFileIsValid(t *testing.T) {
	waivers, err := loadWaivers("../../api/openapi-compat-waivers.yaml")
	require.NoError(t, err)
	for _, w := range waivers {
		assert.NoError(t, w.validate(), "waiver %q", w.Path)
	}
}

// A missing waiver file means the checkout or the path is wrong. Reading it as
// "nothing is waived" would let the gate pass for the wrong reason.
func TestLoadWaiversFailsOnAMissingFile(t *testing.T) {
	_, err := loadWaivers("testdata/does-not-exist.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading the waiver file")
}
