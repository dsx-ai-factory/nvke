// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// now is fixed, so a test cannot start passing or failing because of the date it
// is run on. Every sunset below is expressed relative to it.
var now = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// spec builds a minimal document with one path and the operation fields under
// test. Only `paths` is parsed, so the rest of an OpenAPI document would be
// noise here.
const testPath = "/a"

func spec(ops map[string]string) []byte {
	out := "openapi: 3.0.3\npaths:\n  " + testPath + ":\n"
	for method, body := range ops {
		out += "    " + method + ":\n      operationId: op\n"
		if body != "" {
			out += body
		}
	}
	return []byte(out)
}

func deprecated(sunset string) string {
	if sunset == "" {
		return "      deprecated: true\n"
	}
	return fmt.Sprintf("      deprecated: true\n      x-sunset: %q\n", sunset)
}

// The rules are scoped to what changed, not to the state of the document. A
// pull request that does not touch the specification must not fail because some
// deprecation that predates it is out of compliance -- a gate that fails work
// unrelated to it is a gate someone switches off.
func TestUnchangedDeprecationsAreNotReExamined(t *testing.T) {
	both := spec(map[string]string{"get": deprecated("")})

	result, err := evaluateLifecycle(both, both, now, 180)
	require.NoError(t, err)
	assert.Empty(t, result.Findings, "a pre-existing non-compliant deprecation is not this change's problem")
}

func TestDeprecationMustBeReal(t *testing.T) {
	base := spec(map[string]string{"get": ""})

	tests := []struct {
		name     string
		revision string
		want     string
	}{
		{
			name:     "deprecated with no sunset announces nothing actionable",
			revision: deprecated(""),
			want:     changeDeprecatedWithoutSunset,
		},
		{
			name:     "a sunset without deprecated reaches no client",
			revision: "      x-sunset: \"2027-06-01\"\n",
			want:     changeSunsetWithoutDeprecated,
		},
		{
			name:     "a sunset that is not a date cannot be honored",
			revision: deprecated("next spring"),
			want:     changeSunsetNotADate,
		},
		{
			name:     "a fortnight is not notice",
			revision: deprecated("2026-06-14"),
			want:     changeSunsetTooSoon,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := evaluateLifecycle(base, spec(map[string]string{"get": tt.revision}), now, 180)
			require.NoError(t, err)
			require.Len(t, result.Findings, 1)
			assert.Equal(t, tt.want, result.Findings[0].ChangeText)
			assert.True(t, result.Findings[0].Breaking, "a finding that is not breaking would not fail the gate")
			assert.Equal(t, "$.paths['/a'].get", result.Findings[0].Path)
		})
	}
}

func TestAFullNoticePeriodIsAccepted(t *testing.T) {
	base := spec(map[string]string{"get": ""})
	revision := spec(map[string]string{"get": deprecated("2026-12-01")}) // 183 days

	result, err := evaluateLifecycle(base, revision, now, 180)
	require.NoError(t, err)
	assert.Empty(t, result.Findings)
}

// The point of the whole file: a removal that followed the process costs no
// waiver. Without this, deprecating first and removing abruptly cost a reviewer
// exactly the same, and the patient route has nothing to recommend it.
func TestARemovalPastItsSunsetNeedsNoWaiver(t *testing.T) {
	base := spec(map[string]string{"get": deprecated("2026-01-01")})
	revision := []byte("openapi: 3.0.3\npaths: {}\n")

	result, err := evaluateLifecycle(base, revision, now, 180)
	require.NoError(t, err)
	assert.True(t, result.SanctionedOps["/a GET"])
	assert.True(t, result.SanctionedPaths["/a"], "every operation of the path went, so the path item went")

	kept, discharged := applyLifecycle([]change{
		{Breaking: true, ChangeText: "object_removed", Path: "$.paths", Property: "/a"},
	}, result)
	assert.Empty(t, kept)
	require.Len(t, discharged, 1)
}

func TestARemovalBeforeItsSunsetStillFails(t *testing.T) {
	base := spec(map[string]string{"get": deprecated("2027-01-01")})
	revision := []byte("openapi: 3.0.3\npaths: {}\n")

	result, err := evaluateLifecycle(base, revision, now, 180)
	require.NoError(t, err)
	assert.Empty(t, result.SanctionedOps)

	removal := change{Breaking: true, ChangeText: "object_removed", Path: "$.paths", Property: "/a"}
	kept, discharged := applyLifecycle([]change{removal}, result)
	assert.Equal(t, []change{removal}, kept, "it is still breaking, and still waivable")
	assert.Empty(t, discharged)
}

func TestARemovalThatWasNeverDeprecatedStillFails(t *testing.T) {
	base := spec(map[string]string{"get": ""})
	revision := []byte("openapi: 3.0.3\npaths: {}\n")

	result, err := evaluateLifecycle(base, revision, now, 180)
	require.NoError(t, err)
	assert.Empty(t, result.SanctionedOps)
}

// A path that loses one of its operations is reported differently from a path
// that disappears whole, so the two are discharged differently. Getting this
// wrong in the permissive direction would sanction the surviving operations too.
func TestOneOperationOfAPathIsDischargedOnItsOwn(t *testing.T) {
	base := spec(map[string]string{"get": deprecated("2026-01-01"), "post": ""})
	revision := spec(map[string]string{"post": ""})

	result, err := evaluateLifecycle(base, revision, now, 180)
	require.NoError(t, err)
	assert.True(t, result.SanctionedOps["/a GET"])
	assert.False(t, result.SanctionedPaths["/a"], "the path survives, so removing it whole is not sanctioned")

	kept, discharged := applyLifecycle([]change{
		{Breaking: true, ChangeText: "property_removed", Path: "$.paths['/a']", Property: "get"},
		{Breaking: true, ChangeText: "object_removed", Path: "$.paths", Property: "/a"},
	}, result)
	require.Len(t, discharged, 1)
	assert.Equal(t, "property_removed", discharged[0].ChangeText)
	require.Len(t, kept, 1)
	assert.Equal(t, "object_removed", kept[0].ChangeText)
}

// Discharge is for removals and nothing else. A narrowed type inside an
// operation whose sunset happens to have passed is a separate break.
func TestDischargeDoesNotReachOtherChangesUnderTheSamePath(t *testing.T) {
	result := lifecycleResult{
		SanctionedOps:   map[string]bool{"/a GET": true},
		SanctionedPaths: map[string]bool{"/a": true},
	}
	other := change{Breaking: true, ChangeText: "property_removed", Path: "$.paths['/a'].get.responses", Property: "200"}

	kept, discharged := applyLifecycle([]change{other}, result)
	assert.Equal(t, []change{other}, kept)
	assert.Empty(t, discharged)
}

// Additive changes are carried through untouched: discharge decides which
// breaking changes stop counting, and must not quietly drop anything else.
func TestAdditiveChangesAreLeftAlone(t *testing.T) {
	additive := change{ChangeText: "object_added", Path: "$.paths", Property: "/b"}

	kept, discharged := applyLifecycle([]change{additive}, lifecycleResult{})
	assert.Equal(t, []change{additive}, kept)
	assert.Empty(t, discharged)
}

// A path item holds keys that are not operations. Decoding `parameters`, which
// is a sequence, as an operation fails -- and this specification has them.
func TestNonOperationKeysOfAPathItemAreSkipped(t *testing.T) {
	doc := []byte("openapi: 3.0.3\npaths:\n  /a:\n    summary: a path\n    parameters:\n      - name: id\n        in: query\n    get:\n      deprecated: true\n")

	ops, err := parseOperations(doc, "test")
	require.NoError(t, err)
	assert.Len(t, ops, 1)
	assert.True(t, ops["/a GET"].Deprecated)
}

func TestPathFromJSONPath(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{in: "$.paths['/clusters']", want: "/clusters", wantOK: true},
		{in: "$.paths['/clusters/{clusterID}']", want: "/clusters/{clusterID}", wantOK: true},
		{in: "$.paths['/clusters'].get", wantOK: false},
		{in: "$.paths", wantOK: false},
		{in: "$.components.schemas['Cluster']", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := pathFromJSONPath(tt.in)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// The real specification has to parse, or every rule here is theory.
func TestTheShippedSpecificationParses(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	require.NoError(t, err)

	ops, err := parseOperations(raw, "working-tree")
	require.NoError(t, err)
	assert.NotEmpty(t, ops)

	// A named operation, so the assertion proves the paths were really walked
	// rather than that some non-empty map came back. /clusters GET is the one
	// operation this API cannot lose while remaining this API -- an endpoint
	// picked for being interesting instead goes away on its own schedule and
	// takes this test with it, which is how the previous sentinel
	// (/cniPlugins GET) broke when #1241 retired the CNI plugin catalog.
	assert.Contains(t, ops, "/clusters GET")
}
