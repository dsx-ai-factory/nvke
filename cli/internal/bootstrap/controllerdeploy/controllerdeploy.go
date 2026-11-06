// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controllerdeploy stands up Postgres, Temporal, SPIRE, and the controller worker onto a
// Kubernetes cluster (the ephemeral bootstrap Kind cluster
// cli/internal/bootstrap/engine creates), then waits for the controller to report
// healthy on /readyz.
//
// It installs the controller and Postgres charts embedded in nvkectl
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
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/manifest"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
	"github.com/dsx-ai-factory/nvke/controller/deploy/charts"
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
	cweNamespace = "nvke-controller"

	// tenantSecretsNamespace is where the controller publishes each cluster's kubeconfig
	// and where it puts the cluster's CAPI objects. It is the chart's
	// nvkeSystemNamespace.name default, which the chart creates and scopes the
	// controller's Secret grant to. A bootstrap installs the chart with default
	// values and so never pins another name, which is why this stays a constant.
	tenantSecretsNamespace = "nvke-system"

	// dgxcTenantSecret is the Secret holding the tenant node login, named by
	// the chart's secrets.dgxcTenantCredSecretName. It matches the name the
	// deploy path's 35-dgxc-tenant-credentials template renders.
	dgxcTenantSecret = "dgxc-tenant-credentials"

	postgresReleaseName  = "postgres"
	temporalReleaseName  = "temporal"
	spireCRDsReleaseName = "spire-crds"
	spireReleaseName     = "spire"
	cweReleaseName       = cweNamespace

	// spireServerPort is the port the SPIRE server is reached on inside the
	// cluster: the port its Service listens on, the port its own agents dial,
	// and the port the controller is told to dial.
	//
	// 8081 rather than the chart's own default of 443, so that the controller's
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
	// claims objects under, and the class the NVKE controller stamps on the ClusterStaticEntry
	// objects it writes. The two have to be the same string: the controller
	// manager ignores an entry whose class is not its own, so a mismatch means
	// the NVKE controller writes entries nobody acts on and a node never gets its identity.
	//
	// Neither side's default gets there on its own. The chart derives
	// <namespace>-<release> when className is empty, which here would be
	// "spire-server-spire", while the NVKE controller defaults to "spire-mgmt-spire". So both
	// are set explicitly, from this one constant, rather than left to agree by
	// coincidence. The value is the NVKE controller's default, which keeps it the same string
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

	// DefaultCWEPort is the port the controller API uses at every layer: the Helm
	// Service, the external NodePort Service, and both sides of Kind's host port
	// mapping.
	//
	// nvkectl has to reach the API it just deployed, from the host, to create the
	// site cluster. It cannot use the ClusterIP, and the chart's Service
	// template has no nodePort field -- so this is a second Service beside the
	// chart's own, the same shape and for the same reason as
	// spire-server-nodeport.yaml. bootstrapengine maps it to a host port (see
	// internal/nvkectl/bootstrap.go), which keeps the route the same whether kind runs
	// on Docker or nerdctl, on Linux or macOS.
	//
	DefaultCWEPort int32 = 30080

	// tpmPubhashesConfigMap is the ConfigMap CWE keeps the TPM allowlist in. The
	// name is CWE's default (SPIRE_TPM_PUBHASHES_CONFIG_MAP_NAME) and has to
	// match it.
	tpmPubhashesConfigMap = "tpm-pubhashes"
	tpmPubhashesVolume    = "tpm-pubhashes"

	// tpmHashesMountPath is the directory the chart points the TPM attestor's
	// hash_path at when it is given no static hashes.
	tpmHashesMountPath = "/run/spire/data/tpm-direct/hashes"

	// SPIREServerNodePort is the fixed NodePort used by the bootstrap SPIRE server.
	SPIREServerNodePort int32 = 30081

	// readyzPath is polled through the API server's service proxy rather
	// than a port-forward, so no local port has to be reserved or cleaned
	// up. controller/internal/server/health.go serves it from the controller pod
	// itself: DB Ping + Temporal CheckHealth both have to succeed.
	readyzPath = "/readyz"
	cweSvcPort = "http"

	// profilesPath is where the controller reads OS and config profiles from
	// (profilesDir). Set explicitly rather than left to the chart's empty
	// default, so profilesDir names the empty /profiles the controller image
	// ships rather than resolving somewhere else. nvkectl deploys no profiles
	// into it unless Config.ProfilesImage is set, which copies that image's
	// /profiles tree in; a site otherwise gets them from its deploy repository.
	profilesPath = "/profiles"

	// otlpHeadersEnv is the variable the controller's OpenTelemetry SDK reads
	// the headers of its exports from.
	otlpHeadersEnv = "OTEL_EXPORTER_OTLP_HEADERS"
)

// waitReadyPollInterval is the delay between /readyz polls. A var, not a
// const, so unit tests can shrink it instead of paying real wall-clock time
// for a retry loop.
var waitReadyPollInterval = 3 * time.Second

// tenantKubeconfigPollInterval is the delay between attempts to read the
// asynchronously published tenant kubeconfig Secret.
var tenantKubeconfigPollInterval = 2 * time.Second

// helmFetchAttempts bounds the retries of a chart download that failed with a server error.
const helmFetchAttempts = 3

// helmFetchRetryInterval is the delay between those attempts; a var so tests can shorten it.
var helmFetchRetryInterval = 5 * time.Second

// chartFetchServerError matches helm's report of a chart download that got a 5xx, the one failure a retry fixes.
var chartFetchServerError = regexp.MustCompile(`failed to fetch .*: 5\d\d\b`)

const defaultTenantKubeconfigTimeout = 2 * time.Minute

// oidcValues is the helms values file
type oidcValues struct {
	OIDC oidcProviders `yaml:"oidc"`
}

// oidcProviders is the helm chart's oidc.providers list.
type oidcProviders struct {
	Providers []sitespec.OIDCProvider `yaml:"providers"`
}

// TracingConfig points the controller's own OpenTelemetry export at a collector.
// The zero value leaves it off, which is the chart's default.
type TracingConfig struct {
	// Endpoint is the collector's OTLP/gRPC host:port. Empty turns export off.
	Endpoint string
	// Insecure sends without TLS.
	Insecure bool
	// Headers are sent on every export request, for instance an authorization
	// token. They reach the controller as OTEL_EXPORTER_OTLP_HEADERS, and only
	// when Endpoint is set.
	Headers map[string]string
}

// Config describes a controller deployment.
type Config struct {
	// CWEImage is the image reference to deploy. Defaults to nke-controller:latest,
	// the local build's tag, from the bootstrap manifest. NVKECTL_NVKE_CONTROLLER_IMAGE
	// overrides it, for example with the image .github/workflows/image-nvcr.yml
	// publishes.
	CWEImage string

	// SPIFFEClusterName identifies the ephemeral Kind cluster to SPIRE. It is
	// the Kind cluster's own name, which is the only name this cluster has.
	// Empty defaults to "nvkectl-bootstrap", matching bootstrapengine's default.
	SPIFFEClusterName string

	// SPIFFETrustDomain is the trust domain used for the controller's workload identity.
	// Empty defaults to <SPIFFEClusterName>.nke.local.
	SPIFFETrustDomain string

	// SPIFFEUpstreamCACertPath and SPIFFEUpstreamCAKeyPath are a CA the
	// operator brings to the bootstrap, from NVKECTL_SPIRE_CA_CERT_FILE and
	// NVKECTL_SPIRE_CA_KEY_FILE. SPIRE chains its own CA to it, so a host that was
	// given the certificate ahead of time can verify the server the first time
	// it speaks to it, instead of trusting it blindly.
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

	// ProfilesImage is an OCI content image holding the OS and config profiles
	// CWE reads, mounted at PROFILES_DIR by the chart's init container. Empty
	// deploys CWE with the controller image's empty /profiles, which is enough
	// for hosts that already exist and not for nodes CAPNICo creates: those need
	// the profiles a cluster request names.
	//
	// A reference without a registry is side-loaded from the local image store,
	// like CWEImage. The chart sets no pull policy on it, so it has to carry a
	// tag other than latest, or a digest, or the cluster tries to pull it.
	ProfilesImage string

	// SPIREExternalAddress is the address nodes created by CAPNICo use to reach
	// the SPIRE server. CWE writes it into each node's userdata, so it has to
	// work from outside the cluster; the in-cluster name the provisioned path
	// uses does not. Required with NicoInfrastructure, and always paired with
	// SPIREServerNodePort.
	SPIREExternalAddress string

	// DisableMTLS stands the site up without SPIFFE: no SPIRE is installed, and
	// the controller dials each node agent in plaintext. The agent has to be running with
	// --disable-mtls for that to connect.
	//
	// For bring-up debugging. Everything else about a site is all-or-nothing on
	// attestation, so a break anywhere -- image, attestor, thumbprint, CA,
	// routing -- looks the same, and this is the only way to bisect it.
	DisableMTLS bool

	// ProvisionedProviderImage is the cluster-api-provider-provisioned image to
	// run as the CAPI infrastructure provider for hosts that already exist.
	// Defaults to cluster-api-provider-provisioned:latest, the local build's
	// tag, from the bootstrap manifest. NVKECTL_CAPI_PROVISIONED_IMAGE overrides
	// it, for example with the image .github/workflows/image-provisioned-nvcr.yml
	// publishes. Either way the image is loaded into the cluster rather than
	// pulled, because the bootstrap cluster has no pull secret.
	ProvisionedProviderImage string

	// NicoInfrastructure installs CAPNICo as the infrastructure provider, in
	// place of cluster-api-provider-provisioned: the site's machines are
	// allocated from a NICo site instead of taken from a hardcoded host list.
	// This is for the bootstrap Kind cluster only; the finished controller's
	// tenant-cluster NICo is configured separately and may be a different one.
	NicoInfrastructure bool

	// CAPNICo is what the bootstrap CAPNICo is given to reach NICo. Required
	// with NicoInfrastructure. Bootstrap NICo only; not the tenant-cluster NICo.
	CAPNICo CAPNICoAccess

	// DGXCTenant is the login the controller renders into the cloud-init of
	// every node CAPNICo creates. Required with NicoInfrastructure: the
	// controller refuses to render user data without it.
	DGXCTenant TenantLogin

	// ReleasesPath is a file or directory of MachineImage and ClusterRelease
	// manifests applied to the cluster once the controller's CRDs exist. The
	// controller resolves the control plane's release from them when it
	// creates the first node. Required with NicoInfrastructure.
	ReleasesPath string

	// CAPNICoImage, when set, replaces the image the embedded CAPNICo manifest
	// names, as repository@sha256:digest. It is the capnico-controller artifact
	// of an NVKR release, so a bootstrap runs the bytes the release published
	// rather than a tag. Empty runs the manifest's own image.
	CAPNICoImage string

	// NodeAgentPort is the port CWE dials nke-agent on. Empty leaves CWE's own
	// default (15005) alone. It comes from the site spec, because it describes
	// how the site's hosts were configured long before this cluster existed.
	NodeAgentPort string

	// OIDCProviders are the external oidc providers site cluster kube-apiserver authenticates
	// client requests against. Optional field.
	OIDCProviders []sitespec.OIDCProvider

	// CWEPort is the port the controller API uses for its Helm Service, Kubernetes
	// NodePort, Kind host mapping, and host client. Zero uses DefaultCWEPort.
	CWEPort int32

	// PostgresPassword is the Postgres superuser password shared by the
	// Postgres, Temporal, and controller releases. Generated if empty.
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

	// AllowControlPlaneWithoutVIP permits dev control-plane lifecycle operations without a VIP.
	AllowControlPlaneWithoutVIP bool

	// Tracing exports the controller's spans to a collector the operator named.
	Tracing TracingConfig
}

// TPMAttestorEnabled reports whether the bootstrap SPIRE server attests hosts
// by TPM, and so is published to hosts outside the cluster. That is the case
// when the site spec listed endorsement-key thumbprints, or when CAPNICo creates
// the nodes and CWE learns their keys as it allocates them.
func (c Config) TPMAttestorEnabled() bool {
	return !c.DisableMTLS && (len(c.TPMEKFingerprints) > 0 || c.NicoInfrastructure)
}

// DefaultConfig reads the package's knobs from the environment, mirroring
// bootstrapengine.DefaultConfig's env-driven shape.
func DefaultConfig() Config {
	return Config{
		CWEImage: envOr("NVKECTL_NVKE_CONTROLLER_IMAGE", defaultImage(manifest.ComponentController, manifest.ImageController)),
		// No cluster-name variable of its own: the bootstrap cluster has one
		// name, NVKECTL_BOOTSTRAP_CLUSTER_NAME, and SPIRE is told that one.
		// Callers set SPIFFEClusterName; an empty trust domain is derived from
		// it below.
		SPIFFETrustDomain: getenv("NVKECTL_SPIFFE_TRUST_DOMAIN"),

		ProvisionedProviderImage: envOr("NVKECTL_CAPI_PROVISIONED_IMAGE", defaultImage(manifest.ComponentProvisioned, manifest.ImageManager)),
		CWEPort:                  envPort("NVKECTL_NVKE_CONTROLLER_PORT", DefaultCWEPort),
		// Added after the rename, so it has no NKECTL_ name to fall back to and
		// is not in Settings.
		ProfilesImage: os.Getenv(ProfilesImageEnv),

		HelmTimeout:  envDuration("NVKECTL_NVKE_CONTROLLER_HELM_TIMEOUT", 5*time.Minute),
		ReadyTimeout: envDuration("NVKECTL_NVKE_CONTROLLER_READY_TIMEOUT", 5*time.Minute),

		TemporalChartRef:  getenv("NVKECTL_NVKE_CONTROLLER_TEMPORAL_CHART"),
		SPIREChartRef:     getenv("NVKECTL_NVKE_CONTROLLER_SPIRE_CHART"),
		SPIRECRDsChartRef: getenv("NVKECTL_NVKE_CONTROLLER_SPIRE_CRDS_CHART"),
	}
}

// Settings is every environment variable this package reads. Each one that is
// not NVKECTL_NVKE_* is also read under the name it had while the binary was
// called nkectl, for one release; see envalias.
var Settings = []string{
	"NVKECTL_NVKE_CONTROLLER_IMAGE",
	"NVKECTL_CAPI_PROVISIONED_IMAGE",
	"NVKECTL_SPIFFE_TRUST_DOMAIN",
	"NVKECTL_NVKE_CONTROLLER_PORT",
	"NVKECTL_NVKE_CONTROLLER_HELM_TIMEOUT",
	"NVKECTL_NVKE_CONTROLLER_READY_TIMEOUT",
	"NVKECTL_NVKE_CONTROLLER_TEMPORAL_CHART",
	"NVKECTL_NVKE_CONTROLLER_SPIRE_CHART",
	"NVKECTL_NVKE_CONTROLLER_SPIRE_CRDS_CHART",
}

// DeprecatedSettings returns, sorted by old name, every setting in Settings
// that DefaultConfig reads under its previous name because the current name is
// unset. The caller warns about each; this package has no logger of its own at
// the point DefaultConfig runs.
func DeprecatedSettings() []envalias.Renamed {
	return envalias.Deprecated(Settings...)
}

// getenv returns the setting's value, or, when it is unset or empty, the value
// under its previous name.
func getenv(name string) string {
	return envalias.Get(name)
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

// Engine deploys the controller and waits for it to report healthy.
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
	return &Engine{Runner: runner, log: log.With("component", "controllerdeploy")}
}

// Deploy installs Postgres, Temporal, SPIRE, Cluster API, and the controller (in that
// order -- each depends on the one before it) onto the cluster kubeconfigPath
// points at, waiting for each Helm release to report ready before moving to
// the next. It does not wait for the controller's own /readyz; call WaitReady for that
// once Deploy returns, so callers can log "helm release installed" and
// "controller reports healthy" as distinct, separately-timed steps.
func (e *Engine) Deploy(ctx context.Context, kubeconfigPath string, cfg Config) error {
	if kubeconfigPath == "" {
		return errors.New("controllerdeploy: kubeconfigPath is required")
	}
	if cfg.CWEImage == "" {
		return errors.New("controllerdeploy: Config.CWEImage is required " +
			"(e.g. nvcr.io/<org>/nvke-controller:edge -- see .github/workflows/image-nvcr.yml)")
	}
	if cfg.NicoInfrastructure && !cfg.DisableMTLS && cfg.SPIREExternalAddress == "" {
		return errors.New("controllerdeploy: Config.SPIREExternalAddress is required with NicoInfrastructure; " +
			"nodes created by CAPNICo read the SPIRE server's address from their userdata, " +
			"and the in-cluster name does not reach them")
	}
	if cfg.NicoInfrastructure && (cfg.DGXCTenant.User == "" || cfg.DGXCTenant.Password == "") {
		return errors.New("controllerdeploy: Config.DGXCTenant is required with NicoInfrastructure; " +
			"the controller refuses to render a node's user data without DGXC_TENANT_USER and DGXC_TENANT_PASSWORD")
	}
	if cfg.NicoInfrastructure && cfg.ReleasesPath == "" {
		return errors.New("controllerdeploy: Config.ReleasesPath is required with NicoInfrastructure; " +
			"the controller cannot resolve the control plane's release without the release manifests")
	}
	if cfg.ProvisionedProviderImage == "" && !cfg.NicoInfrastructure {
		return errors.New("controllerdeploy: Config.ProvisionedProviderImage is required; the CAPI " +
			"objects the NVKE controller writes for a site cluster go unreconciled without it " +
			"(see Config.ProvisionedProviderImage)")
	}

	// The charts this binary was built with. helm takes a directory and this
	// process runs the helm binary rather than linking its loader, so they are
	// written out for the length of the deploy.
	chartPath, err := os.MkdirTemp("", "nvkectl-controller-chart-*")
	if err != nil {
		return fmt.Errorf("controllerdeploy: creating a directory for the controller chart: %w", err)
	}
	defer func() { _ = os.RemoveAll(chartPath) }()
	if err := charts.Extract(charts.NVKEController, chartPath); err != nil {
		return fmt.Errorf("controllerdeploy: writing out the controller chart: %w", err)
	}
	postgresChartPath, err := os.MkdirTemp("", "nvkectl-postgres-chart-*")
	if err != nil {
		return fmt.Errorf("controllerdeploy: creating a directory for the postgres chart: %w", err)
	}
	defer func() { _ = os.RemoveAll(postgresChartPath) }()
	if err := charts.Extract(charts.Postgresql, postgresChartPath); err != nil {
		return fmt.Errorf("controllerdeploy: writing out the postgres chart: %w", err)
	}

	password := cfg.PostgresPassword
	if password == "" {
		var err error
		password, err = randomPassword()
		if err != nil {
			return fmt.Errorf("controllerdeploy: generating postgres password: %w", err)
		}
	}
	redact.RegisterSecret(password)
	// The NICo token and the node password reach the cluster only as Secrets,
	// but a pod that logged one would otherwise carry it into the diagnostics.
	redact.RegisterSecret(cfg.CAPNICo.Token)
	redact.RegisterSecret(cfg.DGXCTenant.Password)

	helmTimeout := cfg.HelmTimeout
	if helmTimeout <= 0 {
		helmTimeout = 5 * time.Minute
	}
	spireClusterName := cfg.SPIFFEClusterName
	if spireClusterName == "" {
		spireClusterName = sitespec.DefaultBootstrapClusterName
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
	postgresSecrets, postgresSecretsCleanup, err := writeSecretValues(map[string]any{
		"auth": map[string]any{"password": password},
	})
	if err != nil {
		return err
	}
	defer postgresSecretsCleanup()

	e.log.Info("installing postgres", "namespace", postgresNamespace, "chart", postgresChartPath)
	if _, err := e.helm(ctx, kubeconfigPath,
		"upgrade", "--install", postgresReleaseName, postgresChartPath,
		"--namespace", postgresNamespace, "--create-namespace",
		"--values", postgresValues,
		"--values", postgresSecrets,
		"--wait", "--timeout", helmTimeout.String(),
	); err != nil {
		return fmt.Errorf("controllerdeploy: installing postgres: %w", err)
	}

	temporalValues, cleanup, err := writeEmbeddedFile(temporalValuesFile)
	if err != nil {
		return err
	}
	defer cleanup()
	persistence := map[string]any{
		"default": map[string]any{
			"sql": map[string]any{"password": password},
		},
		"visibility": map[string]any{
			"sql": map[string]any{"password": password},
		},
	}

	temporalSecrets, temporalSecretsCleanup, err := writeSecretValues(map[string]any{
		"server": map[string]any{
			"config": map[string]any{
				"persistence": persistence,
			},
		},
	})
	if err != nil {
		return err
	}
	defer temporalSecretsCleanup()

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
		"--values", temporalSecrets,
		"--version", temporalChart.Version,
		"--wait", "--timeout", helmTimeout.String(),
	)
	if _, err := e.helm(ctx, kubeconfigPath, temporalArgs...); err != nil {
		return fmt.Errorf("controllerdeploy: installing temporal: %w", err)
	}

	// --disable-mtls stands the site up without SPIFFE, so nothing needs SPIRE
	// and the controller dials each agent in plaintext. Bring-up debugging only.
	if cfg.DisableMTLS {
		e.log.Warn("SPIFFE is disabled: skipping SPIRE, and the controller will dial node agents in plaintext")
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
			return fmt.Errorf("controllerdeploy: installing SPIRE CRDs: %w", err)
		}

		spireValues, cleanup, err := writeEmbeddedFile(spireValuesFile)
		if err != nil {
			return err
		}
		defer cleanup()

		// The controller uses SPIFFE workload identity even when the site spec contains no
		// external TPM nodes, so the SPIRE deployment is always required.
		e.log.Info("installing SPIRE", "namespace", spireNamespace, "version", spireChart.Version,
			"clusterName", spireClusterName, "trustDomain", spireTrustDomain,
			"tpmThumbprints", len(cfg.TPMEKFingerprints))
		if _, err := e.helm(ctx, kubeconfigPath,
			spireInstallArgs(cfg, spireChart, spireValues, spireClusterName, spireTrustDomain, helmTimeout)...,
		); err != nil {
			return fmt.Errorf("controllerdeploy: installing SPIRE: %w", err)
		}

		// Only external TPM nodes need the NodePort. The controller and the in-cluster agents
		// use SPIRE's ClusterIP service.
		if cfg.TPMAttestorEnabled() {
			if err := e.publishSpireExternally(ctx, kubeconfigPath); err != nil {
				return err
			}
		}
	}

	// Before the controller, not after: the controller starts writing CAPI objects as soon as a
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
	cweSecrets, cweSecretsCleanup, err := writeSecretValues(map[string]any{
		"appDb": map[string]any{"postgresPassword": password},
	})
	if err != nil {
		return err
	}
	defer cweSecretsCleanup()

	oidcValues := ""
	if len(cfg.OIDCProviders) > 0 {
		oidcValues, cleanup, err = writeOIDCValues(cfg.OIDCProviders)
		if err != nil {
			return err
		}
		defer cleanup()
	}

	// The Secret has to exist before the controller's pod does: the chart wires
	// DGXC_TENANT_USER and DGXC_TENANT_PASSWORD from it only when told its name.
	if cfg.NicoInfrastructure {
		if err := e.applyTenantCredentials(ctx, kubeconfigPath, cfg.DGXCTenant); err != nil {
			return err
		}
	}

	// tag and digest are always set, empty when unused: the chart reads an
	// empty tag as "use appVersion" and an empty digest as "pull by tag".
	imageRepo, imageTag, imageDigest := splitImageRef(cfg.CWEImage)
	e.log.Info("installing the controller", "namespace", cweNamespace, "image", cfg.CWEImage,
		"chart", chartPath)
	cweArgs := []string{
		"upgrade", "--install", cweReleaseName, chartPath,
		"--namespace", cweNamespace, "--create-namespace",
		"--values", cweValues,
		"--values", cweSecrets,
		// The chart then builds the database URL into a Secret and the pod reads
		// it by reference. Without it the password sits in the pod spec, where
		// `kubectl describe` and the diagnostics dump show it.
		"--set", "secrets.createDbSecret=true",
		"--set-string", "image.repository=" + imageRepo,
		"--set-string", "image.tag=" + imageTag,
		"--set-string", "image.digest=" + imageDigest,
		"--set-string", "image.pullPolicy=IfNotPresent",
		"--set-string", "profiles.path=" + profilesPath,
		"--set", "cni.installAfterInit=true",
		"--set", fmt.Sprintf("allowControlPlaneWithoutVIP=%t", cfg.AllowControlPlaneWithoutVIP),
		"--set", fmt.Sprintf("spiffe.enabled=%t", !cfg.DisableMTLS),
		"--set-string", "spiffe.trustDomain=" + spireTrustDomain,
		"--set-string", "spiffe.spire.className=" + spireControllerManagerClass,
		"--set-string", "spiffe.spire.serverAddress=" + spireNodeAddress(cfg),
		"--set-string", "spiffe.spire.serverPort=" + spireNodePort(cfg),
		"--set-string", "supportedInfraProviders=" + supportedInfraProviders(cfg),
		"--set", fmt.Sprintf("controllers.nico.enabled=%t", cfg.NicoInfrastructure),
		"--set", fmt.Sprintf("service.port=%d", cwePort),
	}
	profilesArgs, err := profilesImageArgs(cfg)
	if err != nil {
		return err
	}
	cweArgs = append(cweArgs, profilesArgs...)
	otlpHeaders := ""
	if cfg.Tracing.Endpoint != "" {
		cweArgs = append(cweArgs,
			"--set", "tracing.enabled=true",
			"--set-string", "tracing.endpoint="+cfg.Tracing.Endpoint,
			"--set", fmt.Sprintf("tracing.insecure=%t", cfg.Tracing.Insecure),
		)
		otlpHeaders = telemetry.FormatHeaders(cfg.Tracing.Headers)
	}
	envArgs, removeEnvValuesFile, err := cweEnvHelmArgs(cfg, otlpHeaders)
	if err != nil {
		return err
	}
	// Removed as soon as helm returns; the defer covers an early return or a panic.
	defer removeEnvValuesFile()
	cweArgs = append(cweArgs, envArgs...)
	if cfg.NicoInfrastructure {
		cweArgs = append(cweArgs, "--set-string", "secrets.dgxcTenantCredSecretName="+dgxcTenantSecret)
	}
	cweArgs = append(cweArgs, "--wait", "--timeout", helmTimeout.String())
	if oidcValues != "" {
		cweArgs = append(cweArgs, "--values", oidcValues)
	}
	_, err = e.helm(ctx, kubeconfigPath, cweArgs...)
	removeEnvValuesFile()
	if err != nil {
		return fmt.Errorf("controllerdeploy: installing the controller: %w", err)
	}

	// The chart installed the release CRDs, so the manifests can be applied now,
	// before the site cluster is created.
	if cfg.NicoInfrastructure {
		if err := e.applyReleases(ctx, kubeconfigPath, cfg.ReleasesPath); err != nil {
			return err
		}
	}

	// The API has to be reachable from the host that ran this command: it is
	// what creates the cluster next. See Config.CWEPort.
	return e.publishCWEExternally(ctx, kubeconfigPath, cwePort)
}

// TenantLogin is the user and password the controller renders into the
// cloud-init of the nodes it creates.
type TenantLogin struct {
	User     string
	Password string
}

// applyTenantCredentials applies the Secret secrets.dgxcTenantCredSecretName
// names, with the keys the chart reads (user and password), in the controller's
// namespace. It goes through a 0600 file, not the command line, as the NICo
// credentials do.
func (e *Engine) applyTenantCredentials(ctx context.Context, kubeconfigPath string, login TenantLogin) error {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "List",
		"items": []any{
			map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": cweNamespace}},
			map[string]any{
				"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
				"metadata":   map[string]any{"name": dgxcTenantSecret, "namespace": cweNamespace},
				"stringData": map[string]string{"user": login.User, "password": login.Password},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("controllerdeploy: encoding the tenant credentials: %w", err)
	}
	file, cleanup, err := writeTempFile("nvkectl-tenant-credentials-*.json", body)
	if err != nil {
		return err
	}
	defer cleanup()

	e.log.Info("applying the tenant node credentials", "namespace", cweNamespace, "secret", dgxcTenantSecret)
	if out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"apply", "--server-side", "--force-conflicts", "-f", file); err != nil {
		return fmt.Errorf("controllerdeploy: applying the tenant credentials: %w\n%s", err, out)
	}
	return nil
}

// applyReleases applies the MachineImage and ClusterRelease manifests at path,
// a file or a directory, so the controller can resolve an release from them.
func (e *Engine) applyReleases(ctx context.Context, kubeconfigPath, path string) error {
	e.log.Info("applying the release manifests", "path", path)
	if out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"apply", "--server-side", "--force-conflicts", "-f", path); err != nil {
		return fmt.Errorf("controllerdeploy: applying the release manifests at %s: %w\n%s", path, err, out)
	}
	return nil
}

// ProfilesImageEnv names the environment variable that sets Config.ProfilesImage.
const ProfilesImageEnv = "NVKECTL_NVKE_CONTROLLER_PROFILES_IMAGE"

// ValidateProfilesImage reports an error when image cannot be used as the
// profiles image. An empty image is valid: no profiles image is deployed. The
// check is cheap and needs no cluster, so callers run it before anything is
// built.
func ValidateProfilesImage(image string) error {
	if image == "" {
		return nil
	}
	// A colon with nothing after it is not a tag at all, and splitImageRef would
	// report it as an empty one that the latest check below lets through.
	if repo, _, _ := strings.Cut(image, "@"); strings.HasSuffix(repo, ":") {
		return fmt.Errorf("controllerdeploy: the profiles image %q (%s) has an empty tag: nothing follows the colon", image, ProfilesImageEnv)
	}
	_, tag, digest := splitImageRef(image)
	// splitImageRef reports an untagged reference as latest. Kubernetes pulls
	// latest unconditionally, and the chart gives this image no pull policy to
	// override that, so a side-loaded image would never be found.
	if digest == "" && tag == "latest" {
		return fmt.Errorf("controllerdeploy: the profiles image %q (%s) must carry a tag other than latest, or a digest: "+
			"the chart sets no pull policy on it, so a latest tag makes the cluster pull it instead of using the "+
			"side-loaded copy", image, ProfilesImageEnv)
	}
	return nil
}

// profilesImageArgs turns Config.ProfilesImage into the chart values that make
// its init container copy the profiles into place. Nothing is added when no
// image is set.
func profilesImageArgs(cfg Config) ([]string, error) {
	if cfg.ProfilesImage == "" {
		return nil, nil
	}
	if err := ValidateProfilesImage(cfg.ProfilesImage); err != nil {
		return nil, err
	}
	repo, tag, digest := splitImageRef(cfg.ProfilesImage)
	return []string{
		"--set", "profiles.image.enabled=true",
		"--set-string", "profiles.image.repository=" + repo,
		"--set-string", "profiles.image.tag=" + tag,
		"--set-string", "profiles.image.digest=" + digest,
	}, nil
}

// cweEnvVars lists the settings the controller reads from its own environment,
// as name and value pairs. A setting left at its zero value is omitted, leaving
// the controller's own default in place.
func cweEnvVars(cfg Config) [][2]string {
	var vars [][2]string
	if cfg.NodeAgentPort != "" {
		vars = append(vars, [2]string{"NODE_AGENT_PORT", cfg.NodeAgentPort})
	}
	return vars
}

// cweEnvHelmArgs returns the helm arguments that set the controller's
// environment, and a function that removes any file they name. Without OTLP
// headers they are --set-string arguments. With them, every entry goes in a
// values file, which keeps the headers off the command line.
func cweEnvHelmArgs(cfg Config, otlpHeaders string) (args []string, removeValuesFile func(), err error) {
	if otlpHeaders == "" {
		return cweEnvArgs(cfg), func() {}, nil
	}
	// A values file's env list does not merge with --set env[i] entries, so the
	// file carries every entry.
	vars := append(cweEnvVars(cfg), [2]string{otlpHeadersEnv, otlpHeaders})
	valuesFile, removeValuesFile, err := writeEnvValues(vars)
	if err != nil {
		return nil, func() {}, err
	}
	return []string{"--values", valuesFile}, removeValuesFile, nil
}

// writeEnvValues writes vars as the chart's `env` list to a values file for
// helm's --values, as writeSecretValues does.
func writeEnvValues(vars [][2]string) (path string, cleanup func(), err error) {
	env := make([]map[string]string, 0, len(vars))
	for _, v := range vars {
		env = append(env, map[string]string{"name": v[0], "value": v[1]})
	}
	return writeSecretValues(map[string]any{"env": env})
}

// cweEnvArgs renders the settings the controller reads from its own environment as helm
// arguments over the chart's generic `env` list.
func cweEnvArgs(cfg Config) []string {
	vars := cweEnvVars(cfg)
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

// publishCWEExternally applies the NodePort Service the CLI reaches the controller API
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

	e.log.Info("publishing the controller api for the host", "namespace", cweNamespace, "port", port)
	if out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"-n", cweNamespace, "apply", "-f", manifest); err != nil {
		return fmt.Errorf("controllerdeploy: publishing the controller api with %s: %w\n%s", cweNodePortFile, err, out)
	}
	return nil
}

// readyzResponse mirrors controller/internal/server/health.go's
// healthResponse.
type readyzResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// WaitReady polls the controller's /readyz endpoint through the Kubernetes API server's
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
				e.log.Info("controller reports healthy", "checks", resp.Checks)
				return nil
			}
		}

		select {
		case <-ctx.Done():
			lastErrStr := "<none>"
			if lastErr != nil {
				lastErrStr = lastErr.Error()
			}
			return fmt.Errorf("controllerdeploy: the controller did not report healthy on %s within %s: last error=%s last response=%q",
				readyzPath, timeout, lastErrStr, strings.TrimSpace(lastOut))
		case <-time.After(waitReadyPollInterval):
		}
	}
}

// helm runs the helm CLI with kubeconfigPath and args.
func (e *Engine) helm(ctx context.Context, kubeconfigPath string, args ...string) (string, error) {
	fullArgs := append([]string{"--kubeconfig", kubeconfigPath}, args...)
	// The credentials are in values files, not arguments. Masking the registered
	// ones anyway keeps a future argument that carries one out of the error.
	shown := redact.Text(strings.Join(args, " "))
	for attempt := 1; ; attempt++ {
		out, err := e.Runner.Run(ctx, "helm", fullArgs...)
		out = redact.Text(out)
		if err == nil {
			return out, nil
		}
		// GitHub's release downloads answer 5xx now and then; nothing was installed yet, so the same command is safe to repeat.
		if attempt == helmFetchAttempts || !chartFetchServerError.MatchString(out) {
			return out, fmt.Errorf("helm %s: %w\n%s", shown, err, out)
		}
		e.log.Warn("helm chart download failed, retrying", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return out, fmt.Errorf("helm %s: %w\n%s", shown, ctx.Err(), out)
		case <-time.After(helmFetchRetryInterval):
		}
	}
}

func writeOIDCValues(providers []sitespec.OIDCProvider) (path string, cleanup func(), err error) {
	body, err := yaml.Marshal(oidcValues{
		OIDC: oidcProviders{Providers: providers},
	})
	if err != nil {
		return "", func() {}, fmt.Errorf("controllerdeploy: rendering oidc values: %w", err)
	}
	return writeTempFile("nvkectl-cwe-oidc-*.yaml", body)
}

// writeEmbeddedFile copies an embedded values file to a temporary file on
// disk (helm's -f/--values flag needs a real path) and returns a cleanup
// func the caller should defer immediately.
func writeEmbeddedFile(name string) (path string, cleanup func(), err error) {
	body, err := manifestsFS.ReadFile(name)
	if err != nil {
		return "", func() {}, fmt.Errorf("controllerdeploy: reading embedded %s: %w", name, err)
	}
	return writeTempFile("nvkectl-cwedeploy-*.yaml", body)
}

// writeEmbeddedTemplate renders an embedded YAML template to a temporary file.
func writeEmbeddedTemplate(name string, data any) (path string, cleanup func(), err error) {
	body, err := manifestsFS.ReadFile(name)
	if err != nil {
		return "", func() {}, fmt.Errorf("controllerdeploy: reading embedded %s: %w", name, err)
	}
	tmpl, err := template.New(name).Parse(string(body))
	if err != nil {
		return "", func() {}, fmt.Errorf("controllerdeploy: parsing embedded %s: %w", name, err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return "", func() {}, fmt.Errorf("controllerdeploy: rendering embedded %s: %w", name, err)
	}
	return writeTempFile("nvkectl-cwedeploy-*.yaml", rendered.Bytes())
}

// writeTempFile writes body to a temporary file matching pattern and returns
// the path plus a cleanup func the caller should defer immediately. Both the
// embedded values files and the rendered provider manifest need a real path on
// disk, because helm's --values and kubectl's -f both take one.
func writeTempFile(pattern string, body []byte) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", func() {}, fmt.Errorf("controllerdeploy: creating %s: %w", pattern, err)
	}
	tmpName := f.Name()
	cleanup = func() { _ = os.Remove(tmpName) }
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("controllerdeploy: writing %s: %w", tmpName, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("controllerdeploy: closing %s: %w", tmpName, err)
	}
	return tmpName, cleanup, nil
}

// writeSecretValues writes values, which hold credentials, to a values file for
// helm's --values and returns its path plus a cleanup func the caller should
// defer immediately. A credential goes in a file, not on the command line,
// where it would show in `ps` and in the command an error prints. The file is
// created 0600, and an error from here names the path, never the contents.
func writeSecretValues(values map[string]any) (path string, cleanup func(), err error) {
	body, err := yaml.Marshal(values)
	if err != nil {
		return "", func() {}, fmt.Errorf("controllerdeploy: rendering a values file: %w", err)
	}
	return writeTempFile("nvkectl-secret-values-*.yaml", body)
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

// spireNodeAddress and spireNodePort are where CWE tells nodes to find SPIRE.
// Only the node userdata CWE renders reads them; CWE does not dial them itself.
//
// Hosts that already exist were given the address when they were provisioned, so
// the in-cluster name is only a placeholder for them. Nodes CAPNICo creates have
// nothing but this userdata, so they get the address and NodePort the bootstrap
// publishes the server on.
func spireNodeAddress(cfg Config) string {
	if cfg.NicoInfrastructure {
		return cfg.SPIREExternalAddress
	}
	return spireServiceHost()
}

func spireNodePort(cfg Config) string {
	if cfg.NicoInfrastructure {
		return strconv.Itoa(int(SPIREServerNodePort))
	}
	return spireServerPort
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
		return manifest.Chart{}, fmt.Errorf("controllerdeploy: the embedded bootstrap manifest pins no chart for %q", component)
	}
	return *comp.Chart, nil
}

// supportedInfraProviders is the provider payload CWE accepts on CreateCluster.
// The site cluster's request carries exactly one of them, and CWE rejects the
// other, so it has to match what the bootstrap sends.
func supportedInfraProviders(cfg Config) string {
	if cfg.NicoInfrastructure {
		return "nico"
	}
	return "infra"
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
// nvkectl cannot know ahead of time is computed here — the trust domain and
// cluster name, the controller's admin identity, the CA to chain to, and the thumbprints
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
		"--set-string", prefix+".adminIDs[0]=spiffe://"+trustDomain+"/nvke-service/"+cweReleaseName,
		// Both halves of the two agreements the controller depends on. See the constants.
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
// No fingerprints means no TPM attestor, except with NicoInfrastructure: a
// bootstrap that has not been told which hardware it is entitled to admit must
// not admit any, and the hosts on that path run without a SPIRE identity at
// all. With nico the attestor is on without any, and the allowlist starts
// empty: CWE adds each node's key as CAPNICo allocates the machine, so the
// hardware admitted is what NICo handed this site.
//
// Reaching the server from outside is a separate concern, handled by
// spire-server-nodeport.yaml rather than by the chart's own Service.
func tpmAttestorArgs(cfg Config, prefix string) []string {
	if !cfg.TPMAttestorEnabled() {
		return nil
	}
	args := []string{"--set", prefix + ".nodeAttestor.tpmDirect.enabled=true"}
	if checksum := SpireTPMDirectChecksum(runtime.GOARCH); checksum != "" {
		args = append(args, "--set-string", prefix+".nodeAttestor.tpmDirect.checksum="+checksum)
	}
	if len(cfg.TPMEKFingerprints) == 0 {
		return append(args, tpmDynamicAllowlistArgs(prefix)...)
	}
	for i, fingerprint := range cfg.TPMEKFingerprints {
		args = append(args, "--set-string",
			fmt.Sprintf("%s.nodeAttestor.tpmDirect.hashes[%d]=%s", prefix, i, fingerprint))
	}
	return args
}

// tpmDynamicAllowlistArgs mounts the ConfigMap CWE keeps the allowlist in on the
// directory the TPM attestor reads.
//
// The chart reads its own static hashes list or this directory, never both, so
// this is for a bootstrap with no thumbprints to list: CAPNICo reports each
// machine's key as it allocates the machine, and CWE adds it to the ConfigMap.
// The attestor looks the key up on every attestation, so an entry takes effect
// as soon as the kubelet syncs the ConfigMap into the pod. Optional, so the
// server starts before CWE has written it.
func tpmDynamicAllowlistArgs(prefix string) []string {
	return []string{
		"--set-string", prefix + ".extraVolumes[0].name=" + tpmPubhashesVolume,
		"--set-string", prefix + ".extraVolumes[0].configMap.name=" + tpmPubhashesConfigMap,
		"--set", prefix + ".extraVolumes[0].configMap.optional=true",
		"--set-string", prefix + ".extraVolumeMounts[0].name=" + tpmPubhashesVolume,
		"--set-string", prefix + ".extraVolumeMounts[0].mountPath=" + tpmHashesMountPath,
		"--set", prefix + ".extraVolumeMounts[0].readOnly=true",
	}
}

// SpireTPMDirectChecksum returns the sha256 of the TPM attestor server plugin
// for goarch, amd64 or arm64, or "" for any other. A wrong value crash-loops
// spire-server, so every installer takes it from here.
func SpireTPMDirectChecksum(goarch string) string {
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
		return fmt.Errorf("controllerdeploy: publishing SPIRE with %s: %w\n%s", spireNodePortFile, err, out)
	}
	return nil
}

// TenantKubeconfig waits up to timeout for the asynchronously published admin
// kubeconfig for clusterName and returns it. A non-positive timeout uses the
// package default.
//
// The controller writes it as a Secret in nvke-system, which is also the namespace it puts
// the cluster's CAPI objects in — so this is the same Secret Cluster API
// resolves by name for the cluster, and the one
// cluster-api-provider-provisioned reads to set each Node's provider ID.
//
// It is the plain admin kubeconfig. The controller only rewrites it to a SPIRE exec-auth
// kubeconfig when OIDC authentication is enabled on the cluster, which a
// bootstrap does not turn on (see nvkeOIDCAuth in manifests/controller-values.yaml).
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
			return nil, fmt.Errorf("controllerdeploy: tenant kubeconfig for %q was not published within %s: %w",
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
		return nil, fmt.Errorf("controllerdeploy: reading %s/%s: %w\n%s", tenantSecretsNamespace, secret, err, out)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return nil, fmt.Errorf("controllerdeploy: decoding %s/%s: %w", tenantSecretsNamespace, secret, err)
	}
	if len(decoded) == 0 {
		return nil, fmt.Errorf("controllerdeploy: %s/%s holds an empty kubeconfig", tenantSecretsNamespace, secret)
	}
	return decoded, nil
}

// DumpDiagnostics writes everything the cluster can say about why a bootstrap
// failed into dir.
//
// It runs before the cluster is torn down, which is the only moment it can:
// `bootstrap site` removes the cluster on every exit path, so by the time a
// caller sees the error the evidence is gone. That is fine when the failure
// explains itself and useless when it does not — "the controller never reported healthy"
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
//
// DumpDiagnostics masks the registered credentials in what it collected. It
// reads and changes only the files it collected: any other file in dir is left
// as it was, and a file of the same name is replaced. A collected file it cannot
// scrub is not written to dir, and nothing is written there before it is scrubbed.
func (e *Engine) DumpDiagnostics(ctx context.Context, kubeconfigPath, dir string) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		e.log.Warn("could not create the diagnostics directory", "dir", dir, "err", err)
		return
	}
	// dir can hold other files, such as the caller's own, and is the directory that
	// gets shared. The dump goes into a fresh private directory outside it, so the
	// scrub only touches what this call wrote, and a dump that could not be cleaned
	// up cannot be left in dir.
	staging, err := os.MkdirTemp("", "nvkectl-diagnostics-*")
	if err != nil {
		e.log.Warn("could not create a staging directory for the diagnostics", "err", err)
		return
	}
	defer e.removeStaging(staging)

	if _, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"cluster-info", "dump", "--all-namespaces", "--output-directory", staging); err != nil {
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
		if err := os.WriteFile(filepath.Join(staging, name), []byte(out), 0o600); err != nil {
			e.log.Warn("could not write a diagnostics file", "file", name, "err", err)
		}
	}

	out, err := e.Runner.Run(ctx, "helm", "--kubeconfig", kubeconfigPath, "list", "-A")
	if err != nil {
		out += "\n" + err.Error()
	}
	if err := os.WriteFile(filepath.Join(staging, "helm-releases.txt"), []byte(out), 0o600); err != nil {
		e.log.Warn("could not write a diagnostics file", "file", "helm-releases.txt", "err", err)
	}

	e.publishDiagnostics(staging, dir, e.redactDiagnostics(staging, dir))
}

// redactDiagnostics masks credentials in everything under staging. It returns
// the paths under staging that it could not scrub, and warns about each, named
// as it would be under dir.
func (e *Engine) redactDiagnostics(staging, dir string) map[string]bool {
	unscrubbed := map[string]bool{}
	for _, path := range redact.Tree(staging) {
		unscrubbed[path] = true
		rel, err := filepath.Rel(staging, path)
		if err != nil {
			rel = path
		}
		e.log.Warn("could not scrub part of the diagnostics; it is not published", "path", filepath.Join(dir, rel))
	}
	return unscrubbed
}

// publishDiagnostics copies every regular file under staging to the same relative
// path under dir, replacing a file that is already there. It leaves out each path
// in withheld, and everything under it.
func (e *Engine) publishDiagnostics(staging, dir string, withheld map[string]bool) {
	// dir is the caller's, so it is trusted as given, even if it is a symlink. What
	// is under it is not: it can hold links the bundle must not be written through.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		e.log.Warn("could not resolve the diagnostics directory", "dir", dir, "err", err)
		return
	}
	_ = filepath.WalkDir(staging, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			e.log.Warn("could not read part of the collected diagnostics", "path", path, "err", err)
			return nil
		}
		if withheld[path] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// The scrub only reads regular files, and a copy follows a symlink to
		// whatever it points at, so anything else is left out.
		if !d.Type().IsRegular() {
			e.log.Warn("not publishing a diagnostics entry that is not a regular file", "path", path)
			return nil
		}
		rel, err := filepath.Rel(staging, path)
		if err != nil {
			e.log.Warn("could not place a diagnostics file", "path", path, "err", err)
			return nil
		}
		dst, err := destination(root, rel)
		if err != nil {
			e.log.Warn("not publishing a diagnostics file", "path", filepath.Join(dir, rel), "err", err)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			e.log.Warn("could not place a diagnostics file", "path", dst, "err", err)
			return nil
		}
		if err := copyFile(path, dst, info.Mode().Perm()); err != nil {
			e.log.Warn("could not place a diagnostics file", "path", dst, "err", err)
		}
		return nil
	})
}

// destination returns where rel goes under root, creating the directories on the
// way. It fails if rel is not a path inside root, or if a directory on the way is
// a symlink or is not a directory, so a publish cannot be redirected to write
// outside root. It is safe to call concurrently for paths that share directories.
func destination(root, rel string) (string, error) {
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%q is not a path inside the directory", rel)
	}
	cur := root
	for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		// Create first and check after. Checking first leaves a gap in which
		// someone else can make the directory, and that would be reported as a
		// failure. A symlink already there is also "exists", and is caught below.
		if err := os.Mkdir(cur, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		info, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%s is a %s, not a directory", cur, info.Mode().Type())
		}
	}
	return filepath.Join(root, rel), nil
}

// copyFile copies src to dst with mode perm, replacing dst. It writes a temporary
// file next to dst first, so a failure leaves neither a partial dst nor a stray
// file, and dst is never half-written.
func copyFile(src, dst string, perm fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".nvkectl-copy-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, in); err != nil {
		return err
	}
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// removeStaging deletes the staging directory, which holds the dump as the
// cluster produced it, before any scrub. A directory the dump left unreadable
// blocks the removal, so the directories are made accessible and it is retried.
// If it still cannot be removed, the path is logged, since it holds credentials
// and only the caller can delete it.
func (e *Engine) removeStaging(staging string) {
	if err := os.RemoveAll(staging); err == nil {
		return
	}
	_ = filepath.WalkDir(staging, func(path string, d fs.DirEntry, _ error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(staging); err != nil {
		e.log.Error("could not remove the temporary diagnostics, which hold credentials; delete this directory",
			"path", staging, "err", err)
	}
}
