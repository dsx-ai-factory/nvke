// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs a git command in dir and fails the test if it fails.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=test", "-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
	}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// newRepo makes a repository with one commit, for tags to point at.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

// tag creates the tags on the repository's current commit.
func tag(t *testing.T, dir string, tags ...string) {
	t.Helper()
	for _, name := range tags {
		git(t, dir, "tag", name)
	}
}

// writeFiles writes files into the repository, relative to its root.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewRejects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy string
		with     map[string]string
		want     string
	}{
		{"no strategy", "", nil, "no strategy"},
		{"unknown strategy", "aptPackage", nil, "is not one of"},
		{"missing key", StrategyGitTag, map[string]string{}, `needs "tagPrefix"`},
		{"unknown key", StrategyGitTag, map[string]string{"tagPrefix": "", "tagPrefx": "x"}, "does not take tagPrefx"},
		{"locked package needs from", StrategyLockedPackage, map[string]string{"lockDir": "a"}, `needs "from"`},
		{"make var needs var", StrategyMakeVar, map[string]string{}, `needs "var"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.strategy, tc.with, ".")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestNewAcceptsAnEmptyTagPrefix(t *testing.T) {
	if _, err := New(StrategyGitTag, map[string]string{"tagPrefix": ""}, "."); err != nil {
		t.Errorf("an empty prefix names the repository's own tags: %v", err)
	}
}

func TestGitTagResolve(t *testing.T) {
	dir := newRepo(t)
	tag(t, dir,
		"controller/v0.1.1-rc.2", "controller/v0.1.1-rc.10", "controller/v0.1.0",
		"controller/v0.1.2-rc.1", "controller/not-a-version", "cli/v9.9.9",
		"v0.21.0", "v0.22.0-beta.1",
	)

	for _, tc := range []struct {
		name   string
		prefix string
		inputs map[string]string
		want   string
		wantEr string
	}{
		{"highest release wins over a newer candidate", "controller/", nil, "v0.1.0", ""},
		{"candidates only", "controller/", map[string]string{InputSelectKey: SelectCandidate}, "v0.1.2-rc.1", ""},
		{"named version", "controller/", map[string]string{InputVersionKey: "v0.1.1-rc.10"}, "v0.1.1-rc.10", ""},
		{"named version without v", "controller/", map[string]string{InputVersionKey: "0.1.0"}, "v0.1.0", ""},
		{"rc.10 sorts above rc.2", "controller/", map[string]string{InputVersionKey: "v0.1.1-rc.10"}, "v0.1.1-rc.10", ""},
		{"unprefixed tags", "", nil, "v0.21.0", ""},
		{"unprefixed candidate", "", map[string]string{InputSelectKey: SelectCandidate}, "v0.22.0-beta.1", ""},
		{"named version that does not exist", "controller/", map[string]string{InputVersionKey: "v0.9.9"}, "", "has no version v0.9.9"},
		{"not a version", "controller/", map[string]string{InputVersionKey: "rc1"}, "", "is not a version"},
		{"unknown select", "controller/", map[string]string{InputSelectKey: "oldest"}, "", "is not one this understands"},
		{"no tags under the prefix", "os-image/", nil, "", "has no tagged versions"},
		{"no candidate under the prefix", "cli/", map[string]string{InputSelectKey: SelectCandidate}, "", "has no release candidate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GitTag{Dir: dir, TagPrefix: tc.prefix}.Resolve(tc.inputs)
			if tc.wantEr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantEr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantEr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got[ResultVersionKey] != tc.want {
				t.Errorf("version = %q, want %q", got[ResultVersionKey], tc.want)
			}
			if got[ResultRefKey] != tc.prefix+tc.want {
				t.Errorf("ref = %q, want %q", got[ResultRefKey], tc.prefix+tc.want)
			}
		})
	}
}

// With only a release tagged, the repository has no candidate to qualify, so
// the subject cannot resolve until the first beta is cut.
func TestGitTagCandidateNeedsACandidate(t *testing.T) {
	dir := newRepo(t)
	tag(t, dir, "v0.21.0")

	_, err := GitTag{Dir: dir}.Resolve(map[string]string{InputSelectKey: SelectCandidate})
	if err == nil || !strings.Contains(err.Error(), "no release candidate") {
		t.Errorf("err = %v, want it to say there is no release candidate", err)
	}
}

// lockRepo commits lock files and tags the commit, returning the repository.
func lockRepo(t *testing.T, ref string, locks map[string][]string) string {
	t.Helper()
	dir := newRepo(t)
	files := map[string]string{}
	for arch, versions := range locks {
		for _, v := range versions {
			files["pkg/lock/"+arch+"/"+v+".lock"] = "x"
		}
	}
	writeFiles(t, dir, files)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "locks")
	tag(t, dir, ref)
	return dir
}

func TestLockedPackageResolve(t *testing.T) {
	both := []string{"v1.34.2", "v1.34.11", "v1.35.3", "v1.36.3"}
	dir := lockRepo(t, "os-image/v0.1.1-rc.1", map[string][]string{
		"amd64": append([]string{"v1.34.20"}, both...),
		"arm64": both,
	})
	inputs := func(constraint string) map[string]string {
		return map[string]string{InputConstraintKey: constraint, "os-image.ref": "os-image/v0.1.1-rc.1"}
	}
	l := LockedPackage{Dir: dir, LockDir: "pkg/lock", From: "os-image"}

	for _, tc := range []struct {
		constraint string
		want       string
		wantEr     string
	}{
		{"1.34", "1.34.11", ""},
		{"1.35", "1.35.3", ""},
		{"1.36", "1.36.3", ""},
		{"1.34.2", "1.34.2", ""},
		{"1.3", "", "locks no version matching 1.3"},
		{"1.37", "", "locks no version matching 1.37"},
		{"1.x", "", "is not a version prefix"},
	} {
		t.Run(tc.constraint, func(t *testing.T) {
			got, err := l.Resolve(inputs(tc.constraint))
			if tc.wantEr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantEr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantEr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got[ResultVersionKey] != tc.want {
				t.Errorf("version = %q, want %q", got[ResultVersionKey], tc.want)
			}
		})
	}
}

// 1.34.20 is locked for amd64 only, so a plan naming it would fail on arm64.
// The numeric ordering case is 1.34.11 against 1.34.2, which a string sort
// gets the wrong way round.
func TestLockedPackageNeedsEveryArchitecture(t *testing.T) {
	dir := lockRepo(t, "os-image/v0.1.1-rc.1", map[string][]string{
		"amd64": {"v1.34.20", "v1.34.2", "v1.34.11"},
		"arm64": {"v1.34.2", "v1.34.11"},
	})
	l := LockedPackage{Dir: dir, LockDir: "pkg/lock", From: "os-image"}

	got, err := l.Resolve(map[string]string{InputConstraintKey: "1.34", "os-image.ref": "os-image/v0.1.1-rc.1"})
	if err != nil {
		t.Fatal(err)
	}
	if got[ResultVersionKey] != "1.34.11" {
		t.Errorf("version = %q, want 1.34.11", got[ResultVersionKey])
	}
}

func TestLockedPackageRejects(t *testing.T) {
	dir := lockRepo(t, "os-image/v0.1.1-rc.1", map[string][]string{"amd64": {"v1.34.2"}})
	l := LockedPackage{Dir: dir, LockDir: "pkg/lock", From: "os-image"}

	for _, tc := range []struct {
		name   string
		inputs map[string]string
		want   string
	}{
		{"no constraint", map[string]string{"os-image.ref": "os-image/v0.1.1-rc.1"}, "no constraint given"},
		{"no ref", map[string]string{InputConstraintKey: "1.34"}, "no os-image.ref given"},
		{"ref that is not a tag", map[string]string{InputConstraintKey: "1.34", "os-image.ref": "os-image/v9.9.9"}, "reading pkg/lock at os-image/v9.9.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := l.Resolve(tc.inputs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	t.Run("no lock files", func(t *testing.T) {
		empty := LockedPackage{Dir: dir, LockDir: "missing", From: "os-image"}
		_, err := empty.Resolve(map[string]string{InputConstraintKey: "1.34", "os-image.ref": "os-image/v0.1.1-rc.1"})
		if err == nil || !strings.Contains(err.Error(), "holds no lock files") {
			t.Errorf("err = %v, want it to say there are no lock files", err)
		}
	})
}

func TestMakeVarResolve(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"make/tool-versions.mk": `
KIND_VERSION    ?= v0.32.0
CAPI_VERSION    ?= v1.12.3  # one pin for the binary and its providers
NERDCTL_VERSION := 2.3.5
GO_VERSION      ?= $(shell awk '/^go /{print $$2}' go.work)
EMPTY           ?=
KIND_VERSION_EXTRA ?= nope
`})

	for _, tc := range []struct {
		name   string
		v      string
		want   string
		wantEr string
	}{
		{"conditional assignment", "KIND_VERSION", "v0.32.0", ""},
		{"trailing comment", "CAPI_VERSION", "v1.12.3", ""},
		{"simple assignment", "NERDCTL_VERSION", "2.3.5", ""},
		{"computed value", "GO_VERSION", "", "computed rather than a pin"},
		{"empty value", "EMPTY", "", "is empty"},
		{"not set", "MISSING_VERSION", "", "does not set MISSING_VERSION"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MakeVar{Dir: dir, Path: DefaultToolVersions, Var: tc.v}.Resolve(nil)
			if tc.wantEr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantEr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantEr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got[ResultVersionKey] != tc.want {
				t.Errorf("version = %q, want %q", got[ResultVersionKey], tc.want)
			}
		})
	}
}

func TestMakeVarMissingFile(t *testing.T) {
	_, err := MakeVar{Dir: t.TempDir(), Path: DefaultToolVersions, Var: "X"}.Resolve(nil)
	if err == nil || !strings.Contains(err.Error(), "reading make/tool-versions.mk") {
		t.Errorf("err = %v, want it to name the file", err)
	}
}
