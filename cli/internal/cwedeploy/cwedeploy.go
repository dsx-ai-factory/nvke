// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cwedeploy stands up Postgres, Temporal, SPIRE, and the CWE worker onto a
// Kubernetes cluster (the ephemeral bootstrap Kind cluster
// cli/internal/bootstrapengine creates), then waits for CWE to report
// healthy on /readyz.
//
// It reuses controller/deploy/charts/controller (the real,
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
	spireNamespace    = "spire-server"
	cweNamespace      = "cloud-workflow-engine"

	postgresReleaseName  = "postgres"
	temporalReleaseName  = "temporal"
	spireCRDsReleaseName = "spire-crds"
	spireReleaseName     = "spire"
	cweReleaseName       = "cloud-workflow-engine"

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

	// The SPIFFE project's hardened SPIRE charts, passed via `helm ... --repo`
	// for the same reason Temporal is: this package does not mutate the
	// caller's global Helm repo list. Everything here is public, so a
	// bootstrap needs no registry credential to install it.
	//
	// The CRDs are their own release because a chart cannot depend on its own
	// CRDs being applied first.
	spireChartRepo        = "https://spiffe.github.io/helm-charts-hardened/"
	spireChartName        = "spire"
	spireChartVersion     = "0.30.1"
	spireCRDsChartName    = "spire-crds"
	spireCRDsChartVersion = "0.6.1"

	// spireServerPort is the port the SPIRE server is reached on inside the
	// cluster: the port its Service listens on, the port its own agents dial,
	// and the port CWE is told to dial.
	//
	// 8081 rather than the chart's own default of 443, so that CWE's
	// SPIRE_SERVER_API_ADDRESS -- the address it uses for join-token
	// operations, which it reads separately from the address above -- is
	// correct without being configured at all: it defaults to
	// spire-server.spire-server.svc.cluster.local:8081, which is exactly this
	// Service. Change this and that default stops matching.
	//
	// It has to be set on the agent as well as on the Service. The chart keeps
	// the agent's server port as its own value defaulting to 443, and does not
	// derive it from the Service, so moving one without the other leaves the
	// agent dialling a port the Service does not publish. A ClusterIP silently
	// blackholes traffic to an undeclared port rather than refusing it, so that
	// mistake surfaces as a node-attestation i/o timeout rather than as
	// anything naming a port.
	//
	// Reaching the server from outside the cluster is
	// spire-server-nodeport.yaml, not this.
	spireServerPort = "8081"

	// spireControllerManagerClass is the class the SPIRE controller manager
	// claims objects under, and the class CWE stamps on the ClusterStaticEntry
	// objects it writes. The two have to be the same string: the controller
	// manager ignores an entry whose class is not its own, so a mismatch means
	// CWE writes entries nobody acts on and a node never gets its identity.
	//
	// Neither side's default gets there on its own. The chart derives
	// <namespace>-<release> when className is empty, which here would be
	// "spire-server-spire", while CWE defaults to "spire-mgmt-spire". So both
	// are set explicitly, from this one constant, rather than left to agree by
	// coincidence. The value is CWE's default, which keeps it the same string
	// a site controller already uses.
	spireControllerManagerClass = "spire-mgmt-spire"

	// spireAgentValuesPrefix is where the hardened chart keeps its agent's
	// settings, which are a sibling of the server's rather than inside them.
	spireAgentValuesPrefix = "spire-agent"

	// spireServerValuesPrefix is where the hardened chart keeps its server's
	// settings. A value set at the wrong depth is silently ignored by helm
	// rather than rejected, so the values computed below are all addressed
	// through it.
	spireServerValuesPrefix = "spire-server"

	// readyzPath is polled through the API server's service proxy rather
	// than a port-forward, so no local port has to be reserved or cleaned
	// up. controller/internal/server/health.go serves it from the CWE pod
	// itself: DB Ping + Temporal CheckHealth both have to succeed.
	readyzPath = "/readyz"
	cweSvcPort = "http"

	// profilesPath is where the init container lands the profiles tree and
	// where CWE reads it from (PROFILES_DIR).
	profilesPath = "/profiles"
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

	// ProfilesImage holds the OS/config profiles tree at /profiles, which the
	// chart's materialize-profiles init container copies into CWE. Required:
	// a schema migration reads osProfiles/metadata.yaml, so CWE crash-loops
	// without it. Fleet profiles come from nke-site-deploy;
	// controller/test/fixtures is the sanitized stand-in.
	ProfilesImage string

	// SPIFFEClusterName identifies the ephemeral Kind cluster to SPIRE. It is
	// the Kind cluster's own name, which is the only name this cluster has.
	// Empty defaults to "nkectl-bootstrap", matching bootstrapengine's default.
	SPIFFEClusterName string

	// SPIFFETrustDomain is the trust domain used for CWE's workload identity.
	// Empty defaults to <SPIFFEClusterName>.nke.local.
	SPIFFETrustDomain string

	// SPIFFEUpstreamCACertPath and SPIFFEUpstreamCAKeyPath are a CA the
	// operator brings to the bootstrap, from the inventory's spire section.
	// SPIRE chains its own CA to it, so a host that was given the certificate
	// ahead of time can verify the server the first time it speaks to it,
	// instead of trusting it blindly.
	//
	// Not read from the environment: this describes the site, so the inventory
	// is the one place it comes from.
	SPIFFEUpstreamCACertPath string
	SPIFFEUpstreamCAKeyPath  string

	// TPMEKFingerprints are the SHA-256 endorsement-key thumbprints of the
	// hosts allowed to attest to this SPIRE server, seeded into the TPM node
	// attestor's allowlist. They come from the inventory, one per node, which
	// is where the operator declares which machines it is entitled to admit.
	// Empty leaves the attestor off, which is what a bootstrap that was told
	// about no hardware identities wants.
	//
	// When set, the server is also published on a NodePort: the hosts that
	// attest are outside the cluster, and a ClusterIP is not a route they have.
	TPMEKFingerprints []string

	// ChartPath is the local filesystem path to
	// controller/deploy/charts/controller. Empty triggers
	// repo-root discovery from the working directory (see resolveChartPath).
	// This is a real gap for nkectl shipped as a standalone binary outside a
	// repository checkout: the chart is not yet published anywhere
	// (chart-publish.yml only fires on controller/v* release tags, and none
	// exist yet). Set explicitly once that changes, pointing at an OCI
	// chart ref instead.
	ChartPath string

	// ProvisionedProviderImage is the cluster-api-provider-provisioned image to
	// run as the CAPI infrastructure provider for hosts that already exist. If
	// it is empty, Deploy installs only CAPI core and the kubeadm providers.
	// Nothing publishes this image yet, so the caller builds it and loads it into
	// the cluster rather than pulling it.
	ProvisionedProviderImage string

	// ProvisionedProviderPath is the local path to
	// cluster-api-provider-provisioned/config/default. Empty triggers
	// repo-root discovery, with the same standalone-binary caveat as ChartPath.
	ProvisionedProviderPath string

	// PostgresPassword is the Postgres superuser password shared by the
	// Postgres, Temporal, and CWE releases. Generated if empty.
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
		CWEImage:      os.Getenv("NKECTL_CWE_IMAGE"),
		ProfilesImage: os.Getenv("NKECTL_CWE_PROFILES_IMAGE"),
		// No cluster-name variable of its own: the bootstrap cluster has one
		// name, NKECTL_BOOTSTRAP_CLUSTER_NAME, and SPIRE is told that one.
		// Callers set SPIFFEClusterName; an empty trust domain is derived from
		// it below.
		SPIFFETrustDomain: os.Getenv("NKECTL_SPIFFE_TRUST_DOMAIN"),
		ChartPath:         os.Getenv("NKECTL_CWE_CHART_PATH"),

		ProvisionedProviderImage: os.Getenv("NKECTL_CAPI_PROVISIONED_IMAGE"),
		ProvisionedProviderPath:  os.Getenv("NKECTL_CAPI_PROVISIONED_PATH"),

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

// Deploy installs Postgres, Temporal, SPIRE, Cluster API, and CWE (in that
// order -- each depends on the one before it) onto the cluster kubeconfigPath
// points at, waiting for each Helm release to report ready before moving to
// the next. It does not wait for CWE's own /readyz; call WaitReady for that
// once Deploy returns, so callers can log "helm release installed" and
// "CWE reports healthy" as distinct, separately-timed steps.
func (e *Engine) Deploy(ctx context.Context, kubeconfigPath string, cfg Config) error {
	if kubeconfigPath == "" {
		return errors.New("cwedeploy: kubeconfigPath is required")
	}
	if cfg.CWEImage == "" {
		return errors.New("cwedeploy: Config.CWEImage is required " +
			"(e.g. nvcr.io/<org>/controller:edge -- see .github/workflows/image-nvcr.yml)")
	}
	if cfg.ProfilesImage == "" {
		return errors.New("cwedeploy: Config.ProfilesImage is required; CWE fails " +
			"its schema migration without an OS profiles tree (see Config.ProfilesImage)")
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
	spireClusterName := cfg.SPIFFEClusterName
	if spireClusterName == "" {
		spireClusterName = "nkectl-bootstrap"
	}
	spireTrustDomain := cfg.SPIFFETrustDomain
	if spireTrustDomain == "" {
		spireTrustDomain = spireClusterName + ".nke.local"
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

	e.log.Info("installing SPIRE CRDs", "namespace", spireNamespace, "version", spireCRDsChartVersion)
	if _, err := e.helm(ctx, kubeconfigPath,
		"upgrade", "--install", spireCRDsReleaseName, spireCRDsChartName,
		"--repo", spireChartRepo,
		"--namespace", spireNamespace, "--create-namespace",
		"--version", spireCRDsChartVersion,
		"--wait", "--timeout", helmTimeout.String(),
	); err != nil {
		return fmt.Errorf("cwedeploy: installing SPIRE CRDs: %w", err)
	}

	spireValues, cleanup, err := writeEmbeddedFile(spireValuesFile)
	if err != nil {
		return err
	}
	defer cleanup()

	// CWE uses SPIFFE workload identity even when the inventory contains no
	// external TPM nodes, so the SPIRE deployment is always required.
	e.log.Info("installing SPIRE", "namespace", spireNamespace, "version", spireChartVersion,
		"clusterName", spireClusterName, "trustDomain", spireTrustDomain,
		"tpmThumbprints", len(cfg.TPMEKFingerprints))
	if _, err := e.helm(ctx, kubeconfigPath,
		spireInstallArgs(cfg, spireValues, spireClusterName, spireTrustDomain, helmTimeout)...,
	); err != nil {
		return fmt.Errorf("cwedeploy: installing SPIRE: %w", err)
	}

	// Only external TPM nodes need the NodePort. CWE and the in-cluster agents
	// use SPIRE's ClusterIP service.
	if len(cfg.TPMEKFingerprints) > 0 {
		if err := e.publishSpireExternally(ctx, kubeconfigPath); err != nil {
			return err
		}
	}

	// Before CWE, not after: CWE starts writing CAPI objects as soon as a
	// cluster is created, and an object written before its controller exists
	// simply sits there.
	if err := e.installCAPI(ctx, kubeconfigPath, cfg, helmTimeout); err != nil {
		return err
	}

	cweValues, cleanup, err := writeEmbeddedFile(cweValuesFile)
	if err != nil {
		return err
	}
	defer cleanup()

	// tag and digest are always set, empty when unused: the chart reads an
	// empty tag as "use appVersion" and an empty digest as "pull by tag".
	imageRepo, imageTag, imageDigest := splitImageRef(cfg.CWEImage)
	profRepo, profTag, profDigest := splitImageRef(cfg.ProfilesImage)
	e.log.Info("installing cwe", "namespace", cweNamespace, "image", cfg.CWEImage,
		"profilesImage", cfg.ProfilesImage, "chart", chartPath)
	if _, err := e.helm(ctx, kubeconfigPath,
		"upgrade", "--install", cweReleaseName, chartPath,
		"--namespace", cweNamespace, "--create-namespace",
		"--values", cweValues,
		"--set-string", "appDb.postgresPassword="+password,
		"--set-string", "image.repository="+imageRepo,
		"--set-string", "image.tag="+imageTag,
		"--set-string", "image.digest="+imageDigest,
		"--set-string", "image.pullPolicy=IfNotPresent",
		"--set", "profiles.image.enabled=true",
		"--set-string", "profiles.path="+profilesPath,
		"--set-string", "profiles.image.repository="+profRepo,
		"--set-string", "profiles.image.tag="+profTag,
		"--set-string", "profiles.image.digest="+profDigest,
		"--set", "spiffe.enabled=true",
		"--set-string", "spiffe.trustDomain="+spireTrustDomain,
		"--set-string", "spiffe.spire.className="+spireControllerManagerClass,
		"--set-string", "spiffe.spire.serverAddress="+spireServiceHost(),
		"--set-string", "spiffe.spire.serverPort="+spireServerPort,
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
	return writeTempFile("nkectl-cwedeploy-*.yaml", body)
}

// writeTempFile writes body to a temporary file matching pattern and returns
// the path plus a cleanup func the caller should defer immediately. Both the
// embedded values files and the rendered provider manifest need a real path on
// disk, because helm's --values and kubectl's -f both take one.
func writeTempFile(pattern string, body []byte) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", func() {}, fmt.Errorf("cwedeploy: creating %s: %w", pattern, err)
	}
	tmpName := f.Name()
	cleanup = func() { _ = os.Remove(tmpName) }
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("cwedeploy: writing %s: %w", tmpName, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("cwedeploy: closing %s: %w", tmpName, err)
	}
	return tmpName, cleanup, nil
}

// resolveChartPath locates controller/deploy/charts/controller by
// walking up from the current working directory looking for go.work (the
// repository root marker). This only works inside a checkout of this
// monorepo -- see the ChartPath doc comment for why that is a known gap, not
// an oversight.
func resolveChartPath() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", fmt.Errorf("cwedeploy: Config.ChartPath is empty and %w; "+
			"set NKECTL_CWE_CHART_PATH explicitly", err)
	}
	chart := filepath.Join(root, "controller", "deploy", "charts", "controller")
	if _, err := os.Stat(chart); err != nil {
		return "", fmt.Errorf("cwedeploy: found repository root at %s but no chart at %s", root, chart)
	}
	return chart, nil
}

// repoRoot walks up from the working directory looking for go.work, this
// repository's root marker. Shared by everything that has to find a path inside
// the checkout; see resolveChartPath for why that is a known gap for a
// standalone binary rather than an oversight.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolving the working directory: %w", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no repository root (go.work) was found above the working directory")
		}
		dir = parent
	}
}

// splitImageRef splits "repo:tag", "repo@sha256:..." or "repo:tag@sha256:..."
// into the parts the chart's image.repository/tag/digest values expect. The
// digest comes off first, or the colon inside "sha256:..." reads as a tag
// separator. A registry port ("localhost:5000/controller") is not a tag.
func splitImageRef(ref string) (repo, tag, digest string) {
	if idx := strings.LastIndex(ref, "@"); idx >= 0 {
		digest = ref[idx+1:]
		ref = ref[:idx]
	}
	idx := strings.LastIndex(ref, ":")
	if idx < 0 || strings.Contains(ref[idx:], "/") {
		if digest != "" {
			return ref, "", digest
		}
		return ref, "latest", ""
	}
	return ref[:idx], ref[idx+1:], digest
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

// spireServiceHost is the in-cluster address CWE dials the SPIRE server on.
// The Service is named after the release.
func spireServiceHost() string {
	return spireReleaseName + "-server." + spireNamespace + ".svc.cluster.local"
}

// spireInstallArgs builds the `helm upgrade --install` for the SPIRE chart.
//
// Everything static about the deployment is in spire-values.yaml. Only what
// nkectl cannot know ahead of time is computed here — the trust domain and
// cluster name, CWE's admin identity, the CA to chain to, and the thumbprints
// allowed to attest.
func spireInstallArgs(cfg Config, valuesPath, clusterName, trustDomain string, timeout time.Duration) []string {
	prefix := spireServerValuesPrefix
	args := []string{
		"upgrade", "--install", spireReleaseName, spireChartName,
		"--repo", spireChartRepo,
		"--namespace", spireNamespace, "--create-namespace",
		"--version", spireChartVersion,
		"--values", valuesPath,
		"--set-string", "global.spire.clusterName=" + clusterName,
		"--set-string", "global.spire.trustDomain=" + trustDomain,
		"--set-string", prefix + ".adminIDs[0]=spiffe://" + trustDomain + "/nke-service/cloud-workflow-engine",
		// Both halves of the two agreements CWE depends on. See the constants.
		"--set-string", prefix + ".controllerManager.className=" + spireControllerManagerClass,
		// --set, not --set-string: the chart's schema types these as integers.
		"--set", prefix + ".service.port=" + spireServerPort,
		// The agent's port is a separate value that does not follow the
		// Service. Both or neither; see the constant.
		"--set", spireAgentValuesPrefix + ".server.port=" + spireServerPort,
	}
	args = append(args, upstreamCAArgs(cfg, prefix)...)
	args = append(args, tpmAttestorArgs(cfg, prefix)...)
	return append(args, "--wait", "--timeout", timeout.String())
}

// tpmAttestorArgs turns the EK allowlist into chart values.
//
// No fingerprints means no TPM attestor: a bootstrap that has not been told
// which hardware it is entitled to admit must not admit any, and the hosts on
// that path run without a SPIRE identity at all.
//
// Reaching the server from outside is a separate concern, handled by
// spire-server-nodeport.yaml rather than by the chart's own Service.
func tpmAttestorArgs(cfg Config, prefix string) []string {
	if len(cfg.TPMEKFingerprints) == 0 {
		return nil
	}
	args := []string{"--set", prefix + ".nodeAttestor.tpmDirect.enabled=true"}
	for i, fingerprint := range cfg.TPMEKFingerprints {
		args = append(args, "--set-string",
			fmt.Sprintf("%s.nodeAttestor.tpmDirect.hashes[%d]=%s", prefix, i, fingerprint))
	}
	return args
}

// upstreamCAArgs points SPIRE at a CA the operator supplied.
//
// The PEM goes in with --set-file rather than --set: it is multi-line, and
// --set would mangle it. The paths are files rather than inline values so the
// key never has to be passed around as a string.
func upstreamCAArgs(cfg Config, prefix string) []string {
	if cfg.SPIFFEUpstreamCACertPath == "" || cfg.SPIFFEUpstreamCAKeyPath == "" {
		return nil
	}
	return []string{
		"--set", prefix + ".upstreamAuthority.disk.enabled=true",
		"--set-file", prefix + ".upstreamAuthority.disk.secret.data.certificate=" + cfg.SPIFFEUpstreamCACertPath,
		"--set-file", prefix + ".upstreamAuthority.disk.secret.data.key=" + cfg.SPIFFEUpstreamCAKeyPath,
		// A self-signed root is its own bundle. Without this the chart mounts
		// no bundle.crt and SPIRE has nothing to publish as the trust anchor
		// the agents already hold.
		"--set-file", prefix + ".upstreamAuthority.disk.secret.data.bundle=" + cfg.SPIFFEUpstreamCACertPath,
	}
}

// publishSpireExternally applies spire-server-nodeport.yaml.
//
// The chart's own Service stays exactly as it installed it. This adds a second
// one beside it, selecting the same pods on a port decided in advance, because
// the nodes that attest were configured with an address and a port when they
// were provisioned — long before this cluster existed. Applying a manifest
// keeps that a declaration rather than surgery on somebody else's release.
func (e *Engine) publishSpireExternally(ctx context.Context, kubeconfigPath string) error {
	manifest, cleanup, err := writeEmbeddedFile(spireNodePortFile)
	if err != nil {
		return err
	}
	defer cleanup()

	e.log.Info("publishing SPIRE for out-of-cluster hosts", "namespace", spireNamespace)
	if out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"-n", spireNamespace, "apply", "-f", manifest); err != nil {
		return fmt.Errorf("cwedeploy: publishing SPIRE with %s: %w\n%s", spireNodePortFile, err, out)
	}
	return nil
}

// DumpDiagnostics writes everything the cluster can say about why a bootstrap
// failed into dir.
//
// It runs before the cluster is torn down, which is the only moment it can:
// `bootstrap site` removes the cluster on every exit path, so by the time a
// caller sees the error the evidence is gone. That is fine when the failure
// explains itself and useless when it does not — "CWE never reported healthy"
// can be a crash-looping pod, an unschedulable one, an image that never pulled
// or a webhook refusing admission, and the timeout alone tells those apart not
// at all.
//
// `kubectl cluster-info dump` is the bulk of it: every namespace's objects and
// pod logs, one file each. The rest covers what it omits — the events and
// object state that explain a pod which never started, and which helm releases
// actually landed.
//
// Every step is best-effort. This runs on a path where something has already
// gone wrong, and failing to collect diagnostics must not replace the original
// error with one about diagnostics.
func (e *Engine) DumpDiagnostics(ctx context.Context, kubeconfigPath, dir string) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		e.log.Warn("could not create the diagnostics directory", "dir", dir, "err", err)
		return
	}

	if _, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"cluster-info", "dump", "--all-namespaces", "--output-directory", dir); err != nil {
		e.log.Warn("cluster-info dump did not complete", "err", err)
	}

	for name, args := range map[string][]string{
		"all-resources.txt":  {"get", "all,events", "-A", "-o", "wide"},
		"pods-described.txt": {"describe", "pods", "-A"},
	} {
		out, err := e.Runner.Run(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfigPath}, args...)...)
		if err != nil {
			out += "\n" + err.Error()
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(out), 0o600); err != nil {
			e.log.Warn("could not write a diagnostics file", "file", name, "err", err)
		}
	}

	out, err := e.Runner.Run(ctx, "helm", "--kubeconfig", kubeconfigPath, "list", "-A")
	if err != nil {
		out += "\n" + err.Error()
	}
	if err := os.WriteFile(filepath.Join(dir, "helm-releases.txt"), []byte(out), 0o600); err != nil {
		e.log.Warn("could not write a diagnostics file", "file", "helm-releases.txt", "err", err)
	}
}
