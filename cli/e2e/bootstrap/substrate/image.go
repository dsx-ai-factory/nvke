// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"context"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Build assets for the node image, embedded so the package has no runtime
// dependency on its own source tree layout.
//
//go:embed node/Dockerfile
var nodeAssets embed.FS

// EnsureNodeImage builds the pre-provisioned node image if it is not already
// present locally, and returns its tag.
//
// The tag embeds the base image so a run never silently reuses a derivation
// built on top of a different (e.g. stale) CAPD image.
func EnsureNodeImage(ctx context.Context, cfg Config, log Logf) (string, error) {
	tag := cfg.Image
	if tag == "" {
		tag = "nkectl-e2e/bootstrap-node:" + sanitizeTag(cfg.BaseImage)
	}

	if imageExists(ctx, tag) {
		log("substrate: reusing node image %s", tag)
		return tag, nil
	}

	if !imageExists(ctx, cfg.BaseImage) {
		return "", fmt.Errorf(
			"base image %q not found locally.\n"+
				"  Build it first with `make -C cli ensure-capd-image`, or set\n"+
				"  NKECTL_E2E_BASE_IMAGE to an image that has nke-agent installed\n"+
				"  as a systemd unit",
			cfg.BaseImage,
		)
	}

	buildDir, err := os.MkdirTemp("", "nkectl-e2e-build-*")
	if err != nil {
		return "", fmt.Errorf("creating build context: %w", err)
	}
	defer func() { _ = os.RemoveAll(buildDir) }()

	dockerfile, err := nodeAssets.ReadFile("node/Dockerfile")
	if err != nil {
		return "", fmt.Errorf("reading embedded Dockerfile: %w", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), dockerfile, 0o600); err != nil {
		return "", fmt.Errorf("staging Dockerfile: %w", err)
	}

	log("substrate: building node image %s from %s", tag, cfg.BaseImage)
	build := exec.CommandContext(ctx, "docker", "build",
		"--build-arg", "BASE_IMAGE="+cfg.BaseImage,
		"--build-arg", "NODE_AGENT_PORT="+cfg.AgentPort,
		"-t", tag, buildDir)
	if out, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker build %s: %w\n%s", tag, err, out)
	}

	return tag, nil
}

func imageExists(ctx context.Context, tag string) bool {
	return exec.CommandContext(ctx, "docker", "image", "inspect", tag).Run() == nil
}

// sanitizeTag turns an image reference into something usable as a tag
// component, so the derived tag records which base it was built from.
func sanitizeTag(image string) string {
	replacer := strings.NewReplacer("/", "-", ":", "-")
	return replacer.Replace(image)
}
