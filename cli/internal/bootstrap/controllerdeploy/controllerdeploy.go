// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controllerdeploy stands up Postgres, Temporal, SPIRE, and the CWE worker onto a
// Kubernetes cluster (the ephemeral bootstrap Kind cluster
// cli/internal/bootstrap/engine creates), then waits for CWE to report
// healthy on /readyz.
//
// It installs the controller and Postgres charts embedded in nkectl
// (controller/deploy/charts), and the Temporal and SPIRE charts the
// bootstrap manifest pins. See manifests/*.yaml for the values each release
// gets.
//
// Every helm/kubectl invocation goes through the CommandRunner seam so unit
// tests can substitute a fake, the same pattern
// cli/internal/bootstrap/engine uses for kind/kubectl.
package controllerdeploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/manifest"
	"github.com/dsx-ai-factory/nke/controller/deploy/charts"
)

const (
	// Namespaces each release is installed into. Fixed, not configurable:
	// the embedded values files hard-code the in-cluster DNS names that
	// depend on these (e.g. postgres-postgresql.postgres.svc.cluster.local),
	// so the namespace and the values file have to change together.
	postgresNamespace = "postgres"
	temporalNamespace = "temporal"
	spireNamespace    = "spire-server"
	// cweNamespace is the controller's namespace, and cweReleaseName its
	// release, fullname, ServiceAccount and pod name label: all the same name,
	// the one greenfield sites use. spire-values.yaml, controller-values.yaml and
	// controller-nodeport.yaml carry it too and must change with it.
	cweNamespace = "nke-controller"

	// tenantSecretsNamespace is where CWE publishes each cluster's kubeconfig
	// and where it puts the cluster's CAPI objects. It matches
	// controller/internal/constants.NamespaceNKESystem, which the chart creates
	// and scopes CWE's Secret grant to, so it is a constant on both sides.
	tenantSecretsNamespace = "nke-system"

	postgresReleaseName  = "postgres"
	temporalReleaseName  = "temporal"
	spireCRDsReleaseName = "spire-crds"
	spireReleaseName     = "spire"
	cweReleaseName       = cweNamespace

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

	// The tpm-direct plugin binary is architecture-specific. Keep both
	// checksums explicit so that chart defaults cannot select the wrong binary.
	spireTPMDirectAMD64Checksum = "46d0caad8c25a027dd11c93e18b58a8bc6fbd9f1fe2e36fa2a0dd440986de4dc"
	spireTPMDirectARM64Checksum = "b69d0153c8241383f24da138bc85429a620d9f962a2f10abb314aa0e39046d16"

	// DefaultCWEPort is the port the CWE API uses at every layer: the Helm
	// Service, the external NodePort Service, and both sides of Kind's host port
	// mapping.
	//
	// nkectl has to reach the API it just deployed, from the host, to create the
	// site cluster. It cannot use the ClusterIP, and the chart's Service
	// template has no nodePort field -- so this is a second Service beside the
	// chart's own, the same shape and for the same reason as
	// spire-server-nodeport.yaml. bootstrapengine maps it to a host port (see
	// cmd/nkectl/bootstrap.go), which keeps the route the same whether kind runs
	// on Docker or nerdctl, on Linux or macOS.
	//
	DefaultCWEPort int32 = 30080

	// SPIREServerNodePort is the fixed NodePort used by the bootstrap SPIRE server.
	SPIREServerNodePort int32 = 30081

	// readyzPath is polled through the API server's service proxy rather
	// than a port-forward, so no local port has to be reserved or cleaned
	// up. controller/internal/server/health.go serves it from the CWE pod
	// itself: DB Ping + Temporal CheckHealth both have to succeed.
	readyzPath = "/readyz"
	cweSvcPort = "http"

	// profilesPath is where CWE reads OS and config profiles from
	// (PROFILES_DIR). Set explicitly rather than left to the chart's empty
	// default, so PROFILES_DIR names the empty /profiles the controller image
	// ships rather than resolving somewhere else. nkectl deploys no profiles
	// into it; a site gets them from its deploy repository.
	profilesPath = "/profiles"
)

// waitReadyPollInterval is the delay between /readyz polls. A var, not a
// const, so unit tests can shrink it instead of paying real wall-clock time
// for a retry loop.
var waitReadyPollInterval = 3 * time.Second

// tenantKubeconfigPollInterval is the delay between attempts to read the
// asynchronously published tenant kubeconfig Secret.
var tenantKubeconfigPollInterval = 2 * time.Second

const defaultTenantKubeconfigTimeout = 2 * time.Minute

// Config describes a CWE deployment.
type Config struct {
	// CWEImage is the image reference to deploy. Defaults to nke-controller:latest,
	// the local build's tag, from the bootstrap manifest. NKECTL_CONTROLLER_IMAGE
	// overrides it, for example with the image .github/workflows/image-nvcr.yml
	// publishes.
	CWEImage string

	// SPIFFEClusterName identifies the ephemeral Kind cluster to SPIRE. It is
	// the Kind cluster's own name, which is the only name this cluster has.
	// Empty defaults to "nkectl-bootstrap", matching bootstrapengine's default.
	SPIFFEClusterName string

	// SPIFFETrustDomain is the trust domain used for CWE's workload identity.
	// Empty defaults to <SPIFFEClusterName>.nke.local.
	SPIFFETrustDomain string

	// SPIFFEUpstreamCACertPath and SPIFFEUpstreamCAKeyPath are a CA the
	// operator brings to the bootstrap, from the site spec's spire section.
	// SPIRE chains its own CA to it, so a host that was given the certificate
	// ahead of time can verify the server the first time it speaks to it,
	// instead of trusting it blindly.
	//
	// Not read from the environment: this describes the site, so the site spec
	// is the one place it comes from.
	SPIFFEUpstreamCACertPath string
	SPIFFEUpstreamCAKeyPath  string

	// TPMEKFingerprints are the SHA-256 endorsement-key thumbprints of the
	// hosts allowed to attest to this SPIRE server, seeded into the TPM node
	// attestor's allowlist. They come from the site spec, one per node, which
	// is where the operator declares which machines it is entitled to admit.
	// Empty leaves the attestor off, which is what a bootstrap that was told
	// about no hardware identities wants.
	//
	// When set, the server is also published on a NodePort: the hosts that
	// attest are outside the cluster, and a ClusterIP is not a route they have.
	TPMEKFingerprints []string

	// DisableMTLS stands the site up without SPIFFE: no SPIRE is installed, and
	// CWE dials each node agent in plaintext. The agent has to be running with
	// --disable-mtls for that to connect.
	//
	// For bring-up debugging. Everything else about a site is all-or-nothing on
	// attestation, so a break anywhere -- image, attestor, thumbprint, CA,
	// routing -- looks the same, and this is the only way to bisect it.
	DisableMTLS bool

	// ProvisionedProviderImage is the cluster-api-provider-provisioned image to
	// run as the CAPI infrastructure provider for hosts that already exist.
	// Defaults to cluster-api-provider-provisioned:latest, the local build's
	// tag, from the bootstrap manifest. NKECTL_CAPI_PROVISIONED_IMAGE overrides
	// it, for example with the image .github/workflows/image-provisioned-nvcr.yml
	// publishes. Either way the image is loaded into the cluster rather than
	// pulled, because the bootstrap cluster has no pull secret.
	ProvisionedProviderImage string

	// NodeAgentPort is the port CWE dials nke-agent on. Empty leaves CWE's own
	// default (15005) alone. It comes from the site spec, because it describes
	// how the site's hosts were configured long before this cluster existed.
	NodeAgentPort string

	// CWEPort is the port the CWE API uses for its Helm Service, Kubernetes
	// NodePort, Kind host mapping, and host client. Zero uses DefaultCWEPort.
	CWEPort int32

	// PostgresPassword is the Postgres superuser password shared by the
	// Postgres, Temporal, and CWE releases. Generated if empty.
	PostgresPassword string

	// HelmTimeout bounds each `helm upgrade --install --wait` call.
	HelmTimeout time.Duration

	// ReadyTimeout bounds how long WaitReady polls /readyz before giving up.
	ReadyTimeout time.Duration

	// TemporalChartRef is where helm pulls the Temporal chart from, as an OCI
	// reference or a local path. Empty uses the go.temporal.io chart repo.
	TemporalChartRef string

	// SPIREChartRef is where helm pulls the SPIRE chart from, as an OCI
	// reference or a local path. Empty uses the spiffe.github.io hardened
	// chart repo.
	SPIREChartRef string

	// SPIRECRDsChartRef is where helm pulls the SPIRE CRDs chart from, as an
	// OCI reference or a local path. Empty uses the spiffe.github.io hardened
	// chart repo.
	SPIRECRDsChartRef string
}

// DefaultConfig reads the package's knobs from the environment, mirroring
// bootstrapengine.DefaultConfig's env-driven shape.
func DefaultConfig() Config {
	return Config{
		CWEImage: envOr("NKECTL_CONTROLLER_IMAGE", defaultImage(manifest.ComponentController, manifest.ImageController)),
		// No cluster-name variable of its own: the bootstrap cluster has one
		// name, NKECTL_BOOTSTRAP_CLUSTER_NAME, and SPIRE is told that one.
		// Callers set SPIFFEClusterName; an empty trust domain is derived from
		// it below.
		SPIFFETrustDomain: os.Getenv("NKECTL_SPIFFE_TRUST_DOMAIN"),

		ProvisionedProviderImage: envOr("NKECTL_CAPI_PROVISIONED_IMAGE", defaultImage(manifest.ComponentProvisioned, manifest.ImageManager)),
		CWEPort:                  envPort("NKECTL_CONTROLLER_PORT", DefaultCWEPort),

		HelmTimeout:  envDuration("NKECTL_CONTROLLER_HELM_TIMEOUT", 5*time.Minute),
		ReadyTimeout: envDuration("NKECTL_CONTROLLER_READY_TIMEOUT", 5*time.Minute),

		TemporalChartRef:  getenv("NKECTL_CONTROLLER_TEMPORAL_CHART"),
		SPIREChartRef:     getenv("NKECTL_CONTROLLER_SPIRE_CHART"),
		SPIRECRDsChartRef: getenv("NKECTL_CONTROLLER_SPIRE_CRDS_CHART"),
	}
}

// renamedSettings maps each setting named for the controller to the name it
// had while the controller was called CWE. The old name is still read when the
// new one is unset, for one release, and DeprecatedSettings reports it.
var renamedSettings = map[string]string{
	"NKECTL_CONTROLLER_IMAGE":            "NKECTL_CWE_IMAGE",
	"NKECTL_CONTROLLER_PORT":             "NKECTL_CWE_PORT",
	"NKECTL_CONTROLLER_HELM_TIMEOUT":     "NKECTL_CWE_HELM_TIMEOUT",
	"NKECTL_CONTROLLER_READY_TIMEOUT":    "NKECTL_CWE_READY_TIMEOUT",
	"NKECTL_CONTROLLER_TEMPORAL_CHART":   "NKECTL_CWE_TEMPORAL_CHART",
	"NKECTL_CONTROLLER_SPIRE_CHART":      "NKECTL_CWE_SPIRE_CHART",
	"NKECTL_CONTROLLER_SPIRE_CRDS_CHART": "NKECTL_CWE_SPIRE_CRDS_CHART",
}

// RenamedSetting is an old setting name that is set while its replacement is
// not.
type RenamedSetting struct {
	Old, New string
}

// DeprecatedSettings returns, sorted by old name, every old NKECTL_CWE_*
// setting DefaultConfig reads because its NKECTL_CONTROLLER_* replacement is
// unset. The caller warns about each; this package has no logger of its own at
// the point DefaultConfig runs.
func DeprecatedSettings() []RenamedSetting {
	var found []RenamedSetting
	for name, old := range renamedSettings {
		if os.Getenv(name) == "" && os.Getenv(old) != "" {
			found = append(found, RenamedSetting{Old: old, New: name})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Old < found[j].Old })
	return found
}

// getenv returns the setting's value, or, when it is unset or empty, the value
// of the name it replaced.
func getenv(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	if old, ok := renamedSettings[name]; ok {
		return os.Getenv(old)
	}
	return ""
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
			"(e.g. nvcr.io/<org>/nke-controller:edge -- see .github/workflows/image-nvcr.yml)")
	}
	if cfg.ProvisionedProviderImage == "" {
		return errors.New("cwedeploy: Config.ProvisionedProviderImage is required; the CAPI " +
			"objects CWE writes for a site cluster have no controller without it " +
			"(see Config.ProvisionedProviderImage)")
	}

	// The charts this binary was built with. helm takes a directory and this
	// process runs the helm binary rather than linking its loader, so they are
	// written out for the length of the deploy.
	chartPath, err := os.MkdirTemp("", "nkectl-controller-chart-*")
	if err != nil {
		return fmt.Errorf("cwedeploy: creating a directory for the controller chart: %w", err)
	}
	defer func() { _ = os.RemoveAll(chartPath) }()
	if err := charts.Extract(charts.NKEController, chartPath); err != nil {
		return fmt.Errorf("cwedeploy: writing out the controller chart: %w", err)
	}
	postgresChartPath, err := os.MkdirTemp("", "nkectl-postgres-chart-*")
	if err != nil {
		return fmt.Errorf("cwedeploy: creating a directory for the postgres chart: %w", err)
	}
	defer func() { _ = os.RemoveAll(postgresChartPath) }()
	if err := charts.Extract(charts.Postgresql, postgresChartPath); err != nil {
		return fmt.Errorf("cwedeploy: writing out the postgres chart: %w", err)
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
	cwePort := effectiveCWEPort(cfg)

	// No version flag: the chart's version is the binary's. The chart carries
	// its own defaults -- the Temporal databases, the Service name both dial,
	// and the image pinned by digest -- and the values file renames only the
	// controller's database. The password is supplied per run.
	postgresValues, postgresCleanup, err := writeEmbeddedFile(postgresValuesFile)
	if err != nil {
		return err
	}
	defer postgresCleanup()

	e.log.Info("installing postgres", "namespace", postgresNamespace, "chart", postgresChartPath)
	if _, err := e.helm(ctx, kubeconfigPath,
		"upgrade", "--install", postgresReleaseName, postgresChartPath,
		"--namespace", postgresNamespace, "--create-namespace",
		"--values", postgresValues,
		"--set-string", "auth.password="+password,
		"--wait", "--timeout", helmTimeout.String(),
	); err != nil {
		return fmt.Errorf("cwedeploy: installing postgres: %w", err)
	}

	temporalValues, cleanup, err := writeEmbeddedFile(temporalValuesFile)
	if err != nil {
		return err
	}
	defer cleanup()

	// Chart identities come from the embedded bootstrap manifest, so what the
	// bootstrap installs is pinned in one place. Each chart is passed via
	// `helm --repo` rather than `helm repo add`, so this package does not
	// mutate the caller's global Helm repo list; Config.*ChartRef replaces
	// each repo lookup. The CRDs are their own release because a chart cannot
	// depend on its own CRDs being applied first.
	temporalChart, err := manifestChart(manifest.ComponentTemporal)
	if err != nil {
		return err
	}
	spireCRDsChart, err := manifestChart(manifest.ComponentSPIRECRDs)
	if err != nil {
		return err
	}
	spireChart, err := manifestChart(manifest.ComponentSPIRE)
	if err != nil {
		return err
	}

	temporalChartArgs := chartArgs(cfg.TemporalChartRef, temporalChart.Name, temporalChart.Repo)
	e.log.Info("installing temporal", "namespace", temporalNamespace, "version", temporalChart.Version, "chart", temporalChartArgs[0])
	temporalArgs := append([]string{"upgrade", "--install", temporalReleaseName}, temporalChartArgs...)
	temporalArgs = append(temporalArgs,
		"--namespace", temporalNamespace, "--create-namespace",
		"--values", temporalValues,
		"--set-string", "server.config.persistence.default.sql.password="+password,
		"--set-string", "server.config.persistence.visibility.sql.password="+password,
		"--version", temporalChart.Version,
		"--wait", "--timeout", helmTimeout.String(),
	)
	if _, err := e.helm(ctx, kubeconfigPath, temporalArgs...); err != nil {
		return fmt.Errorf("cwedeploy: installing temporal: %w", err)
	}

	// --disable-mtls stands the site up without SPIFFE, so nothing needs SPIRE
	// and CWE dials each agent in plaintext. Bring-up debugging only.
	if cfg.DisableMTLS {
		e.log.Warn("SPIFFE is disabled: skipping SPIRE, and CWE will dial node agents in plaintext")
	} else {
		spireCRDsChartArgs := chartArgs(cfg.SPIRECRDsChartRef, spireCRDsChart.Name, spireCRDsChart.Repo)
		e.log.Info("installing SPIRE CRDs", "namespace", spireNamespace, "version", spireCRDsChart.Version, "chart", spireCRDsChartArgs[0])
		spireCRDsArgs := append([]string{"upgrade", "--install", spireCRDsReleaseName}, spireCRDsChartArgs...)
		spireCRDsArgs = append(spireCRDsArgs,
			"--namespace", spireNamespace, "--create-namespace",
			"--version", spireCRDsChart.Version,
			"--wait", "--timeout", helmTimeout.String(),
		)
		if _, err := e.helm(ctx, kubeconfigPath, spireCRDsArgs...); err != nil {
			return fmt.Errorf("cwedeploy: installing SPIRE CRDs: %w", err)
		}

		spireValues, cleanup, err := writeEmbeddedFile(spireValuesFile)
		if err != nil {
			return err
		}
		defer cleanup()

		// CWE uses SPIFFE workload identity even when the site spec contains no
		// external TPM nodes, so the SPIRE deployment is always required.
		e.log.Info("installing SPIRE", "namespace", spireNamespace, "version", spireChart.Version,
			"clusterName", spireClusterName, "trustDomain", spireTrustDomain,
			"tpmThumbprints", len(cfg.TPMEKFingerprints))
		if _, err := e.helm(ctx, kubeconfigPath,
			spireInstallArgs(cfg, spireChart, spireValues, spireClusterName, spireTrustDomain, helmTimeout)...,
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
	e.log.Info("installing cwe", "namespace", cweNamespace, "image", cfg.CWEImage,
		"chart", chartPath)
	cweArgs := []string{
		"upgrade", "--install", cweReleaseName, chartPath,
		"--namespace", cweNamespace, "--create-namespace",
		"--values", cweValues,
		"--set-string", "appDb.postgresPassword=" + password,
		"--set-string", "image.repository=" + imageRepo,
		"--set-string", "image.tag=" + imageTag,
		"--set-string", "image.digest=" + imageDigest,
		"--set-string", "image.pullPolicy=IfNotPresent",
		"--set-string", "profiles.path=" + profilesPath,
		"--set", "cni.installAfterInit=true",
		"--set", fmt.Sprintf("spiffe.enabled=%t", !cfg.DisableMTLS),
		"--set-string", "spiffe.trustDomain=" + spireTrustDomain,
		"--set-string", "spiffe.spire.className=" + spireControllerManagerClass,
		"--set-string", "spiffe.spire.serverAddress=" + spireServiceHost(),
		"--set-string", "spiffe.spire.serverPort=" + spireServerPort,
		"--set-string", "supportedInfraProviders=infra",
		"--set", fmt.Sprintf("service.port=%d", cwePort),
	}
	cweArgs = append(cweArgs, cweEnvArgs(cfg)...)
	cweArgs = append(cweArgs, "--wait", "--timeout", helmTimeout.String())
	if _, err := e.helm(ctx, kubeconfigPath, cweArgs...); err != nil {
		return fmt.Errorf("cwedeploy: installing cwe: %w", err)
	}

	// The API has to be reachable from the host that ran this command: it is
	// what creates the cluster next. See Config.CWEPort.
	return e.publishCWEExternally(ctx, kubeconfigPath, cwePort)
}

// cweEnvArgs renders the settings CWE reads from its own environment as helm
// arguments over the chart's generic `env` list. A setting left at its zero
// value is omitted, leaving CWE's own default in place.
func cweEnvArgs(cfg Config) []string {
	vars := [][2]string{
		// Unconditional: sitecluster.ClusterRequest builds only the cluster-level
		// control plane, which CWE rejects without this.
		{"PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER", "true"},
	}
	if cfg.NodeAgentPort != "" {
		vars = append(vars, [2]string{"NODE_AGENT_PORT", cfg.NodeAgentPort})
	}

	// `env` is a list, so the settings share one numbering. Numbering each on its
	// own would put every one of them at env[0], keeping only the last.
	args := make([]string, 0, len(vars)*4)
	for i, v := range vars {
		args = append(args,
			"--set-string", fmt.Sprintf("env[%d].name=%s", i, v[0]),
			"--set-string", fmt.Sprintf("env[%d].value=%s", i, v[1]))
	}
	return args
}

// publishCWEExternally applies the NodePort Service the CLI reaches the CWE API
// through. Same second-Service shape as publishSpireExternally, and applied for
// the same reason: the chart owns its own Service and this does not touch it.
func (e *Engine) publishCWEExternally(
	ctx context.Context,
	kubeconfigPath string,
	port int32,
) error {
	manifest, cleanup, err := writeEmbeddedTemplate(cweNodePortFile, struct {
		Port int32
	}{port})
	if err != nil {
		return err
	}
	defer cleanup()

	e.log.Info("publishing the cwe api for the host", "namespace", cweNamespace, "port", port)
	if out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"-n", cweNamespace, "apply", "-f", manifest); err != nil {
		return fmt.Errorf("cwedeploy: publishing the cwe api with %s: %w\n%s", cweNodePortFile, err, out)
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

// writeEmbeddedTemplate renders an embedded YAML template to a temporary file.
func writeEmbeddedTemplate(name string, data any) (path string, cleanup func(), err error) {
	body, err := manifestsFS.ReadFile(name)
	if err != nil {
		return "", func() {}, fmt.Errorf("cwedeploy: reading embedded %s: %w", name, err)
	}
	tmpl, err := template.New(name).Parse(string(body))
	if err != nil {
		return "", func() {}, fmt.Errorf("cwedeploy: parsing embedded %s: %w", name, err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return "", func() {}, fmt.Errorf("cwedeploy: rendering embedded %s: %w", name, err)
	}
	return writeTempFile("nkectl-cwedeploy-*.yaml", rendered.Bytes())
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
	v := getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func envPort(key string, fallback int32) int32 {
	value := getenv(key)
	if value == "" {
		return fallback
	}
	port, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return fallback
	}
	return int32(port)
}

func effectiveCWEPort(cfg Config) int32 {
	if cfg.CWEPort == 0 {
		return DefaultCWEPort
	}
	return cfg.CWEPort
}

// spireServiceHost is the in-cluster address CWE dials the SPIRE server on.
// The Service is named after the release.
func spireServiceHost() string {
	return spireReleaseName + "-server." + spireNamespace + ".svc.cluster.local"
}

// envOr returns the setting's value, or fallback when it is unset or empty.
func envOr(name, fallback string) string {
	if v := getenv(name); v != "" {
		return v
	}
	return fallback
}

// defaultImage returns the image reference the bootstrap manifest carries for
// one of the images this repository builds. Empty when the manifest has none.
func defaultImage(component, image string) string {
	comp, ok := manifest.Load().Components[component]
	if !ok {
		return ""
	}
	return comp.Images[image].Ref
}

// manifestChart returns the identity of a chart the bootstrap installs, as
// the embedded bootstrap manifest pins it. The manifest ships in the binary,
// so a missing entry is a build defect; TestDeploy_ChartsMatchTheBootstrapManifest
// guards the pairing.
func manifestChart(component string) (manifest.Chart, error) {
	comp, ok := manifest.Load().Components[component]
	if !ok || comp.Chart == nil {
		return manifest.Chart{}, fmt.Errorf("cwedeploy: the embedded bootstrap manifest pins no chart for %q", component)
	}
	return *comp.Chart, nil
}

// chartArgs returns the chart half of a `helm upgrade --install`: the override
// verbatim when set, otherwise the upstream chart name and its --repo. The
// first element is always the chart. An override carries no --repo because
// helm resolves whatever it is given as a name in that repo when one is
// present, so an OCI reference would be looked up in the repo's index rather
// than pulled.
func chartArgs(override, name, repo string) []string {
	if override != "" {
		return []string{override}
	}
	return []string{name, "--repo", repo}
}

// spireInstallArgs builds the `helm upgrade --install` for the SPIRE chart.
//
// Everything static about the deployment is in spire-values.yaml. Only what
// nkectl cannot know ahead of time is computed here — the trust domain and
// cluster name, CWE's admin identity, the CA to chain to, and the thumbprints
// allowed to attest.
func spireInstallArgs(cfg Config, chart manifest.Chart, valuesPath, clusterName, trustDomain string, timeout time.Duration) []string {
	prefix := spireServerValuesPrefix
	args := append([]string{"upgrade", "--install", spireReleaseName},
		chartArgs(cfg.SPIREChartRef, chart.Name, chart.Repo)...)
	args = append(args,
		"--namespace", spireNamespace, "--create-namespace",
		"--version", chart.Version,
		"--values", valuesPath,
		"--set-string", "global.spire.clusterName="+clusterName,
		"--set-string", "global.spire.trustDomain="+trustDomain,
		"--set-string", prefix+".adminIDs[0]=spiffe://"+trustDomain+"/nke-service/"+cweReleaseName,
		// Both halves of the two agreements CWE depends on. See the constants.
		"--set-string", prefix+".controllerManager.className="+spireControllerManagerClass,
		// --set, not --set-string: the chart's schema types these as integers.
		"--set", prefix+".service.port="+spireServerPort,
		// The agent's port is a separate value that does not follow the
		// Service. Both or neither; see the constant.
		"--set", spireAgentValuesPrefix+".server.port="+spireServerPort,
	)
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
	if checksum := tpmPluginChecksum(runtime.GOARCH); checksum != "" {
		args = append(args, "--set-string", prefix+".nodeAttestor.tpmDirect.checksum="+checksum)
	}
	for i, fingerprint := range cfg.TPMEKFingerprints {
		args = append(args, "--set-string",
			fmt.Sprintf("%s.nodeAttestor.tpmDirect.hashes[%d]=%s", prefix, i, fingerprint))
	}
	return args
}

func tpmPluginChecksum(goarch string) string {
	switch goarch {
	case "amd64":
		return spireTPMDirectAMD64Checksum
	case "arm64":
		return spireTPMDirectARM64Checksum
	default:
		return ""
	}
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

// TenantKubeconfig waits up to timeout for the asynchronously published admin
// kubeconfig for clusterName and returns it. A non-positive timeout uses the
// package default.
//
// CWE writes it as a Secret in nke-system, which is also the namespace it puts
// the cluster's CAPI objects in — so this is the same Secret Cluster API
// resolves by name for the cluster, and the one
// cluster-api-provider-provisioned reads to set each Node's provider ID.
//
// It is the plain admin kubeconfig. CWE only rewrites it to a SPIRE exec-auth
// kubeconfig when OIDC authentication is enabled on the cluster, which a
// bootstrap does not turn on (see nkeOIDCAuth in manifests/controller-values.yaml).
func (e *Engine) TenantKubeconfig(
	ctx context.Context,
	kubeconfigPath, clusterName string,
	timeout time.Duration,
) ([]byte, error) {
	if timeout <= 0 {
		timeout = defaultTenantKubeconfigTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	for {
		kubeconfig, err := e.tenantKubeconfigOnce(ctx, kubeconfigPath, clusterName)
		if err == nil {
			return kubeconfig, nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("cwedeploy: tenant kubeconfig for %q was not published within %s: %w",
				clusterName, timeout, lastErr)
		case <-time.After(tenantKubeconfigPollInterval):
		}
	}
}

func (e *Engine) tenantKubeconfigOnce(ctx context.Context, kubeconfigPath, clusterName string) ([]byte, error) {
	secret := clusterName + "-kubeconfig"
	out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"-n", tenantSecretsNamespace, "get", "secret", secret,
		"-o", "jsonpath={.data.value}")
	if err != nil {
		return nil, fmt.Errorf("cwedeploy: reading %s/%s: %w\n%s", tenantSecretsNamespace, secret, err, out)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return nil, fmt.Errorf("cwedeploy: decoding %s/%s: %w", tenantSecretsNamespace, secret, err)
	}
	if len(decoded) == 0 {
		return nil, fmt.Errorf("cwedeploy: %s/%s holds an empty kubeconfig", tenantSecretsNamespace, secret)
	}
	return decoded, nil
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
