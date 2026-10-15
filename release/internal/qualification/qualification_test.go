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
	// The registry names the org its artifacts sit under rather than stating
	// it, so loading it needs one the way a run does.
	t.Setenv("QUALIFY_E2E_NGC_ORG", "an-org")

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

	// Every half of every registry entry has to build, which holds its strategy
	// to the half it is written in, to the set that exists and to the keys that
	// strategy takes.
	version := map[string]resolve.Resolver{}
	location := map[string]resolve.Resolver{}
	for _, c := range r.Spec.Components {
		if v := c.Resolve.Version; v != nil && v.Strategy != "" {
			built, err := resolve.NewVersion(v.Strategy, r.StrategyConfig(v), repoRoot)
			if err != nil {
				t.Errorf("registry component %s, version: %v", c.Name, err)
			} else {
				version[c.Name] = built
			}
		}
		if l := c.Resolve.Location; l != nil {
			built, err := resolve.NewLocation(l.Strategy, r.StrategyConfig(l), resolve.Options{})
			if err != nil {
				t.Errorf("registry component %s, location: %v", c.Name, err)
			} else {
				location[c.Name] = built
			}
		}
	}

	// What the subject declares has to suit each strategy it is declared for.
	if err := Check(p, r, s, version, location); err != nil {
		t.Error(err)
	}
}

// An identifier the file names but the run does not supply fails at the file
// that asked for it, with every missing name at once, rather than later
// against a registry that was asked for an artifact under no org.
func TestExpandRequiresEveryName(t *testing.T) {
	t.Setenv("QUALIFY_TEST_SET", "a-value")
	t.Setenv("QUALIFY_TEST_EMPTY", "")

	got, err := expand([]byte("base: oci://registry.example/${QUALIFY_TEST_SET}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "base: oci://registry.example/a-value\n"; string(got) != want {
		t.Errorf("= %q, want %q", got, want)
	}

	// Set but empty counts as unset: an unset repository variable reaches a
	// workflow as the empty string rather than as absent.
	_, err = expand([]byte("a: ${QUALIFY_TEST_EMPTY}\nb: ${QUALIFY_TEST_MISSING}\n"))
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	for _, want := range []string{"QUALIFY_TEST_EMPTY", "QUALIFY_TEST_MISSING"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

// A scope decides which streams a run covers, and the stage decides how far
// each goes. They are independent everywhere except here: a dispatch defaults
// to stage full, and the e2e suite refuses a full run on a provider whose
// tenant it cannot build. So a scope that reaches such a provider at that
// stage schedules a lane that can only fail, whatever the versions under test.
//
// Forge is the only provider with a tenant spec today, so `full` selects on it.
// This test is what stops the selector being dropped as redundant: it looks
// redundant exactly until a second provider is added.
func TestFullScopeReachesOnlyProvidersAFullRunCanFinish(t *testing.T) {
	t.Setenv("QUALIFY_E2E_NGC_ORG", "an-org")

	p, err := LoadPolicy(filepath.Join(repoRoot, "release", "policy.yaml"))
	if err != nil {
		t.Fatalf("release/policy.yaml: %v", err)
	}

	full, ok := p.Spec.Scopes["full"]
	if !ok {
		t.Fatal("release/policy.yaml declares no full scope")
	}

	var matched int
	for _, stream := range p.Spec.Streams {
		if !full.Matches(stream) {
			continue
		}
		matched++
		if got := stream.Capabilities["provider"]; got != "forge" {
			t.Errorf("stream %q (provider %q) is in scope full, but a full run on it is refused "+
				"before a machine is taken; give it a tenant spec or keep it out of this scope",
				stream.Name, got)
		}
	}
	if matched == 0 {
		t.Error("scope full matched no stream at all, which cannot be right")
	}
}
