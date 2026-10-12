// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

const (
	// DeployPathRef is what the deploy/nico deploy path is staged from: the
	// commit under test. It was pinned to the head of pull request 812 while
	// the deploy path existed only there; 812 was squash-merged, so that
	// commit is on no branch and a clone without `refs/pull/812/head` cannot
	// resolve it. The site lane stages from the controller's own commit
	// instead, since the deploy path carries the controller's chart; see
	// deployPathRef in site_controller_test.go.
	DeployPathRef = "HEAD"

	// deployDir holds the e2e deploy paths and the charts every path installs
	// by relative path; hackDir the post-install script; and nktpChartDir the
	// NKTP chart, which the nktp item installs by relative path from outside
	// both. These three have not moved between releases.
	deployDir    = "controller/deploy"
	hackDir      = "controller/hack"
	nktpChartDir = "nktp/deploy/charts/nktp"
)

// DeployLayout is where a ref keeps the deploy assets, and how the executor it
// ships is called. The lane exports from the release tag under
// qualification, so it reads trees older than itself and cannot assume they
// are shaped like the checkout it was built from.
//
// There are two so far. The executor, its helpers and the customer deploy
// path moved to a top-level deploy/ in #1291; every tag cut before that
// keeps them under build/ and controller/deploy/nke-sbom. A new entry is
// owed to this list whenever they move again.
type DeployLayout struct {
	// Name identifies the layout in errors, as the directory it is built
	// around.
	Name string
	// Executor is the install script, relative to the exported assets.
	Executor string
	// DeployPath is the customer deploy path, relative to the exported
	// assets. What is inside it has not changed: both layouts hold
	// controller/release.yaml and spire/overlay.yaml.
	DeployPath string
	// Export is what to pull out of the ref.
	Export []string
	// legacyTargetFlags selects the deploy path the way the older executor
	// does. See TargetArgs.
	legacyTargetFlags bool
}

// CurrentLayout() is the layout from #1453 onwards, which gave the #1291
// executor its nvke- name and changed nothing else. The one tag cut between
// the two, controller/v0.1.2-rc.1, matches neither layout and is not
// qualified; a later tag supersedes it.
//
// A function and not a package variable, so each caller gets its own Export
// and appending to one cannot reshape the other's.
func CurrentLayout() DeployLayout {
	return DeployLayout{
		Name:       "deploy",
		Executor:   "deploy/nvke-deploy.sh",
		DeployPath: "deploy/nico",
		Export:     []string{"deploy", deployDir, hackDir, nktpChartDir},
	}
}

// LegacyLayout() is the layout every release tag before #1291 carries.
func LegacyLayout() DeployLayout {
	return DeployLayout{
		Name:              "build",
		Executor:          "build/use-nke-sbom.sh",
		DeployPath:        deployDir + "/nke-sbom",
		Export:            []string{"build", deployDir, hackDir, nktpChartDir},
		legacyTargetFlags: true,
	}
}

// TargetArgs names this layout's deploy path, which is under assets, to its
// executor. It is the whole command-line difference between the two:
// `--sbom DIR --target NAME` became `--deploy-path DIR/NAME`. Every other
// flag they take is spelled the same.
func (l DeployLayout) TargetArgs(assets string) []string {
	full := path.Join(assets, l.DeployPath)
	if l.legacyTargetFlags {
		return []string{"--sbom", path.Dir(full), "--target", path.Base(full)}
	}
	return []string{"--deploy-path", full}
}

// ControllerReleasePath is the HelmRelease deployPath installs the
// controller from, relative to the exported assets. Both layouts keep it in
// the same place inside the deploy path.
func ControllerReleasePath(deployPath string) string {
	return path.Join(deployPath, "controller", "release.yaml")
}

// ResolveLayout reports which layout ref uses.
//
// It probes for each executor rather than reading the version, because the
// move is a property of the tree and a version string only correlates with
// it: a backport, a branch or a hand-cut tag can carry either shape.
func ResolveLayout(ctx context.Context, repo, ref string) (DeployLayout, error) {
	// That ref resolves at all, first. This runs ahead of ExportRef, which
	// is where that check used to live, and without it a version with a
	// typo in it reports that the executor moved and a layout is owed --
	// sending the reader to add an entry for a ref that does not exist.
	if err := exec.CommandContext(ctx, "git", "-C", repo, "cat-file", "-e", ref+"^{commit}").Run(); err != nil {
		return DeployLayout{}, fmt.Errorf("resolve layout: %s is not a commit in %s: %w", ref, repo, err)
	}
	for _, layout := range []DeployLayout{CurrentLayout(), LegacyLayout()} {
		probe := exec.CommandContext(ctx, "git", "-C", repo, "cat-file", "-e", ref+":"+layout.Executor)
		if probe.Run() == nil {
			return layout, nil
		}
	}
	return DeployLayout{}, fmt.Errorf(
		"%s in %s carries neither %s nor %s, so the deploy assets cannot be placed: "+
			"if the executor moved again, add the layout it moved to",
		ref, repo, CurrentLayout().Executor, LegacyLayout().Executor,
	)
}

// ExportRef writes paths as they are at ref in the repository at repo into a
// new temporary directory, and returns it for Site.Assets. Only tracked
// files are exported, so nothing from the runner's working tree leaks in.
func ExportRef(ctx context.Context, repo, ref string, paths []string) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("export ref: no paths")
	}
	var stderr bytes.Buffer
	verify := exec.CommandContext(ctx, "git", "-C", repo, "cat-file", "-e", ref+"^{commit}")
	verify.Stderr = &stderr
	if err := verify.Run(); err != nil {
		return "", fmt.Errorf("export ref: %s is not a commit in %s: %w", ref, repo, err)
	}

	dir, err := os.MkdirTemp("", "nke-e2e-assets-")
	if err != nil {
		return "", err
	}
	args := append([]string{"-C", repo, "archive", "--format=tar", ref, "--"}, paths...)
	archive := exec.CommandContext(ctx, "git", args...)
	stderr.Reset()
	archive.Stderr = &stderr
	out, err := archive.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := archive.Start(); err != nil {
		return "", err
	}
	extractErr := extract(out, dir)
	_, _ = io.Copy(io.Discard, out)
	if err := archive.Wait(); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("export ref: git archive %s: %w: %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	if extractErr != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("export ref: %w", extractErr)
	}
	return dir, nil
}

// extract unpacks a git archive into dir, refusing any entry that would land
// outside it. Only directories and regular files are accepted: the deploy
// path has no symlinks, and one appearing is worth a failure to look at.
func extract(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := within(dir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeEntry(target, tr, hdr.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader:
			// git archive records the commit here; there is nothing to write.
		default:
			return fmt.Errorf("unexpected entry %s of type %c", hdr.Name, hdr.Typeflag)
		}
	}
}

func within(dir, name string) (string, error) {
	target := filepath.Join(dir, filepath.FromSlash(name))
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("entry %s is outside the export", name)
	}
	return target, nil
}

func writeEntry(target string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
