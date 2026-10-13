// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", append([]string{
		"-C", dir, "-c", "user.name=e2e", "-c", "user.email=e2e@example.com", "-c", "commit.gpgsign=false",
	}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestExportRef(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	writeFile(t, filepath.Join(repo, "deploy", "nvke-deploy.sh"), "v1")
	writeFile(t, filepath.Join(repo, "deploy", "nico", "nvke-deploy.yaml"), "items")
	writeFile(t, filepath.Join(repo, "controller", "main.go"), "package main")
	if err := os.Chmod(filepath.Join(repo, "deploy", "nvke-deploy.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "pinned")
	pinned := git(t, repo, "rev-parse", "HEAD")

	writeFile(t, filepath.Join(repo, "deploy", "nvke-deploy.sh"), "v2")
	git(t, repo, "commit", "-q", "-am", "later")
	writeFile(t, filepath.Join(repo, "deploy", "._nvke-deploy.sh"), "untracked")

	dir, err := ExportRef(context.Background(), repo, pinned, []string{"deploy"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	script := filepath.Join(dir, "deploy", "nvke-deploy.sh")
	if got, err := os.ReadFile(script); err != nil || string(got) != "v1" {
		t.Errorf("script = %q, %v; want the pinned content", got, err)
	}
	if info, err := os.Stat(script); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("script lost its executable bit: %v", info.Mode())
	}
	for _, absent := range []string{"deploy/._nvke-deploy.sh", "controller/main.go"} {
		if _, err := os.Stat(filepath.Join(dir, absent)); !os.IsNotExist(err) {
			t.Errorf("%s was exported", absent)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy", "nico", "nvke-deploy.yaml")); err != nil {
		t.Error(err)
	}
}

func TestExportRefMissingCommit(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	// An empty repository has no HEAD commit, so DeployPathRef does not
	// resolve and the error has to name the ref rather than the git plumbing.
	_, err := ExportRef(context.Background(), repo, DeployPathRef, CurrentLayout().Export)
	if err == nil || !strings.Contains(err.Error(), DeployPathRef) || !strings.Contains(err.Error(), "is not a commit") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractRefusesEscapes(t *testing.T) {
	for name, hdr := range map[string]*tar.Header{
		"parent":  {Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		"symlink": {Name: "deploy/link", Typeflag: tar.TypeSymlink, Linkname: "nvke-deploy.sh"},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extract(&buf, t.TempDir()); err == nil {
				t.Fatal("an unsafe entry was extracted")
			}
		})
	}
}

// TestResolveLayout covers reading the layout off the tree rather than off
// the version string.
//
// Needs a repository, like the ExportRef tests above it.
func TestResolveLayout(t *testing.T) {
	commit := func(t *testing.T, executor string) string {
		t.Helper()
		repo := t.TempDir()
		git(t, repo, "init", "-q")
		writeFile(t, filepath.Join(repo, executor), "script")
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-qm", "layout")
		return repo
	}

	t.Run("a release carrying the executor", func(t *testing.T) {
		got, err := ResolveLayout(context.Background(), commit(t, CurrentLayout().Executor), "HEAD")
		if err != nil {
			t.Fatalf("resolving: %v", err)
		}
		if got.Name != CurrentLayout().Name {
			t.Errorf("ResolveLayout = %s, want %s", got.Name, CurrentLayout().Name)
		}
	})

	t.Run("an older release says it is not qualified", func(t *testing.T) {
		_, err := ResolveLayout(context.Background(), commit(t, "somewhere/else.sh"), "HEAD")
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{CurrentLayout().Executor, "controller/v0.2.0-rc.1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %s: %v", want, err)
			}
		}
	})
}

// TestLayoutExportsWhatItsExecutorNeeds is the regression for run
// 37830345659, which died on `git archive controller/v0.1.1: fatal: pathspec
// 'deploy' did not match any files`. A layout that exports one tree and runs
// an executor from another cannot work.
func TestLayoutExportsWhatItsExecutorNeeds(t *testing.T) {
	layout := CurrentLayout()
	root := strings.SplitN(layout.Executor, "/", 2)[0]
	if !slices.Contains(layout.Export, root) {
		t.Errorf("layout %s runs %s but exports %v, which does not include %s",
			layout.Name, layout.Executor, layout.Export, root)
	}
	// The deploy path is read from the assets, so whatever holds it has to
	// be exported too.
	if !slices.ContainsFunc(layout.Export, func(p string) bool {
		return layout.DeployPath == p || strings.HasPrefix(layout.DeployPath, p+"/")
	}) {
		t.Errorf("layout %s installs %s but exports %v, none of which contains it",
			layout.Name, layout.DeployPath, layout.Export)
	}
}

func TestLayoutTargetArgs(t *testing.T) {
	want := []string{"--deploy-path", "/assets/deploy/nico"}
	if got := CurrentLayout().TargetArgs("/assets"); !slices.Equal(got, want) {
		t.Errorf("TargetArgs = %v, want %v", got, want)
	}
}

// TestLayoutsMatchTheReleaseTags is the only test here that checks the
// layout table against reality rather than against itself. The others pin
// the table's internal shape, which stays true even if every entry drifts
// away from what the tags actually carry.
//
// Skips rather than fails when the tags are not in the clone, the way
// deployPath does in stack_charts_test.go: a shallow or fork clone is a
// normal thing to run tests in, and has nothing to say about this code.
func TestLayoutsMatchTheReleaseTags(t *testing.T) {
	if testing.Short() {
		t.Skip("exports a tree per release tag")
	}
	top, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not in a git clone: %v", err)
	}
	repo := strings.TrimSpace(string(top))

	// controller/v* only: controller/api/* tags the API module, which ships
	// no deploy assets of its own.
	out, err := exec.CommandContext(context.Background(), "git", "-C", repo, "tag", "-l", "controller/v*").Output()
	if err != nil {
		t.Skipf("listing tags: %v", err)
	}
	tags := strings.Fields(string(out))
	if len(tags) == 0 {
		t.Skip("no release tags in this clone")
	}

	var resolved, readable int
	for _, tag := range tags {
		// A tag ref whose commit was never fetched says nothing about the
		// table. CI checks out shallow, so this is the common case there
		// and has to be told apart from a tag that genuinely carries no
		// executor.
		if exec.CommandContext(context.Background(), "git", "-C", repo, "cat-file", "-e", tag+"^{commit}").Run() != nil {
			t.Logf("%s is not in this clone", tag)
			continue
		}
		readable++

		layout, err := ResolveLayout(context.Background(), repo, tag)
		if err != nil {
			// Tags before controller/v0.2.0-rc.1 are not qualified, which
			// is a true answer about them rather than a gap in the table.
			t.Logf("%s predates the supported layout", tag)
			continue
		}
		resolved++

		t.Run(tag, func(t *testing.T) {
			assets, err := ExportRef(context.Background(), repo, tag, layout.Export)
			if err != nil {
				t.Fatalf("layout %s cannot be exported from %s: %v", layout.Name, tag, err)
			}
			defer func() { _ = os.RemoveAll(assets) }()

			// Everything the deployer reaches for after the export.
			for _, rel := range []string{
				layout.Executor,
				layout.DeployPath,
				ControllerReleasePath(layout.DeployPath),
			} {
				if _, err := os.Stat(filepath.Join(assets, rel)); err != nil {
					t.Errorf("layout %s exports %v from %s, which does not place %s: %v",
						layout.Name, layout.Export, tag, rel, err)
				}
			}
		})
	}

	if readable == 0 {
		t.Skip("no release tag in this clone has its commit; CI needs fetch-tags on the checkout")
	}
	// A table that resolves nothing passes every assertion above by doing
	// no work, which is the failure mode this test exists to avoid.
	if resolved == 0 {
		t.Errorf("none of the %d readable release tags resolves to a layout, so the table describes no release that exists", readable)
	}
}

// TestControllerReleasePathFollowsTheDeployPath pins the shape inside a
// deploy path. CheckControllerName reads this file for the name the
// controller installs under.
func TestControllerReleasePathFollowsTheDeployPath(t *testing.T) {
	const want = "deploy/nico/controller/release.yaml"
	if got := ControllerReleasePath(CurrentLayout().DeployPath); got != want {
		t.Errorf("ControllerReleasePath(%q) = %q, want %q", CurrentLayout().DeployPath, got, want)
	}
}
