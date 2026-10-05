// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package environment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setSynthetic gives every ${NAME} an environment file references a value, so
// the structural checks run wherever this test does -- on a laptop, or in a
// pipeline that has no business holding one organization's identifiers. Read
// from the file rather than listed here, so adding a reference does not need a
// matching edit in the test that would be discovered by a failure.

// An unset reference has to fail here. Expanding it to an empty string would
// leave a file that still parses, so the run would reach Forge and ask it to
// create a cluster in org "", and the failure would name Forge's field rather
// than the variable nobody set.
func TestLoadRejectsUnsetReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gap.yaml")
	body := "provider: forge\ninfrastructure:\n  org:\n    id: \"${FORGE_ABSENT_ORG}\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	_, err := LoadPath("gap", path)
	if err == nil {
		t.Fatal("loading an environment with an unset reference should have failed")
	}
	if !strings.Contains(err.Error(), "FORGE_ABSENT_ORG") {
		t.Errorf("error should name the unset variable, got: %v", err)
	}
}

// A set-but-empty reference has to fail for the same reason an absent one
// does, and it is the shape CI actually produces: a workflow that writes
// `env: FORGE_ABSENT_ORG: ${{ vars.FORGE_ABSENT_ORG }}` for a repository
// variable nobody created sets the variable to "" rather than leaving it out.
// Checking presence alone would let exactly the run this package exists to
// prevent through, and only on the runner.
func TestLoadRejectsEmptyReference(t *testing.T) {
	t.Setenv("FORGE_ABSENT_ORG", "")

	path := filepath.Join(t.TempDir(), "empty.yaml")
	body := "provider: forge\ninfrastructure:\n  org:\n    id: \"${FORGE_ABSENT_ORG}\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	_, err := LoadPath("empty", path)
	if err == nil {
		t.Fatal("loading an environment with an empty reference should have failed")
	}
	if !strings.Contains(err.Error(), "FORGE_ABSENT_ORG") {
		t.Errorf("error should name the empty variable, got: %v", err)
	}
}

func TestExpandOptionalReference(t *testing.T) {
	t.Setenv("FORGE_SET_OPTIONAL", "jump.example")
	for _, tc := range []struct{ in, want string }{
		{`a: "${FORGE_ABSENT_OPTIONAL:-}"`, `a: ""`},
		{`a: "${FORGE_ABSENT_OPTIONAL:-root}"`, `a: "root"`},
		{`a: "${FORGE_SET_OPTIONAL:-}"`, `a: "jump.example"`},
	} {
		got, err := expand([]byte(tc.in))
		if err != nil {
			t.Errorf("expand(%s): %v", tc.in, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("expand(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestLoadRejectsPaths(t *testing.T) {
	for _, name := range []string{"", "../secrets", "forge/../../etc", "."} {
		if _, err := Load(name); err == nil {
			t.Errorf("Load(%q) should have failed", name)
		}
	}
}
