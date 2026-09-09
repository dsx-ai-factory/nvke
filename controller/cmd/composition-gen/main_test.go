// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestCompositionUpToDate fails when generated/nke-stack has drifted from the
// recipe under deploy/composition.
//
// This lives in a test rather than in `make generate-check` because that check
// runs in CI under controller/api's pinned Go with GOTOOLCHAIN=local, and this
// module's go.mod asks for a newer toolchain than the api module's -- so
// invoking the generator there fails with "go.mod requires go >= 1.26.6"
// before it reads a single file. `make test` builds this module properly, so
// the drift is caught there instead.
func TestCompositionUpToDate(t *testing.T) {
	// Paths in main() are relative to the module root, but `go test` runs with
	// the package directory as the working directory. Derive the module root
	// from this file instead of assuming either.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine the path of this source file")
	}
	moduleRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	recipeDir := filepath.Join(moduleRoot, "deploy", "composition")
	outDir := filepath.Join(moduleRoot, "..", "generated", "nke-stack")

	if err := run(recipeDir, "nke-stack.yaml", outDir, true); err != nil {
		t.Fatalf("the committed chart does not match the recipe; run `make composition`: %v", err)
	}
}
