// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package engine drives the lifecycle of the ephemeral Kind cluster
// that `nvkectl bootstrap site` runs the controller on: create it, wait for it to be
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
package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
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

	// netconfPathEnv is the environment form of nerdctl's --cni-netconfpath:
	// the directory nerdctl writes its network configs to.
	netconfPathEnv = "NETCONFPATH"

	// SharedCNIConfDir is the CNI configuration directory the node's own
	// containerd reads, and so the one the bootstrap runtime must not write to.
	SharedCNIConfDir = "/etc/cni/net.d"

	// IsolatedCNIConfDir is where the bootstrap runtime's network configs go
	// instead of SharedCNIConfDir.
	IsolatedCNIConfDir = "/etc/containerd-nerdctl/net.d"

	// createCleanupTimeout bounds each of the diagnostics and the teardown that follow a
	// failed create. They run on a context detached from the caller's, so they
	// need a stop of their own.
	createCleanupTimeout = 2 * time.Minute
)

// kindEnv returns the environment a kind invocation runs with, so kind
// resolves clusters and images through containerd rather than its default.
//
// A fresh map per call, not a package-level one: the result crosses the
// CommandRunner interface, whose implementations include test fakes and
// could include anything. One that annotated the map it was handed would
// otherwise corrupt every later invocation in the process.
//
// It also moves nerdctl's network configs out of SharedCNIConfDir, which is the
// node containerd's own conf_dir. Left there, the bootstrap bridge is the only
// CNI config on the node between kubeadm init and the CNI plugin writing its
// own, so the first pods are sandboxed on the bootstrap bridge and keep that
// address: CoreDNS then answers on a network the cluster's pods cannot reach.
func kindEnv() map[string]string {
	return kindEnvAs(os.Geteuid(), os.Getenv(netconfPathEnv))
}

// kindEnvAs is kindEnv for a given effective user and NETCONFPATH. An operator's
// NETCONFPATH wins unless it names SharedCNIConfDir, which is the directory this
// exists to avoid and is replaced like an unset value. Rootless nerdctl keeps its
// own per-user directory, which never reaches the node containerd.
func kindEnvAs(euid int, netconfPath string) map[string]string {
	env := map[string]string{kindExperimentalProviderEnv: RuntimeCLI}
	if netconfPath != "" && filepath.Clean(netconfPath) == SharedCNIConfDir {
		netconfPath = ""
	}
	if euid == 0 && netconfPath == "" {
		env[netconfPathEnv] = IsolatedCNIConfDir
	}
	return env
}

// NerdctlCNIConfigs returns the nerdctl network configs in dir, in name order.
// A missing directory has none; any other failure to read it is an error, since
// an unreadable directory has not been shown to be clear.
func NerdctlCNIConfigs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing nerdctl CNI configs in %s: %w", dir, err)
	}
	var paths []string
	for _, entry := range entries {
		if ok, _ := filepath.Match("nerdctl-*.conflist", entry.Name()); ok {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	return paths, nil
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
	// ListenAddress is the host address the port is bound to. Empty binds
	// every address.
	ListenAddress string
}

// Config describes the ephemeral bootstrap Kind cluster.
type Config struct {
	// ClusterName is the kind cluster name (not the Kubernetes cluster name
	// in the site spec file — this is purely the kind/kubectl-context handle
	// for the ephemeral host cluster the controller itself runs on).
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

	// DiagnosticsDir is where a failed `kind create cluster` leaves what the
	// half-built node container had to say. Empty skips the collection.
	DiagnosticsDir string

	// KeepFailedCluster leaves the node container of a failed create in place
	// instead of deleting it once diagnostics are collected.
	KeepFailedCluster bool
}

// DefaultConfig reads the engine's knobs from the environment, mirroring
// cli/e2e/bootstrap/substrate.DefaultConfig's env-driven shape. Every value
// has a working default for a local or CI run.
func DefaultConfig() Config {
	return Config{
		ClusterName: envOr("NVKECTL_BOOTSTRAP_CLUSTER_NAME", sitespec.DefaultBootstrapClusterName),
		NodeImage:   envalias.Get("NVKECTL_BOOTSTRAP_NODE_IMAGE"),
		WaitReady:   envDuration("NVKECTL_BOOTSTRAP_WAIT_READY", 3*time.Minute),

		DockerHubMirror: envalias.Get("NVKECTL_BOOTSTRAP_DOCKERHUB_MIRROR"),
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
		// --retain suppresses kind's own cleanup of a failed create. Without
		// it kind deletes the node container as it exits, which takes the
		// kubelet journal and the control-plane static pod logs with it — so
		// a `kubeadm init` that never brings the API server up reports only
		// "connection refused" and leaves nothing to explain why. The failure
		// path below collects those and then deletes, so a caller sees the
		// same end state as before, minus the missing evidence.
		"--retain",
	}
	if cfg.WaitReady > 0 {
		args = append(args, "--wait", cfg.WaitReady.String())
	}

	e.log.Info("creating bootstrap kind cluster", "bootstrap_kind_cluster", cfg.ClusterName)
	if out, err := e.Runner.Run(ctx, kindEnv(), "kind", args...); err != nil {
		// A SIGINT during `kind create` cancels ctx, and the runner execs
		// through exec.CommandContext, so reusing ctx would skip the
		// diagnostics and the delete and leave the retained node container
		// standing. Detach from the cancellation, and give the diagnostics and
		// the delete a bound each so slow diagnostics cannot eat the delete's
		// budget.
		diagnosticsCtx, cancelDiagnostics := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
		e.dumpCreateFailure(diagnosticsCtx, cfg)
		cancelDiagnostics()
		deleteCtx, cancelDelete := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
		defer cancelDelete()
		if cfg.KeepFailedCluster {
			e.log.Warn("--keep-bootstrap-cluster set: leaving the failed bootstrap kind cluster in place",
				"bootstrap_kind_cluster", cfg.ClusterName)
		} else if deleteErr := e.Delete(deleteCtx, cfg.ClusterName); deleteErr != nil {
			e.log.Error("could not clean up after a failed create",
				"bootstrap_kind_cluster", cfg.ClusterName, "err", deleteErr)
		}
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

	e.log.Info("bootstrap kind cluster ready", "bootstrap_kind_cluster", cfg.ClusterName)
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

	e.log.Info("deleting bootstrap kind cluster", "bootstrap_kind_cluster", clusterName)
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
	dir, err := os.MkdirTemp("", "nvkectl-bootstrap-kubeconfig-"+clusterName+"-*")
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

// LoadImage loads image from the host's image store into every node of
// clusterName through a temporary archive. It may change the host's store
// first: a multi-platform tag is retagged to this host's manifest, and an
// image pinned by digest gets a <repo>:nvkectl-<hex> tag and, for an index,
// every platform pulled.
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
		"bootstrap_kind_cluster", clusterName, "image", image)

	archivePath, cleanup, err := saveImageArchive(ctx, e.Runner, e.log, image)
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
func saveImageArchive(ctx context.Context, runner CommandRunner, log *slog.Logger, image string) (path string, cleanup func(), err error) {
	if err := flattenToHostPlatform(ctx, runner, RuntimeCLI, image); err != nil {
		return "", nil, err
	}
	flags, names, err := prepareDigestRef(ctx, runner, log, RuntimeCLI, image)
	if err != nil {
		return "", nil, err
	}

	f, err := os.CreateTemp("", "nvkectl-bootstrap-image-*.tar")
	if err != nil {
		return "", nil, fmt.Errorf("creating image archive for %s: %w", image, err)
	}
	archivePath := f.Name()
	_ = f.Close()
	cleanup = func() { _ = os.Remove(archivePath) }

	args := append(append([]string{"save"}, flags...), "-o", archivePath)
	if out, err := runner.Run(ctx, nil, RuntimeCLI, append(args, names...)...); err != nil {
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
// A digest reference is left alone: retagging its name would point it at a
// different digest. prepareDigestRef readies those instead.
//
// Pulls the platform manifest by digest before retagging (see the comment on
// that call below) -- without it, `tag` fails with "not found" for a digest
// `image inspect` just resolved, confirmed against a real nerdctl-provider
// CI run: https://github.com/dsx-ai-factory/nvke/actions/runs/33088745329.
func flattenToHostPlatform(ctx context.Context, runner CommandRunner, cli, image string) error {
	repo, ok := repoOfTaggedRef(image)
	if !ok {
		return nil
	}
	target, manifest, err := inspectTargetAndManifest(ctx, runner, cli, image)
	if err != nil {
		return err
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

// prepareDigestRef prepares an image pinned by a sha256 digest for `nerdctl
// save`. It tags the image <repo>:nvkectl-<first 12 hex digits> in the local
// image store and, when the digest names a multi-platform index, pulls every
// platform the index lists. It returns the save flags and the names to save,
// the image and that tag. Any other reference is returned under its own name
// with no flags.
func prepareDigestRef(ctx context.Context, runner CommandRunner, log *slog.Logger, cli, image string) (flags, names []string, err error) {
	tag, ok := localTagForDigestRef(image)
	if !ok {
		return nil, []string{image}, nil
	}
	target, manifest, err := inspectTargetAndManifest(ctx, runner, cli, image)
	if err != nil {
		return nil, nil, err
	}
	// The earlier pull fetched an index for this host's platform only, and
	// kind imports with `ctr images import --all-platforms`, so the archive
	// must carry every platform the index lists.
	if target != manifest {
		log.Info("pulling every platform of the pinned image index; kind imports them all",
			"image", image, "index", target)
		if out, err := runner.Run(ctx, nil, cli, "pull", "--all-platforms", image); err != nil {
			return nil, nil, fmt.Errorf("%s pull --all-platforms %s: %w\n%s", cli, image, err, out)
		}
		flags = []string{"--all-platforms"}
	}
	// kind's import also names the archive import-<date>@sha256:<digest of the
	// archive's index>. The node's CRI uses an image's first reference, tagged
	// ones first, in normalized form: for a digest-only image that can be
	// docker.io/library/import-<date>@..., a name containerd never stored, and
	// creating the container fails with "failed to check if this is a
	// checkpoint image". A tagged name always sorts first.
	log.Info("tagging the digest-pinned image so the node can resolve it", "image", image, "tag", tag)
	if out, err := runner.Run(ctx, nil, cli, "tag", image, tag); err != nil {
		return nil, nil, fmt.Errorf("%s tag %s %s: %w\n%s", cli, image, tag, err, out)
	}
	return flags, []string{image, tag}, nil
}

// inspectTargetAndManifest returns the digest image points at and the digest
// of the manifest for this host's platform. The two differ when image is a
// multi-arch index.
func inspectTargetAndManifest(ctx context.Context, runner CommandRunner, cli, image string) (target, manifest string, err error) {
	out, err := runner.Run(ctx, nil, cli, "image", "inspect", "--mode=native",
		"--format", "{{.Image.Target.Digest}} {{.ManifestDesc.Digest}}", image)
	if err != nil {
		return "", "", fmt.Errorf("%s image inspect %s: %w\n%s", cli, image, err, out)
	}
	// One line per store entry matching image, all the same pair.
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	target, manifest, ok := strings.Cut(line, " ")
	if !ok || !strings.HasPrefix(manifest, "sha256:") {
		return "", "", fmt.Errorf("%s image inspect %s: want two sha256 digests, got %q", cli, image, out)
	}
	return target, manifest, nil
}

// localTagForDigestRef returns "<repo>:nvkectl-<first 12 hex digits>" for an
// image pinned by a sha256 digest, "repo[:tag]@sha256:<hex>", and false for
// any other reference.
func localTagForDigestRef(image string) (string, bool) {
	repo, digest, ok := strings.Cut(image, "@")
	if !ok {
		return "", false
	}
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hex) != 64 {
		return "", false
	}
	// A reference carries one tag: drop any tag beside the digest.
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo + ":nvkectl-" + hex[:12], true
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

// Settings is every environment variable this package reads; each is also
// read under its previous NKECTL_* name for one release (see envalias).
var Settings = []string{
	"NVKECTL_BOOTSTRAP_CLUSTER_NAME",
	"NVKECTL_BOOTSTRAP_NODE_IMAGE",
	"NVKECTL_BOOTSTRAP_WAIT_READY",
	"NVKECTL_BOOTSTRAP_DOCKERHUB_MIRROR",
}

func envOr(key, fallback string) string {
	if v := envalias.Get(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := envalias.Get(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
