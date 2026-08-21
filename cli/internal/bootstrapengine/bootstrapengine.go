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
// The container runtime front end (Docker or nerdctl/containerd) is a Config
// field, not a hardcoded assumption — this implements NKX-14787 ("use
// nerdctl/containerd as the container runtime for the bootstrap kind
// cluster") in the same package as the walking-skeleton create/delete flow,
// since both land through the same seam with nothing merging between them.
// kind's own provider abstraction (KIND_EXPERIMENTAL_PROVIDER) does the work
// of routing `kind create cluster`/`kind load docker-image` through nerdctl
// and containerd's content store instead of the Docker daemon; this package
// just plumbs the selection through consistently and keeps Docker available
// as the acceptance-criteria rollback path.
package bootstrapengine

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Provider selects the container runtime front end kind drives the bootstrap
// cluster through. kind treats every non-Docker provider as experimental;
// callers that hit provider-specific breakage have Docker as a documented
// rollback (NKX-14787 acceptance criteria).
type Provider string

const (
	// ProviderNerdctl drives kind through nerdctl/containerd. This is the
	// default: it matches the target platform's runtime and removes the
	// Docker daemon dependency (NKX-14787's motivation).
	ProviderNerdctl Provider = "nerdctl"
	// ProviderDocker drives kind through the Docker daemon. Kept selectable
	// as the rollback path until the nerdctl provider is proven out in CI.
	ProviderDocker Provider = "docker"
)

// kindExperimentalProviderEnv is the env var kind itself reads to select a
// non-default provider. See https://kind.sigs.k8s.io — "Ahead of Time"
// providers are experimental as of the kind versions this repo pins.
const kindExperimentalProviderEnv = "KIND_EXPERIMENTAL_PROVIDER"

// providerEnv returns the KIND_EXPERIMENTAL_PROVIDER value to export for p,
// or empty for the Docker provider, which is kind's built-in default and
// needs no override.
func (p Provider) providerEnv() string {
	if p == ProviderDocker {
		return ""
	}
	return string(p)
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
	// in the inventory file — this is purely the kind/kubectl-context handle
	// for the ephemeral host cluster CWE itself runs on).
	ClusterName string

	// Provider selects the container runtime front end. See Provider.
	Provider Provider

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
}

// DefaultConfig reads the engine's knobs from the environment, mirroring
// cli/e2e/bootstrap/substrate.DefaultConfig's env-driven shape. Every value
// has a working default for a local or CI run.
func DefaultConfig() Config {
	return Config{
		ClusterName: envOr("NKECTL_BOOTSTRAP_CLUSTER_NAME", "nkectl-bootstrap"),
		Provider:    Provider(envOr("NKECTL_BOOTSTRAP_PROVIDER", string(ProviderNerdctl))),
		NodeImage:   os.Getenv("NKECTL_BOOTSTRAP_NODE_IMAGE"),
		WaitReady:   envDuration("NKECTL_BOOTSTRAP_WAIT_READY", 3*time.Minute),
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
	Provider       Provider
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
	if cfg.Provider == "" {
		cfg.Provider = ProviderNerdctl
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

	env := providerEnvMap(cfg.Provider)
	e.log.Info("creating bootstrap kind cluster",
		"cluster", cfg.ClusterName, "provider", cfg.Provider)
	if out, err := e.Runner.Run(ctx, env, "kind", args...); err != nil {
		return nil, fmt.Errorf("kind create cluster %s (provider=%s): %w\n%s", cfg.ClusterName, cfg.Provider, err, out)
	}

	kubeconfigPath, err := e.Kubeconfig(ctx, cfg.ClusterName)
	if err != nil {
		// The cluster came up but we could not retrieve its kubeconfig —
		// leave it running rather than silently deleting state the caller
		// might want to inspect; they can Delete explicitly once notified.
		return nil, fmt.Errorf("bootstrap kind cluster %s created but kubeconfig fetch failed: %w", cfg.ClusterName, err)
	}

	e.log.Info("bootstrap kind cluster ready", "cluster", cfg.ClusterName, "provider", cfg.Provider)
	return &Cluster{
		Name:           cfg.ClusterName,
		Provider:       cfg.Provider,
		KubeconfigPath: kubeconfigPath,
	}, nil
}

// Delete tears down the named cluster. Deleting a cluster that does not
// exist is not an error — teardown has to be safe to call from a deferred
// cleanup even when Create failed partway through.
func (e *Engine) Delete(ctx context.Context, clusterName string, provider Provider) error {
	if clusterName == "" {
		return fmt.Errorf("bootstrapengine: clusterName is required")
	}
	if provider == "" {
		provider = ProviderNerdctl
	}

	e.log.Info("deleting bootstrap kind cluster", "cluster", clusterName, "provider", provider)
	env := providerEnvMap(provider)
	out, err := e.Runner.Run(ctx, env, "kind", "delete", "cluster", "--name", clusterName)
	if err != nil {
		return fmt.Errorf("kind delete cluster %s (provider=%s): %w\n%s", clusterName, provider, err, out)
	}
	return nil
}

// Exists reports whether a kind cluster named clusterName is currently
// registered with kind.
func (e *Engine) Exists(ctx context.Context, clusterName string) (bool, error) {
	out, err := e.Runner.Run(ctx, nil, "kind", "get", "clusters")
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

// Kubeconfig returns the path to a kubeconfig file for clusterName, writing
// it out via `kind get kubeconfig` if not already cached on disk.
func (e *Engine) Kubeconfig(ctx context.Context, clusterName string) (string, error) {
	out, err := e.Runner.Run(ctx, nil, "kind", "get", "kubeconfig", "--name", clusterName)
	if err != nil {
		return "", fmt.Errorf("kind get kubeconfig %s: %w\n%s", clusterName, err, out)
	}

	f, err := os.CreateTemp("", "nkectl-bootstrap-kubeconfig-"+clusterName+"-*.yaml")
	if err != nil {
		return "", fmt.Errorf("creating kubeconfig file for %s: %w", clusterName, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(out); err != nil {
		return "", fmt.Errorf("writing kubeconfig file for %s: %w", clusterName, err)
	}
	return f.Name(), nil
}

// LoadImage loads a locally built image into every node of clusterName.
// Under both providers this goes through `kind load docker-image`: kind's
// own provider abstraction (selected via KIND_EXPERIMENTAL_PROVIDER) routes
// the load through nerdctl and containerd's content store when the nerdctl
// provider is selected, rather than the Docker daemon — see NKX-14787's
// "update the local image build and load path" scope item.
func (e *Engine) LoadImage(ctx context.Context, clusterName string, provider Provider, image string) error {
	if image == "" {
		return fmt.Errorf("bootstrapengine: image is required")
	}
	if provider == "" {
		provider = ProviderNerdctl
	}
	env := providerEnvMap(provider)
	e.log.Info("loading image into bootstrap kind cluster",
		"cluster", clusterName, "provider", provider, "image", image)
	out, err := e.Runner.Run(ctx, env, "kind", "load", "docker-image", image, "--name", clusterName)
	if err != nil {
		return fmt.Errorf("kind load docker-image %s into %s (provider=%s): %w\n%s", image, clusterName, provider, err, out)
	}
	return nil
}

// providerEnvMap returns the environment overrides Run should apply for the
// given provider. Empty for Docker, which needs no override.
func providerEnvMap(p Provider) map[string]string {
	v := p.providerEnv()
	if v == "" {
		return nil
	}
	return map[string]string{kindExperimentalProviderEnv: v}
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
