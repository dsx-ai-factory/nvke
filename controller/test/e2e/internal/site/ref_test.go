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
	writeFile(t, filepath.Join(repo, "deploy", "nke-deploy.sh"), "v1")
	writeFile(t, filepath.Join(repo, "deploy", "nico", "nke-deploy.yaml"), "items")
	writeFile(t, filepath.Join(repo, "controller", "main.go"), "package main")
	if err := os.Chmod(filepath.Join(repo, "deploy", "nke-deploy.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "pinned")
	pinned := git(t, repo, "rev-parse", "HEAD")

	writeFile(t, filepath.Join(repo, "deploy", "nke-deploy.sh"), "v2")
	git(t, repo, "commit", "-q", "-am", "later")
	writeFile(t, filepath.Join(repo, "deploy", "._nke-deploy.sh"), "untracked")

	dir, err := ExportRef(context.Background(), repo, pinned, []string{"deploy"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	script := filepath.Join(dir, "deploy", "nke-deploy.sh")
	if got, err := os.ReadFile(script); err != nil || string(got) != "v1" {
		t.Errorf("script = %q, %v; want the pinned content", got, err)
	}
	if info, err := os.Stat(script); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("script lost its executable bit: %v", info.Mode())
	}
	for _, absent := range []string{"deploy/._nke-deploy.sh", "controller/main.go"} {
		if _, err := os.Stat(filepath.Join(dir, absent)); !os.IsNotExist(err) {
			t.Errorf("%s was exported", absent)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy", "nico", "nke-deploy.yaml")); err != nil {
		t.Error(err)
	}
}

func TestExportRefMissingCommit(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	// An empty repository has no HEAD commit, so DeployPathRef does not
	// resolve and the error has to name the ref rather than the git plumbing.
	_, err := ExportRef(context.Background(), repo, DeployPathRef, LayoutDeploy().Export)
	if err == nil || !strings.Contains(err.Error(), DeployPathRef) || !strings.Contains(err.Error(), "is not a commit") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractRefusesEscapes(t *testing.T) {
	for name, hdr := range map[string]*tar.Header{
		"parent":  {Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		"symlink": {Name: "deploy/link", Typeflag: tar.TypeSymlink, Linkname: "nke-deploy.sh"},
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

// TestResolveLayout covers picking the layout off the tree rather than off
// the version string. A backport, a branch or a hand-cut tag can carry
// either shape, so the executor's presence decides it.
//
// Needs a repository, like the ExportRef tests above it.
func TestResolveLayout(t *testing.T) {
	for _, tc := range []struct {
		name, executor string
		want           Layout
	}{
		{"a release from #1291 onwards", LayoutDeploy().Executor, LayoutDeploy()},
		{"a release before it", LayoutBuild().Executor, LayoutBuild()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			git(t, repo, "init", "-q")
			writeFile(t, filepath.Join(repo, tc.executor), "script")
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-qm", "layout")

			got, err := ResolveLayout(context.Background(), repo, "HEAD")
			if err != nil {
				t.Fatalf("resolving: %v", err)
			}
			if got.Name != tc.want.Name {
				t.Errorf("ResolveLayout = %s, want %s", got.Name, tc.want.Name)
			}
		})
	}

	t.Run("neither executor names both, so the next move is obvious", func(t *testing.T) {
		repo := t.TempDir()
		git(t, repo, "init", "-q")
		writeFile(t, filepath.Join(repo, "somewhere", "else.sh"), "script")
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-qm", "moved again")

		_, err := ResolveLayout(context.Background(), repo, "HEAD")
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{LayoutDeploy().Executor, LayoutBuild().Executor} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %s: %v", want, err)
			}
		}
	})
}

// TestLayoutsExportWhatTheirExecutorNeeds is the regression for run
// 37830345659, which died on `git archive controller/v0.1.1: fatal: pathspec
// 'deploy' did not match any files`. The harness asked a tag from 28
// September for a directory #1291 created on 8 October. A layout that
// exports one tree and runs an executor from another cannot work, so each
// one has to carry its own export list.
func TestLayoutsExportWhatTheirExecutorNeeds(t *testing.T) {
	for _, layout := range []Layout{LayoutDeploy(), LayoutBuild()} {
		t.Run(layout.Name, func(t *testing.T) {
			root := strings.SplitN(layout.Executor, "/", 2)[0]
			if !slices.Contains(layout.Export, root) {
				t.Errorf("layout %s runs %s but exports %v, which does not include %s",
					layout.Name, layout.Executor, layout.Export, root)
			}
			// The deploy path is read from the assets, so whatever holds it
			// has to be exported too.
			if !slices.ContainsFunc(layout.Export, func(p string) bool {
				return layout.DeployPath == p || strings.HasPrefix(layout.DeployPath, p+"/")
			}) {
				t.Errorf("layout %s installs %s but exports %v, none of which contains it",
					layout.Name, layout.DeployPath, layout.Export)
			}
		})
	}

	// The specific mistake: main's list requested deploy/ from tags that
	// have never had it.
	if slices.Contains(LayoutBuild().Export, "deploy") {
		t.Errorf("the build layout exports deploy/, which no tag before #1291 has: %v", LayoutBuild().Export)
	}
}

// TestLayoutTargetArgs covers the whole command-line difference between the
// two executors. `--sbom DIR --target NAME` became `--deploy-path DIR/NAME`;
// passing either form to the other executor is rejected as an unknown flag.
func TestLayoutTargetArgs(t *testing.T) {
	for _, tc := range []struct {
		layout Layout
		want   []string
	}{
		{LayoutDeploy(), []string{"--deploy-path", "/assets/deploy/nico"}},
		{LayoutBuild(), []string{"--sbom", "/assets/controller/deploy", "--target", "nke-sbom"}},
	} {
		t.Run(tc.layout.Name, func(t *testing.T) {
			got := tc.layout.TargetArgs("/assets", tc.layout.DeployPath)
			if !slices.Equal(got, tc.want) {
				t.Errorf("TargetArgs = %v, want %v", got, tc.want)
			}
		})
	}

	// An overridden deploy path still gets its own layout's flags, rather
	// than the layout's default path.
	got := LayoutBuild().TargetArgs("/assets", "controller/deploy/capd-e2e")
	want := []string{"--sbom", "/assets/controller/deploy", "--target", "capd-e2e"}
	if !slices.Equal(got, want) {
		t.Errorf("an overridden deploy path gives %v, want %v", got, want)
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

	var resolved int
	for _, tag := range tags {
		layout, err := ResolveLayout(context.Background(), repo, tag)
		if err != nil {
			// Tags older than the SBOM deploy path carry no executor at
			// all. That is a true answer about them, not a gap in the
			// table, and the error says so in terms a reader can act on.
			t.Logf("%s predates both layouts", tag)
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

	// A table that resolves nothing passes every assertion above by doing
	// no work, which is the failure mode this test exists to avoid.
	if resolved == 0 {
		t.Errorf("none of the %d release tags resolves to a layout, so the table describes no release that exists", len(tags))
	}
}

// TestControllerReleasePathFollowsTheDeployPath pins the one thing both
// layouts share: the shape inside a deploy path. CheckControllerName reads
// this file to decide the name CWE installs under, and it moved with the
// directory, not within it.
func TestControllerReleasePathFollowsTheDeployPath(t *testing.T) {
	for _, tc := range []struct{ deployPath, want string }{
		{LayoutDeploy().DeployPath, "deploy/nico/controller/release.yaml"},
		{LayoutBuild().DeployPath, "controller/deploy/nke-sbom/controller/release.yaml"},
	} {
		if got := ControllerReleasePath(tc.deployPath); got != tc.want {
			t.Errorf("ControllerReleasePath(%q) = %q, want %q", tc.deployPath, got, tc.want)
		}
	}
}
