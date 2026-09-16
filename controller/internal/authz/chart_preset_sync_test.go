// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The chart's "standard" preset (deploy/charts/controller/files/authz/presets/
// standard/{policies.cedar,roles.json}) is a hand-copy of the shipped bundle
// (deploy/authz/{policies.cedar,roles.json}), not derived from it by any
// generate step or template lookup -- unlike actions.json/schema.json, which
// hack/gen-authz-actions now writes to both places from one pass (see its
// -chart-bundle flag), policies.cedar and roles.json have no generator: they
// are deployment-specific content that this one preset happens to mirror.
//
// Nothing else catches the two drifting: deploy/authz/policies.cedar can gain
// a new rule and the packaged "standard" preset silently falls behind, still
// rendering clean, still passing every other check, just no longer matching
// what its own values.yaml/README comment claims it does. This is a plain
// content-equality check, not a semantic one -- it exists to fail loudly the
// day someone edits one copy and forgets the other, not to say anything about
// whether either copy is itself correct.
func chartPresetStandardDir(tb testing.TB) string {
	tb.Helper()
	dir := filepath.Join("..", "..", "deploy", "charts", "controller", "files", "authz", "presets", "standard")
	_, err := os.Stat(filepath.Join(dir, policiesFile))
	require.NoErrorf(tb, err, "the chart's standard preset must be readable at %s for this test to assert anything", dir)
	return dir
}

func TestChartStandardPresetMatchesTheShippedBundle(t *testing.T) {
	shippedDir := bundleDir(t)
	presetDir := chartPresetStandardDir(t)

	for _, name := range []string{policiesFile, rolesFile} {
		shipped, err := os.ReadFile(filepath.Join(shippedDir, name))
		require.NoError(t, err)
		preset, err := os.ReadFile(filepath.Join(presetDir, name))
		require.NoError(t, err)

		assert.Equalf(t, string(shipped), string(preset),
			"deploy/charts/controller/files/authz/presets/standard/%s has drifted from deploy/authz/%s -- "+
				"the chart's \"standard\" preset is a hand-copy with no generator behind it; "+
				"re-copy deploy/authz/%s over the preset file", name, name, name)
	}
}

// chartBundleFilesDir is the chart's copy of the *generated* bundle half --
// hack/gen-authz-actions's -chart-bundle output, written by the same
// `go generate` pass as deploy/authz/{actions.json,schema.json} (see main.go's
// run(), and -chart-bundle's own comment). Unlike the "standard" preset
// above, this has a generator behind it, but gen-authz-actions writes the
// canonical copy and the chart copy via two separate, non-transactional
// os.WriteFile calls -- a failure between them (disk full, permission error,
// interrupted process) can leave the two out of sync with nothing to catch
// it, since this test is the only thing that compares them.
func chartBundleFilesDir(tb testing.TB) string {
	tb.Helper()
	dir := filepath.Join("..", "..", "deploy", "charts", "controller", "files", "authz")
	_, err := os.Stat(filepath.Join(dir, actionsFile))
	require.NoErrorf(tb, err, "the chart's packaged bundle files must be readable at %s for this test to assert anything", dir)
	return dir
}

// TestChartBundleFilesMatchTheGeneratedBundle proves deploy/authz/
// {actions.json,schema.json} (the canonical generated output) and
// deploy/charts/controller/files/authz/{actions.json,schema.json} (the
// chart-packaged copy of the same generation pass) are byte-identical.
// Nothing else in this repo's test suite compares these two specific files
// -- TestChartStandardPresetMatchesTheShippedBundle above only covers the
// hand-copied preset's policies.cedar/roles.json, not these generator
// outputs.
func TestChartBundleFilesMatchTheGeneratedBundle(t *testing.T) {
	canonicalDir := bundleDir(t)
	chartDir := chartBundleFilesDir(t)

	for _, name := range []string{actionsFile, schemaFile} {
		canonical, err := os.ReadFile(filepath.Join(canonicalDir, name))
		require.NoError(t, err)
		chart, err := os.ReadFile(filepath.Join(chartDir, name))
		require.NoError(t, err)

		assert.Equalf(t, string(canonical), string(chart),
			"deploy/charts/controller/files/authz/%s has drifted from deploy/authz/%s -- both are written by "+
				"the same `go generate ./internal/authz/` pass (hack/gen-authz-actions's -bundle/-chart-bundle "+
				"flags); re-run go generate to bring them back in sync", name, name)
	}
}
