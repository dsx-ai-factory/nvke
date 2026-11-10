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
	"strconv"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/manifest"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
	"github.com/dsx-ai-factory/nvke/cli/internal/redact"
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

	// AllowControlPlaneWithoutVIP permits dev control-plane lifecycle operations without a VIP.
	AllowControlPlaneWithoutVIP bool
}

// TPMAttestorEnabled reports whether the bootstrap SPIRE server attests hosts
// by TPM, and so is published to hosts outside the cluster. That is the case
// when the site spec listed endorsement-key thumbprints, or when CAPNICo creates
// the nodes and CWE learns their keys as it allocates them.
func (c Config) TPMAttestorEnabled() bool {
	return len(c.TPMEKFingerprints) > 0 || c.NicoInfrastructure
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

// TenantLogin is the user and password the controller renders into the
// cloud-init of the nodes it creates.
type TenantLogin struct {
	User     string
	Password string
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

// defaultImage returns the image reference the bootstrap manifest carries for
// one of the images this repository builds. Empty when the manifest has none.
func defaultImage(component, image string) string {
	comp, ok := manifest.Load().Components[component]
	if !ok {
		return ""
	}
	return comp.Images[image].Ref
}
