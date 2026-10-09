// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseReport(t *testing.T) {
	tests := []struct {
		name          string
		json          string
		wantChanges   int
		wantNoChanges bool
		wantErr       string
	}{
		{
			// The shape the engine writes when the two specifications are
			// equivalent. It has no changes array at all.
			name:          "the no-changes message is a pass",
			json:          `{"message": "No changes found between specifications"}`,
			wantNoChanges: true,
		},
		{
			name:        "a report with changes parses",
			json:        `{"reportSummary":{"paths":{"totalChanges":2,"breakingChanges":1}},"changes":[{"breaking":true,"changeText":"property_removed","path":"$.x","property":"required"},{"breaking":false,"changeText":"object_added","path":"$.y"}]}`,
			wantChanges: 2,
		},
		{
			name:        "an empty changes array parses",
			json:        `{"changes":[]}`,
			wantChanges: 0,
		},
		{
			// The engine writes nothing to stdout and the reason to stderr when
			// a specification will not parse. Reading that as zero breaking
			// changes is the exact failure this gate exists to prevent.
			name:    "an empty report is an error",
			json:    "   \n",
			wantErr: "empty report",
		},
		{
			name:    "truncated json is an error",
			json:    `{"changes":[{"breaking":true`,
			wantErr: "parsing the change engine report",
		},
		{
			name:    "a summary without changes is an error",
			json:    `{"reportSummary":{"paths":{"totalChanges":2,"breakingChanges":1}}}`,
			wantErr: "output format has moved",
		},
		{
			// A future rewording of the no-changes message must fail loudly
			// rather than be mistaken for an unrelated success.
			name:    "an unrecognized message is an error",
			json:    `{"message": "something else entirely"}`,
			wantErr: "unrecognized change engine report",
		},
		{
			name:    "an unrelated object is an error",
			json:    `{}`,
			wantErr: "unrecognized change engine report",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseReport([]byte(tt.json))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantNoChanges, got.NoChanges)
			assert.Len(t, got.Changes, tt.wantChanges)
		})
	}
}

func TestChangeDescribe(t *testing.T) {
	tests := []struct {
		name string
		c    change
		want string
	}{
		{
			name: "a removal names what was there",
			c:    change{ChangeText: "property_removed", Path: "$.x", Property: "required", Original: "upgrading"},
			want: `property_removed required at $.x (was "upgrading")`,
		},
		{
			name: "a modification names both sides",
			c:    change{ChangeText: "modified", Path: "$.x", Property: "type", Original: "string", New: "integer"},
			want: `modified type at $.x ("string" -> "integer")`,
		},
		{
			name: "a change with no property omits it",
			c:    change{ChangeText: "object_removed", Path: "$.x"},
			want: "object_removed at $.x",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.c.describe())
		})
	}
}

// The stub printed on failure has to be pasteable, so it must match on the same
// three fields loadWaivers validates and evaluate compares.
func TestWaiverStubMatchesTheChangeItDescribes(t *testing.T) {
	c := change{Breaking: true, ChangeText: "property_removed", Path: "$.components.schemas['Cluster']", Property: "required"}

	stub := c.waiverStub("controller/api/v0.0.28")
	require.NotEmpty(t, stub)
	assert.Contains(t, stub[0], `"property_removed"`)
	assert.Contains(t, stub[1], `"$.components.schemas['Cluster']"`)
	assert.Contains(t, stub[2], `"required"`)
	assert.Contains(t, stub[len(stub)-1], `"controller/api/v0.0.28"`)

	w := waiver{Change: c.ChangeText, Path: c.Path, Property: c.Property}
	assert.True(t, w.matches(c))
}

// --report has to go through the same parser as a live run, so a hand-supplied
// report cannot bypass the fail-closed checks. The request resolves both
// specifications first, which is why this one names a ref that has them.
func TestGatherReportRejectsAnUnreadableSuppliedReport(t *testing.T) {
	_, err := gatherReport(t.Context(), "testdata/does-not-exist.json", "", reportRequest{
		spec:       "../../api/openapi.yaml",
		specInRepo: defaultSpecInRepo,
		baseline:   "HEAD",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading report")
}

// --report supplies a report, never the documents. The deprecation rules read
// those directly, so a run that cannot reach them must fail rather than reach a
// verdict without them.
func TestGatherReportStillNeedsBothSpecifications(t *testing.T) {
	_, err := gatherReport(t.Context(), "testdata/does-not-exist.json", "", reportRequest{
		spec:       "testdata/no-such-openapi.yaml",
		specInRepo: defaultSpecInRepo,
		baseline:   "HEAD",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading the working-tree specification")
}

// Without a pinned engine version the tool would run whatever `go run` resolves,
// so the classification could change under a run that looks identical.
func TestProduceRequiresAPinnedEngineVersion(t *testing.T) {
	_, err := reportRequest{spec: "../../api/openapi.yaml"}.produce(t.Context(), specPair{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--differ-version is required")
}

// A tag pattern that matches nothing is a shallow clone, not a clean diff.
func TestNewestTagFailsWhenNothingMatches(t *testing.T) {
	_, err := newestTag(t.Context(), "controller/api/this-matches-no-tag-*")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no baseline to compare against")
}

// The glob is fnmatch, so `[0-9]*` lets `controller/api/v0.0.29-rc1` through,
// and `--sort=-version:refname` would rank it above the release it precedes.
// A prerelease baseline cannot be ordered by supersedes, so an older-baseline
// waiver would stay Unused and fail instead of expiring.
func TestNewestReleaseSkipsPrereleasesAndNonReleaseTags(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []string
		want string
	}{
		{
			name: "prerelease of a later version loses to the stable release",
			tags: []string{"controller/api/v0.0.29-rc1", "controller/api/v0.0.28"},
			want: "controller/api/v0.0.28",
		},
		{
			name: "ordering is numeric, not lexical",
			tags: []string{"controller/api/v0.0.9", "controller/api/v0.0.10"},
			want: "controller/api/v0.0.10",
		},
		{
			name: "build metadata is not a consumable release either",
			tags: []string{"controller/api/v0.1.0+build.5", "controller/api/v0.0.28"},
			want: "controller/api/v0.0.28",
		},
		{
			name: "tags outside the release prefix are ignored",
			tags: []string{"controller/v9.9.9", "  controller/api/v1.2.3  ", ""},
			want: "controller/api/v1.2.3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := newestRelease(tc.tags)
			require.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Matching only prereleases is the same situation as matching nothing: there is
// no baseline a break could have shipped in.
func TestNewestReleaseRejectsAPrereleaseOnlySet(t *testing.T) {
	_, ok := newestRelease([]string{"controller/api/v0.0.29-rc1", "controller/api/v0.0.30-rc2"})
	assert.False(t, ok)
}
