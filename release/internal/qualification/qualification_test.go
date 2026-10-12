// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/release/internal/resolve"
)

// repoRoot is where the real files live, relative to this package.
const repoRoot = "../../.."

// write puts a file in a temporary directory and returns its path.
func write(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantError fails unless err mentions want.
func wantError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not mention %q", err, want)
	}
}

// TestRepositoryFilesAreValid is the check that earns this package its place in
// the merge gate. The three files are configuration that nothing compiles, so
// without this a typo in a capability name, a component name or a strategy
// reaches CI as a scope that silently selects nothing or a plan that cannot
// resolve.
func TestRepositoryFilesAreValid(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(repoRoot, "release", "policy.yaml"))
	if err != nil {
		t.Fatalf("release/policy.yaml: %v", err)
	}
	r, err := LoadRegistry(filepath.Join(repoRoot, "release", "registry.yaml"))
	if err != nil {
		t.Fatalf("release/registry.yaml: %v", err)
	}
	s, err := LoadSubject(SubjectPath(repoRoot, SubjectNVKE))
	if err != nil {
		t.Fatalf("release/qualification.yaml: %v", err)
	}

	// Every registry entry has to build, which holds its strategy to the set
	// that exists and its configuration to the keys that strategy takes.
	resolvers := map[string]resolve.Resolver{}
	for _, c := range r.Spec.Components {
		built, err := resolve.New(c.Resolve.Strategy, r.StrategyConfig(c), repoRoot)
		if err != nil {
			t.Errorf("registry component %s: %v", c.Name, err)
			continue
		}
		resolvers[c.Name] = built
	}

	// What the subject declares has to suit each strategy it is declared for.
	if err := Check(p, r, s, resolvers); err != nil {
		t.Error(err)
	}
}
