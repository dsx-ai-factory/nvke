// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bootstrapengine drives the lifecycle of the ephemeral Kind cluster
// that `nkectl bootstrap site` runs CWE on: create it, wait for it to be
// Ready, load images into it, and tear it down.
//
// Every `kind`/`kubectl` invocation goes through the CommandRunner seam so
// unit tests can substitute a fake rather than requiring a real kind binary
// or container runtime — the same pattern cli/e2e/bootstrap/substrate
// establishes for the fake pre-provisioned-node fixture, generalized here
// into an actual interface (substrate calls os/exec directly; this package
// takes it one step further because Track A's own plan commits to a real,
// mockable seam).
//
// kind is driven through nerdctl and containerd's content store, selected by
// exporting KIND_EXPERIMENTAL_PROVIDER on every kind invocation. That matches
// the runtime the target platform runs, and means a host needs no Docker
// daemon to bootstrap a site.
package bootstrapengine

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// kindExperimentalProviderEnv is the env var kind itself reads to select a
	// provider other than its built-in default. See https://kind.sigs.k8s.io —
	// non-default providers are experimental as of the kind versions this repo
	// pins.
	kindExperimentalProviderEnv = "KIND_EXPERIMENTAL_PROVIDER"

	// RuntimeCLI is the container runtime front end, both as the value kind
	// reads from kindExperimentalProviderEnv and as the binary callers shell
	// out to for the image operations kind does not cover.
	RuntimeCLI = "nerdctl"
)

// kindEnv returns the environment a kind invocation runs with, so kind
// resolves clusters and images through containerd rather than its default.
//
// A fresh map per call, not a package-level one: the result crosses the
// CommandRunner interface, whose implementations include test fakes and
// could include anything. One that annotated the map it was handed would
// otherwise corrupt every later invocation in the process.
func kindEnv() map[string]string {
	return map[string]string{kindExperimentalProviderEnv: RuntimeCLI}
}

// Mount is an extra bind mount into every cluster node container, for e.g.
// exposing a local image cache or CNI manifests directory.
type Mount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// PortMapping exposes a control-plane node port on the host.
type PortMapping struct {
	ContainerPort int32
	HostPort      int32
	// Protocol is "TCP" or "UDP". Empty defaults to TCP.
	Protocol string
}

// Config describes the ephemeral bootstrap Kind cluster.
type Config struct {
	// ClusterName is the kind cluster name (not the Kubernetes cluster name
	// in the site spec file — this is purely the kind/kubectl-context handle
	// for the ephemeral host cluster CWE itself runs on).
	ClusterName string

	// NodeImage overrides the kindest/node image kind uses for the
	// cluster's single node. Empty uses kind's own pinned default.
	NodeImage string

	// ExtraMounts and ExtraPortMappings are applied to the control-plane
	// node in the generated kind cluster config.
	ExtraMounts       []Mount
	ExtraPortMappings []PortMapping

	// WaitReady bounds how long `kind create cluster --wait` blocks for the
	// control-plane node to report Ready.
	WaitReady time.Duration

	// DockerHubMirror routes the node's docker.io pulls through a registry
	// mirror, given as a host with an optional port. Empty leaves the pulls
	// going to Docker Hub directly.
	DockerHubMirror string
}

// DefaultConfig reads the engine's knobs from the environment, mirroring
// cli/e2e/bootstrap/substrate.DefaultConfig's env-driven shape. Every value
// has a working default for a local or CI run.
func DefaultConfig() Config {
	return Config{
		ClusterName: envOr("NKECTL_BOOTSTRAP_CLUSTER_NAME", "nkectl-bootstrap"),
		NodeImage:   os.Getenv("NKECTL_BOOTSTRAP_NODE_IMAGE"),
		WaitReady:   envDuration("NKECTL_BOOTSTRAP_WAIT_READY", 3*time.Minute),

		DockerHubMirror: os.Getenv("NKECTL_BOOTSTRAP_DOCKERHUB_MIRROR"),
	}
}

// CommandRunner is the seam every kind/kubectl invocation goes through, so
// unit tests can substitute a fake instead of requiring a real kind binary
// and container runtime on the machine running `go test`.
type CommandRunner interface {
	// Run executes name with args, with env merged on top of the process
	// environment, and returns combined stdout+stderr. A non-nil error means
	// the command exited non-zero or failed to start; the returned output is
	// still populated on error so callers can surface it.
	Run(ctx context.Context, env map[string]string, name string, args ...string) (output string, err error)
}

// ExecCommandRunner is the real CommandRunner, backed by os/exec.
type ExecCommandRunner struct{}

// Run implements CommandRunner.
func (ExecCommandRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// Cluster is a created, running bootstrap Kind cluster.
type Cluster struct {
	Name           string
	KubeconfigPath string
}

// Engine drives the lifecycle of the ephemeral bootstrap Kind cluster.
type Engine struct {
	Runner CommandRunner
	log    *slog.Logger
}

// New returns an Engine backed by runner. A nil runner defaults to
// ExecCommandRunner{}. A nil logger defaults to slog.Default().
func New(runner CommandRunner, log *slog.Logger) *Engine {
	if runner == nil {
		runner = ExecCommandRunner{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{Runner: runner, log: log.With("component", "bootstrapengine")}
}

// Create stands up the ephemeral Kind cluster described by cfg and waits for
// it to be Ready. The caller must Delete it.
func (e *Engine) Create(ctx context.Context, cfg Config) (*Cluster, error) {
	if cfg.ClusterName == "" {
		return nil, fmt.Errorf("bootstrapengine: Config.ClusterName is required")
	}
	if err := validateRegistryMirror(cfg.DockerHubMirror); err != nil {
		return nil, err
	}

	exists, err := e.Exists(ctx, cfg.ClusterName)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("bootstrapengine: kind cluster %q already exists", cfg.ClusterName)
	}

	configPath, cleanup, err := writeKindConfig(cfg)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	args := []string{
		"create", "cluster",
		"--name", cfg.ClusterName,
		"--config", configPath,
	}
	if cfg.WaitReady > 0 {
		args = append(args, "--wait", cfg.WaitReady.String())
	}

	e.log.Info("creating bootstrap kind cluster", "cluster", cfg.ClusterName)
	if out, err := e.Runner.Run(ctx, kindEnv(), "kind", args...); err != nil {
		return nil, fmt.Errorf("kind create cluster %s: %w\n%s", cfg.ClusterName, err, out)
	}

	// Before anything pulls: the cluster exists from here on, so a failure
	// leaves it for the caller's Delete, like a failed kubeconfig fetch does.
	if cfg.DockerHubMirror != "" {
		if err := e.configureDockerHubMirror(ctx, cfg); err != nil {
			return nil, fmt.Errorf("bootstrap kind cluster %s created but the docker.io mirror was not configured: %w", cfg.ClusterName, err)
		}
	}

	kubeconfigPath, err := e.Kubeconfig(ctx, cfg.ClusterName)
	if err != nil {
		// The cluster came up but we could not retrieve its kubeconfig —
		// leave it running rather than silently deleting state the caller
		// might want to inspect; they can Delete explicitly once notified.
		return nil, fmt.Errorf("bootstrap kind cluster %s created but kubeconfig fetch failed: %w", cfg.ClusterName, err)
	}

	e.log.Info("bootstrap kind cluster ready", "cluster", cfg.ClusterName)
	return &Cluster{
		Name:           cfg.ClusterName,
		KubeconfigPath: kubeconfigPath,
	}, nil
}

// Delete tears down the named cluster. Deleting a cluster that does not
// exist is not an error — teardown has to be safe to call from a deferred
// cleanup even when Create failed partway through.
func (e *Engine) Delete(ctx context.Context, clusterName string) error {
	if clusterName == "" {
		return fmt.Errorf("bootstrapengine: clusterName is required")
	}

	e.log.Info("deleting bootstrap kind cluster", "cluster", clusterName)
	out, err := e.Runner.Run(ctx, kindEnv(), "kind", "delete", "cluster", "--name", clusterName)
	if err != nil {
		return fmt.Errorf("kind delete cluster %s: %w\n%s", clusterName, err, out)
	}
	return nil
}

// Exists reports whether a kind cluster named clusterName is currently
// registered with kind. kind's cluster listing is scoped to the provider that
// created it, the same way create/delete/load are, so this only sees clusters
// created through containerd.
func (e *Engine) Exists(ctx context.Context, clusterName string) (bool, error) {
	out, err := e.Runner.Run(ctx, kindEnv(), "kind", "get", "clusters")
	if err != nil {
		// kind returns exit code 1 with "No kind clusters found." on stderr
		// when none exist — that is not a failure of the query itself.
		if strings.Contains(out, "No kind clusters found") {
			return false, nil
		}
		return false, fmt.Errorf("kind get clusters: %w\n%s", err, out)
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == clusterName {
			return true, nil
		}
	}
	return false, nil
}

// Kubeconfig returns the path to a kubeconfig file for clusterName.
//
// kind writes the file, rather than this function capturing its stdout: Run
// merges stderr into stdout (see CommandRunner), and kind prints "using
// nerdctl due to KIND_EXPERIMENTAL_PROVIDER" on stderr. Captured, that line
// became line 1 of the kubeconfig and clients failed with "yaml: line 2:
// mapping values are not allowed in this context".
func (e *Engine) Kubeconfig(ctx context.Context, clusterName string) (string, error) {
	// A new directory, not a temp file: `kind export kubeconfig` merges into
	// whatever the path holds, and an empty file is not a kubeconfig.
	dir, err := os.MkdirTemp("", "nkectl-bootstrap-kubeconfig-"+clusterName+"-*")
	if err != nil {
		return "", fmt.Errorf("creating kubeconfig file for %s: %w", clusterName, err)
	}
	path := filepath.Join(dir, "config")

	out, err := e.Runner.Run(ctx, kindEnv(),
		"kind", "export", "kubeconfig", "--name", clusterName, "--kubeconfig", path)
	if err != nil {
		return "", fmt.Errorf("kind export kubeconfig %s: %w\n%s", clusterName, err, out)
	}
	return path, nil
}

// LoadImage loads a locally built image into every node of clusterName. It
// saves the image to a temporary archive first and hands kind that archive.
//
// `kind load docker-image` is unusable here: its "is the image present, and
// what is its ID" and "save it to a tarball" steps are hardcoded to the
// `docker` binary regardless of KIND_EXPERIMENTAL_PROVIDER — see
// https://github.com/kubernetes-sigs/kind/blob/main/pkg/cmd/kind/load/docker-image/docker-image.go.
// Only the final "load the tarball onto each node" step honors the provider.
// An image pulled via `nerdctl pull` was never in Docker's image store, so
// that command fails with "not present locally" even though the image
// genuinely exists. `kind load image-archive` has no such dependency: it
// opens a tarball the caller already produced and loads it through the
// provider abstraction.
func (e *Engine) LoadImage(ctx context.Context, clusterName, image string) error {
	if image == "" {
		return fmt.Errorf("bootstrapengine: image is required")
	}
	e.log.Info("loading image into bootstrap kind cluster",
		"cluster", clusterName, "image", image)

	archivePath, cleanup, err := saveImageArchive(ctx, e.Runner, image)
	if err != nil {
		return err
	}
	defer cleanup()

	out, err := e.Runner.Run(ctx, kindEnv(), "kind", "load", "image-archive", archivePath, "--name", clusterName)
	if err != nil {
		return fmt.Errorf("kind load image-archive %s into %s: %w\n%s", image, clusterName, err, out)
	}
	return nil
}

// saveImageArchive saves image to a temp tarball via `nerdctl save`. The
// caller must call cleanup once done with the file.
func saveImageArchive(ctx context.Context, runner CommandRunner, image string) (path string, cleanup func(), err error) {
	if err := flattenToHostPlatform(ctx, runner, RuntimeCLI, image); err != nil {
		return "", nil, err
	}

	f, err := os.CreateTemp("", "nkectl-bootstrap-image-*.tar")
	if err != nil {
		return "", nil, fmt.Errorf("creating image archive for %s: %w", image, err)
	}
	archivePath := f.Name()
	_ = f.Close()
	cleanup = func() { _ = os.Remove(archivePath) }

	if out, err := runner.Run(ctx, nil, RuntimeCLI, "save", "-o", archivePath, image); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%s save %s: %w\n%s", RuntimeCLI, image, err, out)
	}
	return archivePath, cleanup, nil
}

// flattenToHostPlatform points image at the single manifest for this host,
// the way `docker pull` already stores one.
//
// nerdctl stores a multi-arch tag as the whole manifest index, and `nerdctl
// save` writes that index out. `--platform` limits which blobs it writes, not
// what the index lists. kind imports on the node with `ctr images import
// --all-platforms`, so it reads every entry the index lists and fails on the
// platform nobody pulled: "content digest sha256:...: not found".
//
// A digest reference is already one manifest, and `tag` cannot target one, so
// it is left alone.
//
// Pulls the platform manifest by digest before retagging (see the comment on
// that call below) -- without it, `tag` fails with "not found" for a digest
// `image inspect` just resolved, confirmed against a real nerdctl-provider
// CI run: https://github.com/dsx-ai-factory/nke/actions/runs/33088745329.
func flattenToHostPlatform(ctx context.Context, runner CommandRunner, cli, image string) error {
	repo, ok := repoOfTaggedRef(image)
	if !ok {
		return nil
	}
	out, err := runner.Run(ctx, nil, cli, "image", "inspect", "--mode=native",
		"--format", "{{.Image.Target.Digest}} {{.ManifestDesc.Digest}}", image)
	if err != nil {
		return fmt.Errorf("%s image inspect %s: %w\n%s", cli, image, err, out)
	}
	// One line per store entry matching image, all the same pair.
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	target, manifest, ok := strings.Cut(line, " ")
	if !ok || !strings.HasPrefix(manifest, "sha256:") {
		return fmt.Errorf("%s image inspect %s: want two sha256 digests, got %q", cli, image, out)
	}
	// Equal means the tag already points at one manifest, so there is no
	// index to flatten. A locally built or loaded image is always this, and
	// has no digest name for `tag` to read anyway.
	if target == manifest {
		return nil
	}

	// `tag` resolves its source by looking up an existing named image entry
	// in the image store, not by walking arbitrary content-store blobs. The
	// plain pull that got image here only registered the index itself
	// (target, above) as a named entry -- the platform manifest content
	// exists (image inspect's ManifestDesc walk finds it), but nothing has
	// registered it as an image nerdctl can address by name, so `tag`
	// reports "not found" for it even though `inspect` just resolved it.
	// Pulling the digest directly registers it as its own image entry --
	// its layers are already in the local content store under the same
	// digests, so this only fetches the small manifest JSON, not the image
	// again -- which the following tag can then find.
	digestRef := repo + "@" + manifest
	if out, err := runner.Run(ctx, nil, cli, "pull", digestRef); err != nil {
		return fmt.Errorf("%s pull %s: %w\n%s", cli, digestRef, err, out)
	}

	// Retag image itself, not a new name: the node must end up holding the
	// reference the pod spec asks for.
	if out, err := runner.Run(ctx, nil, cli, "tag", digestRef, image); err != nil {
		return fmt.Errorf("%s tag %s %s: %w\n%s", cli, digestRef, image, err, out)
	}
	return nil
}

// repoOfTaggedRef splits the repository off a "repo:tag" reference. It reports
// false when the reference has no tag or pins a digest. A colon starts a tag
// only after the last slash; before it, it is a registry port.
func repoOfTaggedRef(image string) (repo string, ok bool) {
	lastSlash := strings.LastIndex(image, "/")
	if strings.Contains(image[lastSlash+1:], "@") {
		return "", false
	}
	i := strings.LastIndex(image, ":")
	if i <= lastSlash {
		return "", false
	}
	return image[:i], true
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
