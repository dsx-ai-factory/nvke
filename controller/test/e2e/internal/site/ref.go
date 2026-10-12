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
	// both.
	deployDir    = "controller/deploy"
	hackDir      = "controller/hack"
	nktpChartDir = "nktp/deploy/charts/nktp"
)

// DeployLayout is where a ref keeps the deploy assets, and how the executor it
// ships is called. The lane exports from the release tag under
// qualification, so it reads trees older than itself and cannot assume they
// are shaped like the checkout it was built from.
//
// Only CurrentLayout() is supported. Release tags cut before it are not
// qualified: the plan selects the latest controller tag, and nothing pins an
// older one.
type DeployLayout struct {
	// Name identifies the layout in errors, as the directory it is built
	// around.
	Name string
	// Executor is the install script, relative to the exported assets.
	Executor string
	// DeployPath is the customer deploy path, relative to the exported
	// assets. It holds controller/release.yaml and spire/overlay.yaml.
	DeployPath string
	// Export is what to pull out of the ref.
	Export []string
}

// CurrentLayout() is the layout from #1453 onwards, the first controller tag
// carrying it being controller/v0.2.0-rc.1.
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

// TargetArgs names this layout's deploy path, which is under assets, to its
// executor.
func (l DeployLayout) TargetArgs(assets string) []string {
	return []string{"--deploy-path", path.Join(assets, l.DeployPath)}
}

// ControllerReleasePath is the HelmRelease deployPath installs the
// controller from, relative to the exported assets.
func ControllerReleasePath(deployPath string) string {
	return path.Join(deployPath, "controller", "release.yaml")
}

// ResolveLayout reports the layout ref uses, or that it predates the one the
// lane supports. It probes for the executor rather than reading the version,
// because a backport, a branch or a hand-cut tag need not follow the version.
func ResolveLayout(ctx context.Context, repo, ref string) (DeployLayout, error) {
	// That ref resolves at all, first, so a version with a typo in it does
	// not report that the executor moved.
	if err := exec.CommandContext(ctx, "git", "-C", repo, "cat-file", "-e", ref+"^{commit}").Run(); err != nil {
		return DeployLayout{}, fmt.Errorf("resolve layout: %s is not a commit in %s: %w", ref, repo, err)
	}
	layout := CurrentLayout()
	if exec.CommandContext(ctx, "git", "-C", repo, "cat-file", "-e", ref+":"+layout.Executor).Run() != nil {
		return DeployLayout{}, fmt.Errorf(
			"%s in %s has no %s: the site lane qualifies controller/v0.2.0-rc.1 and later, "+
				"so qualify a newer controller, or update CurrentLayout if the executor moved",
			ref, repo, layout.Executor,
		)
	}
	return layout, nil
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
