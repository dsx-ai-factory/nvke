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

// chartBundleFilesDir is the chart's copy of the *generated* bundle half --
// hack/gen-authz-actions's -chart-bundle output, written by the same
// `go generate` pass as deploy/authz/{actions.json,schema.json} (see main.go's
// run(), and -chart-bundle's own comment). Unlike
// policies.cedar/roles.json (hand-copied -- see
// TestChartDefaultBundleMatchesDeployAuthz below), this
// pair has a generator behind it, but gen-authz-actions writes the canonical copy and the chart copy via two
// separate, non-transactional os.WriteFile calls -- a failure between them
// (disk full, permission error, interrupted process) can leave the two out
// of sync with nothing to catch it, since this test is the only thing that
// compares them.
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
// Nothing else in this repo's test suite compares these two specific files.
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

// TestChartDefaultBundleMatchesDeployAuthz proves the chart's default bundle,
// files/authz/default/{policies.cedar,roles.json}, is byte-identical to
// deploy/authz/. Neither ships roleMappings.json: the chart mounts it from the
// operator's ConfigMap. It is a copy rather than a symlink because
// the chart is also embedded in the nkectl binary (controller/deploy/charts),
// and go:embed does not follow symlinks: a symlinked default bundle is missing
// from the embedded chart, whose render then fails with "resolved no content".
func TestChartDefaultBundleMatchesDeployAuthz(t *testing.T) {
	canonicalDir := bundleDir(t)
	chartDir := filepath.Join(chartBundleFilesDir(t), "default")

	for _, name := range []string{policiesFile, rolesFile} {
		canonical, err := os.ReadFile(filepath.Join(canonicalDir, name))
		require.NoError(t, err)
		chart, err := os.ReadFile(filepath.Join(chartDir, name))
		require.NoError(t, err)

		assert.Equalf(t, string(canonical), string(chart),
			"deploy/charts/controller/files/authz/default/%s has drifted from deploy/authz/%s; "+
				"copy deploy/authz/%s over it", name, name, name)
	}
}

// The errors that say where a grant belongs name the chart's default
// roleMappings ConfigMap, so that name and the chart's must agree.
func TestRoleMappingsSourceNamesTheChartDefault(t *testing.T) {
	helpers, err := os.ReadFile(filepath.Join("..", "..", "deploy", "charts", "controller", "templates", "_helpers.tpl"))
	require.NoError(t, err)
	assert.Contains(t, string(helpers), `default "`+defaultRoleMappingsConfigMap+`"`,
		"_helpers.tpl's default for apiAuthz.roleMappingsConfigMap must be defaultRoleMappingsConfigMap (bundle.go)")
	assert.Contains(t, roleMappingsSource, defaultRoleMappingsConfigMap)
}
