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
	writeFile(t, filepath.Join(repo, "build", "use-nke-sbom.sh"), "v1")
	writeFile(t, filepath.Join(repo, "controller", "deploy", "nke-sbom", "nke-deploy.yaml"), "items")
	writeFile(t, filepath.Join(repo, "controller", "main.go"), "package main")
	if err := os.Chmod(filepath.Join(repo, "build", "use-nke-sbom.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "pinned")
	pinned := git(t, repo, "rev-parse", "HEAD")

	writeFile(t, filepath.Join(repo, "build", "use-nke-sbom.sh"), "v2")
	git(t, repo, "commit", "-q", "-am", "later")
	writeFile(t, filepath.Join(repo, "build", "._use-nke-sbom.sh"), "untracked")

	dir, err := ExportRef(context.Background(), repo, pinned, []string{"build", "controller/deploy"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	script := filepath.Join(dir, "build", "use-nke-sbom.sh")
	if got, err := os.ReadFile(script); err != nil || string(got) != "v1" {
		t.Errorf("script = %q, %v; want the pinned content", got, err)
	}
	if info, err := os.Stat(script); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("script lost its executable bit: %v", info.Mode())
	}
	for _, absent := range []string{"build/._use-nke-sbom.sh", "controller/main.go"} {
		if _, err := os.Stat(filepath.Join(dir, absent)); !os.IsNotExist(err) {
			t.Errorf("%s was exported", absent)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "controller", "deploy", "nke-sbom", "nke-deploy.yaml")); err != nil {
		t.Error(err)
	}
}

func TestExportRefMissingCommit(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	_, err := ExportRef(context.Background(), repo, DeployPathRef, DeployAssetPaths())
	if err == nil || !strings.Contains(err.Error(), DeployPathFetchRef) {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractRefusesEscapes(t *testing.T) {
	for name, hdr := range map[string]*tar.Header{
		"parent":  {Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		"symlink": {Name: "build/link", Typeflag: tar.TypeSymlink, Linkname: "use-nke-sbom.sh"},
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
