// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cwedeploy stands up Postgres, Temporal and the CWE worker onto a
// Kubernetes cluster (the ephemeral bootstrap Kind cluster
// cli/internal/bootstrapengine creates), then waits for CWE to report
// healthy on /readyz.
//
// It reuses controller/deploy/charts/cloud-workflow-engine (the real,
// actively maintained Helm chart) for CWE itself, and the same Postgres/
// Temporal shape controller/Tiltfile already drives for local development
// (the Bitnami postgresql chart + the official go.temporal.io/helm-charts
// chart) — see manifests/*.yaml for where each values file was ported from.
// This is a deliberate reuse of existing patterns rather than a port of the
// GitLab spike's raw kubectl-apply manifests.
//
// Every helm/kubectl invocation goes through the CommandRunner seam so unit
// tests can substitute a fake, the same pattern
// cli/internal/bootstrapengine uses for kind/kubectl.
package cwedeploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Namespaces each release is installed into. Fixed, not configurable:
	// the embedded values files hard-code the in-cluster DNS names that
	// depend on these (e.g. postgres-postgresql.postgres.svc.cluster.local),
	// so the namespace and the values file have to change together.
	postgresNamespace = "postgres"
	temporalNamespace = "temporal"
	cweNamespace      = "cloud-workflow-engine"

	postgresReleaseName = "postgres"
	temporalReleaseName = "temporal"
	cweReleaseName      = "cloud-workflow-engine"

	// postgresChartRef is the Bitnami postgresql chart, pulled over OCI to
	// avoid the classic HTTP repo's full index.yaml download. Version pin
	// matches controller/Tiltfile's CHART_VERSION_POSTGRESQL.
	postgresChartRef     = "oci://registry-1.docker.io/bitnamicharts/postgresql"
	postgresChartVersion = "18.5.15"

	// temporalChartRepo/temporalChartName are passed via `helm ... --repo`
	// rather than a `helm repo add`, so this package does not mutate the
	// caller's global Helm repo list. Version pin matches controller/
	// Tiltfile's CHART_VERSION_TEMPORAL.
	temporalChartRepo    = "https://go.temporal.io/helm-charts"
	temporalChartName    = "temporal"
	temporalChartVersion = "0.74.0"

	// readyzPath is polled through the API server's service proxy rather
	// than a port-forward, so no local port has to be reserved or cleaned
	// up. controller/internal/server/health.go serves it from the CWE pod
	// itself: DB Ping + Temporal CheckHealth both have to succeed.
	readyzPath = "/readyz"
	cweSvcPort = "http"
)

// waitReadyPollInterval is the delay between /readyz polls. A var, not a
// const, so unit tests can shrink it instead of paying real wall-clock time
// for a retry loop.
var waitReadyPollInterval = 3 * time.Second

// Config describes a CWE deployment.
type Config struct {
	// CWEImage is the full image reference to deploy, e.g.
	// "nvcr.io/<org>/controller:edge" (published by
	// .github/workflows/image-nvcr.yml on every push to main -- see that
	// workflow for how the tag is resolved). Required; this package does not
	// default it; a registry org identifier does not belong hardcoded in a
	// tracked file of a repository being prepared to go public.
	CWEImage string

	// ChartPath is the local filesystem path to
	// controller/deploy/charts/cloud-workflow-engine. Empty triggers
	// repo-root discovery from the working directory (see resolveChartPath).
	// This is a real gap for nkectl shipped as a standalone binary outside a
	// repository checkout: the chart is not yet published anywhere
	// (chart-publish.yml only fires on controller/v* release tags, and none
	// exist yet). Set explicitly once that changes, pointing at an OCI
	// chart ref instead.
	ChartPath string

	// PostgresPassword is the Postgres superuser password shared by the
	// Postgres, Temporal and CWE releases. Generated if empty.
	PostgresPassword string

	// HelmTimeout bounds each `helm upgrade --install --wait` call.
	HelmTimeout time.Duration

	// ReadyTimeout bounds how long WaitReady polls /readyz before giving up.
	ReadyTimeout time.Duration
}

// DefaultConfig reads the package's knobs from the environment, mirroring
// bootstrapengine.DefaultConfig's env-driven shape.
func DefaultConfig() Config {
	return Config{
		CWEImage:     os.Getenv("NKECTL_CWE_IMAGE"),
		ChartPath:    os.Getenv("NKECTL_CWE_CHART_PATH"),
		HelmTimeout:  envDuration("NKECTL_CWE_HELM_TIMEOUT", 5*time.Minute),
		ReadyTimeout: envDuration("NKECTL_CWE_READY_TIMEOUT", 5*time.Minute),
	}
}

// CommandRunner is the seam every helm/kubectl invocation goes through, so
// unit tests can substitute a fake instead of requiring real binaries and a
// real cluster.
type CommandRunner interface {
	// Run executes name with args and returns combined stdout+stderr. A
	// non-nil error means the command exited non-zero or failed to start;
	// the returned output is still populated on error so callers can
	// surface it.
	Run(ctx context.Context, name string, args ...string) (output string, err error)
}

// ExecCommandRunner is the real CommandRunner, backed by os/exec.
type ExecCommandRunner struct{}

// Run implements CommandRunner.
func (ExecCommandRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Engine deploys CWE and waits for it to report healthy.
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
	return &Engine{Runner: runner, log: log.With("component", "cwedeploy")}
}

// Deploy installs Postgres, Temporal and CWE (in that order -- each depends
// on the one before it) onto the cluster kubeconfigPath points at, waiting
// for each Helm release to report ready before moving to the next. It does
// not wait for CWE's own /readyz; call WaitReady for that once Deploy
// returns, so callers can log "helm release installed" and
// "CWE reports healthy" as distinct, separately-timed steps.
func (e *Engine) Deploy(ctx context.Context, kubeconfigPath string, cfg Config) error {
	if kubeconfigPath == "" {
		return errors.New("cwedeploy: kubeconfigPath is required")
	}
	if cfg.CWEImage == "" {
		return errors.New("cwedeploy: Config.CWEImage is required " +
			"(e.g. nvcr.io/<org>/controller:edge -- see .github/workflows/image-nvcr.yml)")
	}

	chartPath := cfg.ChartPath
	if chartPath == "" {
		var err error
		chartPath, err = resolveChartPath()
		if err != nil {
			return err
		}
	}

	password := cfg.PostgresPassword
	if password == "" {
		var err error
		password, err = randomPassword()
		if err != nil {
			return fmt.Errorf("cwedeploy: generating postgres password: %w", err)
		}
	}

	helmTimeout := cfg.HelmTimeout
	if helmTimeout <= 0 {
		helmTimeout = 5 * time.Minute
	}

	postgresValues, cleanup, err := writeEmbeddedFile(postgresValuesFile)
	if err != nil {
		return err
	}
	defer cleanup()

	e.log.Info("installing postgres", "namespace", postgresNamespace, "version", postgresChartVersion)
	if _, err := e.helm(ctx, kubeconfigPath,
		"upgrade", "--install", postgresReleaseName, postgresChartRef,
		"--namespace", postgresNamespace, "--create-namespace",
		"--values", postgresValues,
		"--set-string", "auth.password="+password,
		"--version", postgresChartVersion,
		"--wait", "--timeout", helmTimeout.String(),
	); err != nil {
		return fmt.Errorf("cwedeploy: installing postgres: %w", err)
	}

	temporalValues, cleanup, err := writeEmbeddedFile(temporalValuesFile)
	if err != nil {
		return err
	}
	defer cleanup()

	e.log.Info("installing temporal", "namespace", temporalNamespace, "version", temporalChartVersion)
	if _, err := e.helm(ctx, kubeconfigPath,
		"upgrade", "--install", temporalReleaseName, temporalChartName,
		"--repo", temporalChartRepo,
		"--namespace", temporalNamespace, "--create-namespace",
		"--values", temporalValues,
		"--set-string", "server.config.persistence.default.sql.password="+password,
		"--set-string", "server.config.persistence.visibility.sql.password="+password,
		"--version", temporalChartVersion,
		"--wait", "--timeout", helmTimeout.String(),
	); err != nil {
		return fmt.Errorf("cwedeploy: installing temporal: %w", err)
	}

	cweValues, cleanup, err := writeEmbeddedFile(cweValuesFile)
	if err != nil {
		return err
	}
	defer cleanup()

	imageRepo, imageTag := splitImageRef(cfg.CWEImage)
	e.log.Info("installing cwe", "namespace", cweNamespace, "image", cfg.CWEImage, "chart", chartPath)
	if _, err := e.helm(ctx, kubeconfigPath,
		"upgrade", "--install", cweReleaseName, chartPath,
		"--namespace", cweNamespace, "--create-namespace",
		"--values", cweValues,
		"--set-string", "appDb.postgresPassword="+password,
		"--set-string", "image.repository="+imageRepo,
		"--set-string", "image.tag="+imageTag,
		"--set-string", "image.pullPolicy=IfNotPresent",
		"--wait", "--timeout", helmTimeout.String(),
	); err != nil {
		return fmt.Errorf("cwedeploy: installing cwe: %w", err)
	}

	return nil
}

// readyzResponse mirrors controller/internal/server/health.go's
// healthResponse.
type readyzResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// WaitReady polls CWE's /readyz endpoint through the Kubernetes API server's
// service proxy (no port-forward needed) until it reports healthy or timeout
// elapses. Returns an error naming the last observed state on timeout, so a
// failure is diagnosable from CI output alone.
func (e *Engine) WaitReady(ctx context.Context, kubeconfigPath string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	proxyPath := fmt.Sprintf("/api/v1/namespaces/%s/services/%s:%s/proxy%s",
		cweNamespace, cweReleaseName, cweSvcPort, readyzPath)

	var lastOut string
	var lastErr error
	for {
		out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath, "get", "--raw", proxyPath)
		lastOut, lastErr = out, err
		if err == nil {
			var resp readyzResponse
			if jsonErr := json.Unmarshal([]byte(out), &resp); jsonErr == nil && resp.Status == "ok" {
				e.log.Info("cwe reports healthy", "checks", resp.Checks)
				return nil
			}
		}

		select {
		case <-ctx.Done():
			lastErrStr := "<none>"
			if lastErr != nil {
				lastErrStr = lastErr.Error()
			}
			return fmt.Errorf("cwedeploy: cwe did not report healthy on %s within %s: last error=%s last response=%q",
				readyzPath, timeout, lastErrStr, strings.TrimSpace(lastOut))
		case <-time.After(waitReadyPollInterval):
		}
	}
}

// helm runs the helm CLI with kubeconfigPath and args.
func (e *Engine) helm(ctx context.Context, kubeconfigPath string, args ...string) (string, error) {
	fullArgs := append([]string{"--kubeconfig", kubeconfigPath}, args...)
	out, err := e.Runner.Run(ctx, "helm", fullArgs...)
	if err != nil {
		return out, fmt.Errorf("helm %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

// writeEmbeddedFile copies an embedded values file to a temporary file on
// disk (helm's -f/--values flag needs a real path) and returns a cleanup
// func the caller should defer immediately.
func writeEmbeddedFile(name string) (path string, cleanup func(), err error) {
	body, err := manifestsFS.ReadFile(name)
	if err != nil {
		return "", func() {}, fmt.Errorf("cwedeploy: reading embedded %s: %w", name, err)
	}
	f, err := os.CreateTemp("", "nkectl-cwedeploy-*.yaml")
	if err != nil {
		return "", func() {}, fmt.Errorf("cwedeploy: creating values file for %s: %w", name, err)
	}
	tmpName := f.Name()
	cleanup = func() { _ = os.Remove(tmpName) }
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("cwedeploy: writing values file for %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("cwedeploy: closing values file for %s: %w", name, err)
	}
	return tmpName, cleanup, nil
}

// resolveChartPath locates controller/deploy/charts/cloud-workflow-engine by
// walking up from the current working directory looking for go.work (the
// repository root marker). This only works inside a checkout of this
// monorepo -- see the ChartPath doc comment for why that is a known gap, not
// an oversight.
func resolveChartPath() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cwedeploy: resolving chart path: %w", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			chart := filepath.Join(dir, "controller", "deploy", "charts", "cloud-workflow-engine")
			if _, statErr := os.Stat(chart); statErr == nil {
				return chart, nil
			}
			return "", fmt.Errorf("cwedeploy: found repository root at %s but no chart at %s", dir, chart)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("cwedeploy: Config.ChartPath is empty and no repository root " +
				"(go.work) was found above the working directory; set NKECTL_CWE_CHART_PATH explicitly")
		}
		dir = parent
	}
}

// splitImageRef splits "repo:tag" on the last colon, tolerating a
// repo that itself contains a colon (a registry port, e.g.
// "localhost:5000/controller:edge").
func splitImageRef(ref string) (repo, tag string) {
	idx := strings.LastIndex(ref, ":")
	if idx < 0 || strings.Contains(ref[idx:], "/") {
		return ref, "latest"
	}
	return ref[:idx], ref[idx+1:]
}

// randomPassword returns a 32-character hex string suitable for the shared
// Postgres superuser password used only within this ephemeral cluster's
// lifetime.
func randomPassword() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
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
