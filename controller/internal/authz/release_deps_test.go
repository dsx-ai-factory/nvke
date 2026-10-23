// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Keeping the experimental validator out of the shipped binary.
//
// policy_validation_test.go type-checks the bundle with
// cedar-go/x/exp/schema/validate, which is experimental API the project may
// break or withdraw. That is an acceptable dependency for a test and not for
// the thing running in a cluster, so the split is load-bearing: an import added
// to a non-test file would pull it into cmd/worker without anything failing.
//
// The dependency graph is the only place that distinction is visible, so this
// reads it. `go list -deps` reports what the package actually links, so it
// catches an indirect path too -- a helper package importing the validator on
// behalf of the engine looks the same to this test as importing it directly.

// The experimental import path. Both the validator and the schema types under
// it are covered, since neither belongs in a release build.
const experimentalSchemaPkg = "github.com/cedar-policy/cedar-go/x/exp/schema"

// The release binary, as controller/Makefile's `build` target names it.
const releaseBinaryPkg = "./cmd/worker"

func TestReleaseBinaryExcludesExperimentalValidator(t *testing.T) {
	// Not a skip: `go test` ran this binary, so a toolchain was on PATH a moment
	// ago. A test that quietly stops asserting when its tool goes missing is the
	// same shape as the one this file exists to catch.
	goBin, err := exec.LookPath("go")
	require.NoError(t, err, "go test ran without a go toolchain on PATH")

	// From the module root, so the pattern above is the one the Makefile uses
	// and dependencies resolve the same way they do for a
	// build.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	cmd := exec.CommandContext(t.Context(), goBin, "list", "-deps", releaseBinaryPkg)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "go list -deps %s: %s", releaseBinaryPkg, out)

	deps := strings.Fields(string(out))

	// A `go list` that resolved nothing would leave the loop below with nothing
	// to reject, so the listing is checked for two packages that must be there
	// before it is trusted. Cedar itself is the point: the engine ships, only
	// the validator does not.
	require.Contains(t, deps, "github.com/dsx-ai-factory/nvke/controller/internal/authz",
		"go list did not report the authz package; the listing is not the release binary's")
	require.Contains(t, deps, "github.com/cedar-policy/cedar-go",
		"go list did not report cedar-go; the listing is not the release binary's")

	for _, dep := range deps {
		if dep == experimentalSchemaPkg || strings.HasPrefix(dep, experimentalSchemaPkg+"/") {
			t.Errorf("%s reaches %s; the experimental validator is test-only, see policy_validation_test.go",
				releaseBinaryPkg, dep)
		}
	}
}
