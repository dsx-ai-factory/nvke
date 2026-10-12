// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// Build assets for the node image, embedded so the package has no runtime
// dependency on its own source tree layout.
//
//go:embed node/Dockerfile node/agent.conf.tmpl
var nodeAssets embed.FS

const (
	// tpmPluginVersion pins get_tpm_pubhash to the spire-tpm-plugin release NVKE
	// already ships in its spire-agent bundle (pkg-builder/bundles_latest/
	// spire-agent.yaml, TPM_PLUGIN_VERSION), so the fingerprint this tool
	// prints is computed by the same code as the attestor that presents it.
	tpmPluginVersion    = "v1.9.0"
	tpmPluginReleaseURL = "https://github.com/spiffe/spire-tpm-plugin/releases/download"
)

// EnsureNodeImage ensures that the pre-provisioned node image exists locally
// and returns its tag.
func EnsureNodeImage(ctx context.Context, cfg Config, log Logf) (string, error) {
	// Before the cache check, not after: a cached image means the certificate
	// is already baked in, but `nvkectl bootstrap site` still needs the key on
	// this run. Generating it here only when the image is rebuilt would leave a
	// second run with an image and no CA to go with it.
	caCert, _, err := EnsureBootstrapCA(cfg.CADir)
	if err != nil {
		return "", err
	}
	caPEM, err := os.ReadFile(caCert)
	if err != nil {
		return "", fmt.Errorf("reading the bootstrap CA %s: %w", caCert, err)
	}
	dockerfile, err := nodeAssets.ReadFile("node/Dockerfile")
	if err != nil {
		return "", fmt.Errorf("reading embedded Dockerfile: %w", err)
	}
	agentConfigTemplate, err := nodeAssets.ReadFile("node/agent.conf.tmpl")
	if err != nil {
		return "", fmt.Errorf("reading embedded agent config: %w", err)
	}

	tag := cfg.Image
	if tag == "" {
		// Every input baked into the image is part of the cache key. This keeps
		// source or SPIRE configuration changes from silently reusing a stale
		// node image.
		cacheInput := bytes.Join([][]byte{
			caPEM,
			dockerfile,
			agentConfigTemplate,
			[]byte(tpmPluginVersion),
			[]byte(cfg.AgentPort),
			[]byte(cfg.SpireServerAddress),
			[]byte(cfg.SpireServerPort),
			[]byte(cfg.SpireTrustDomain),
		}, []byte{0})
		digest := sha256.Sum256(cacheInput)
		tag = fmt.Sprintf("nvkectl-e2e/bootstrap-node:%s-ca%x",
			sanitizeTag(cfg.BaseImage), digest[:6])
	}

	if imageExists(ctx, tag) {
		log("substrate: reusing node image %s", tag)
		return tag, nil
	}

	if !imageExists(ctx, cfg.BaseImage) {
		return "", fmt.Errorf(
			"base image %q not found locally.\n"+
				"  Build it first with `make -C cli ensure-capd-image`, or set\n"+
				"  NVKECTL_E2E_BASE_IMAGE to an image that has nvke-agent installed\n"+
				"  as a systemd unit",
			cfg.BaseImage,
		)
	}

	buildDir, err := os.MkdirTemp("", "nvkectl-e2e-build-*")
	if err != nil {
		return "", fmt.Errorf("creating build context: %w", err)
	}
	defer func() { _ = os.RemoveAll(buildDir) }()

	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), dockerfile, 0o600); err != nil {
		return "", fmt.Errorf("staging Dockerfile: %w", err)
	}

	toolsDir := filepath.Join(buildDir, "tools")
	if err := os.MkdirAll(toolsDir, 0o750); err != nil {
		return "", fmt.Errorf("creating tools build context: %w", err)
	}
	if err := stageGetTPMPubhash(ctx, cfg, toolsDir, log); err != nil {
		return "", err
	}

	if err := stageAgentConfig(cfg, buildDir); err != nil {
		return "", err
	}

	// The agents verify the SPIRE server against this on first contact, so it
	// has to be in the image before any of them start.
	if err := os.WriteFile(filepath.Join(toolsDir, CACertFile), caPEM, 0o644); err != nil { //nolint:gosec // a public certificate
		return "", fmt.Errorf("staging the bootstrap CA: %w", err)
	}

	log("substrate: building node image %s from %s", tag, cfg.BaseImage)
	build := exec.CommandContext(ctx, ContainerCLI, "build",
		"--build-arg", "BASE_IMAGE="+cfg.BaseImage,
		"--build-arg", "NODE_AGENT_PORT="+cfg.AgentPort,
		"-t", tag, buildDir)
	if out, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s build %s: %w\n%s", ContainerCLI, tag, err, out)
	}

	return tag, nil
}

// stageAgentConfig renders the spire-agent config into the build context.
//
// It lives in node/agent.conf.tmpl rather than in a printf inside the
// Dockerfile: it is a config file, it is read far more often than the build
// steps around it, and a here-doc escaped through Dockerfile line
// continuations is the worst possible place to read one.
func stageAgentConfig(cfg Config, buildDir string) error {
	body, err := nodeAssets.ReadFile("node/agent.conf.tmpl")
	if err != nil {
		return fmt.Errorf("reading the embedded agent config: %w", err)
	}
	parsed, err := template.New("agent.conf").Parse(string(body))
	if err != nil {
		return fmt.Errorf("parsing the agent config template: %w", err)
	}

	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, struct {
		ServerAddress, ServerPort, TrustDomain, TrustBundlePath string
	}{
		ServerAddress:   cfg.SpireServerAddress,
		ServerPort:      cfg.SpireServerPort,
		TrustDomain:     cfg.SpireTrustDomain,
		TrustBundlePath: nodeTrustBundlePath,
	}); err != nil {
		return fmt.Errorf("rendering the agent config: %w", err)
	}

	return os.WriteFile(filepath.Join(buildDir, "agent.conf"), rendered.Bytes(), 0o600)
}

// stageGetTPMPubhash downloads the pinned upstream tool into the build context,
// caching it so a node-image rebuild does not depend on GitHub being reachable.
func stageGetTPMPubhash(ctx context.Context, cfg Config, toolsDir string, log Logf) error {
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("spire-tpm-plugin publishes no get_tpm_pubhash for GOARCH=%s", arch)
	}
	cached := filepath.Join(cfg.CacheDir, "spire-tpm-plugin", tpmPluginVersion, "get_tpm_pubhash")

	if _, err := os.Stat(cached); err != nil {
		url := fmt.Sprintf("%s/%s/spire_tpm_plugin_get_tpm_pubhash_linux_%s_%s.tar.gz",
			tpmPluginReleaseURL, tpmPluginVersion, arch, tpmPluginVersion)
		if err := os.MkdirAll(filepath.Dir(cached), 0o750); err != nil {
			return fmt.Errorf("creating the plugin cache: %w", err)
		}
		log("substrate: downloading get_tpm_pubhash %s (%s)", tpmPluginVersion, arch)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return fmt.Errorf("building the request for %s: %w", url, err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return fmt.Errorf("downloading %s: %w", url, err)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("downloading %s: %s", url, response.Status)
		}
		// The release asset is a single-file tarball; its layout is fixed by
		// the release rather than discovered.
		untar := exec.CommandContext(ctx, "tar", "xz", "-C", filepath.Dir(cached), "get_tpm_pubhash")
		untar.Stdin = response.Body
		if out, err := untar.CombinedOutput(); err != nil {
			return fmt.Errorf("extracting get_tpm_pubhash: %w\n%s", err, out)
		}
	}

	body, err := os.ReadFile(cached)
	if err != nil {
		return fmt.Errorf("reading %s: %w", cached, err)
	}
	return os.WriteFile(filepath.Join(toolsDir, "get_tpm_pubhash"), body, 0o755) //nolint:gosec // staged for a container image
}

func imageExists(ctx context.Context, tag string) bool {
	return exec.CommandContext(ctx, ContainerCLI, "image", "inspect", tag).Run() == nil
}

// sanitizeTag turns an image reference into something usable as a tag
// component, so the derived tag records which base it was built from.
func sanitizeTag(image string) string {
	replacer := strings.NewReplacer("/", "-", ":", "-")
	return replacer.Replace(image)
}
