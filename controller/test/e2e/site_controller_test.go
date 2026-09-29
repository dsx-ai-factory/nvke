// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/discovery"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/config"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/environment"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/infra"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/infra/forge"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/infra/teleport"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/nke"
	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/site"
)

// The stages of the site lane, each doing everything the one before it did.
const (
	siteStageProvision = "provision" // a Forge node, reachable, then released
	siteStageBootstrap = "bootstrap" // plus vanilla Kubernetes from nkectl
	siteStageFull      = "full"      // plus the NKE stack, CWE, and a tenant
)

// Secrets the site lane reads from the environment. They are never written to
// the config file, a values file on the runner, or a command line.
const (
	envForgeAPIKey   = "FORGE_API_KEY"
	envRegistryUser  = "GHCR_USERNAME"
	envRegistryToken = "GHCR_TOKEN"
	// envJumpHostPassword selects the plain-SSH route to the jump host.
	// Unset uses Teleport.
	envJumpHostPassword = "JUMP_HOST_PASSWORD"
)

// Host tools the node installs for nkectl, pinned to what has built a site on
// the Forge hardware. Each can be overridden through -e2e.versions, as can
// capi-provisioned, the only published CAPI provider tag there is.
const (
	siteDefaultNerdctlVersion         = "2.3.5"
	siteDefaultKindVersion            = "v0.30.0"
	siteDefaultClusterctlVersion      = "v1.12.3"
	siteDefaultCAPIProvisionedVersion = "v0.1.0-rc.1"
)

// The shape of the site the lane bootstraps.
const (
	siteCNI = CNIPluginOVN

	// siteDisableMTLS bootstraps without SPIFFE. The alternative needs the
	// node's TPM EK thumbprint in the site spec, and Forge does not report
	// one: run 36016817952 installed SPIRE with tpmThumbprints=0, so no TPM
	// attestor was deployed, the agent could never attest, and the cluster
	// sat in Provisioning for its full 30 minutes without an error. The
	// procedure that has actually built a site on Forge bootstraps this way
	// too, and turns SPIRE and TPM on in the deploy path afterwards -- which
	// the full stage still exercises.
	//
	// A constant rather than derived from infra.Node.TPMEkThumbprint: it
	// also decides the node agent's arguments, which cloud-init writes
	// before the node, and so its thumbprint, exists. See nodeInputs.
	siteDisableMTLS = true

	// siteCAPNICoRegistry is the one registry the node pulls from itself:
	// CAPNICo's manifest is its published release asset, which names a GHCR
	// image, and the deploy path's ghcr-imagepull Secret carries one
	// registry. Everything else is staged. The credential is envRegistryUser
	// and envRegistryToken.
	siteCAPNICoRegistry = "ghcr.io"
)

var (
	siteStage string
	keepSite  bool
)

func init() {
	flag.StringVar(&siteStage, "e2e.site-stage", "",
		"build a site controller on Forge up to this stage: provision, bootstrap or full (empty skips the site lane)")
	flag.BoolVar(&keepSite, "e2e.keep-site", false,
		"keep the site controller's node after the run, for debugging")
}

// siteReaches is what the node must reach for bootstrap and the deploy path:
// image registries, chart repositories, and the release downloads cloud-init
// and the StorageClass fetch.
var siteReaches = []string{
	"https://registry-1.docker.io/v2/",
	"https://ghcr.io/v2/",
	"https://spiffe.github.io/helm-charts-hardened/index.yaml",
	"https://go.temporal.io/helm-charts/index.yaml",
	"https://github.com/",
	"https://raw.githubusercontent.com/",
}

// The lane is serial: one site, built once, holding the tenant specs that run
// against it. ContinueOnFailure lets teardown and the tenant sweep run after a
// failure; a failed build still skips the tenants, which check for a site.
var _ = Describe("Site controller on Forge", Ordered, ContinueOnFailure, Label("site-controller"), func() {
	var (
		lane    siteLane
		node    infra.Node
		session infra.Session
		built   site.Result
		failed  bool
	)

	BeforeAll(func() {
		switch siteStage {
		case "":
			Skip("-e2e.site-stage is not set")
		case siteStageProvision, siteStageBootstrap, siteStageFull:
		default:
			Fail(fmt.Sprintf("-e2e.site-stage %q is not provision, bootstrap or full", siteStage))
		}
		// An archive an earlier run left behind describes another node.
		if err := os.Remove(siteDiagnostics); err != nil && !errors.Is(err, os.ErrNotExist) {
			Fail(fmt.Sprintf("removing a stale %s: %v", siteDiagnostics, err))
		}
		lane = expectSucceed(newSiteLane())
	})

	AfterEach(func() { failed = failed || CurrentSpecReport().Failed() })

	It("provisions a node", func(ctx SpecContext) {
		var err error
		node, session, err = lane.provider.ProvisionNode(ctx, infra.NodeRequest{
			Name:         lane.name,
			Capabilities: lane.capabilities,
			Labels:       map[string]string{"purpose": "nke-e2e-site-controller"},
		})
		Expect(err).NotTo(HaveOccurred(), "should provision and reach a Forge node")
		GinkgoLogr.Info("site node", "name", node.Name, "ip", node.IP, "tpmEkThumbprint", node.TPMEkThumbprint)

		out, err := session.Run(ctx, "uname -srm && . /etc/os-release && echo \"$PRETTY_NAME\"")
		Expect(err).NotTo(HaveOccurred(), "should run a command on the node: %s", out)
		GinkgoLogr.Info("site node OS", "uname", strings.TrimSpace(out))
	}, NodeTimeout(45*time.Minute))

	It("reaches what bootstrap and the deploy path pull from", func(ctx SpecContext) {
		if session == nil {
			Skip("no node")
		}
		var unreachable []string
		for _, url := range siteReaches {
			if out, err := session.Run(ctx, "curl -sS -o /dev/null --max-time 15 "+url); err != nil {
				unreachable = append(unreachable, fmt.Sprintf("%s: %s", url, strings.TrimSpace(out)))
			}
		}
		Expect(unreachable).To(BeEmpty(), "the node needs egress to these")
	}, NodeTimeout(5*time.Minute))

	if siteStage == siteStageBootstrap || siteStage == siteStageFull {
		It("bootstraps the site controller", func(ctx SpecContext) {
			if session == nil {
				Skip("no node")
			}
			candidate, opts := expectSucceed2(lane.candidate(node))
			var err error
			built, err = site.Bootstrap(ctx, node, session, candidate, opts)
			if err != nil {
				collectSiteDiagnostics(ctx, session, lane.placedSecrets()...)
			}
			Expect(err).NotTo(HaveOccurred(), "should build the site controller")
			GinkgoLogr.Info("site controller", "kubeconfig", built.Kubeconfig, "cweVersion", built.Version)
		}, NodeTimeout(3*time.Hour))
	}

	if siteStage == siteStageFull {
		DescribeTableSubtree("tenant on the built site", tenantSubtree(func() (managementCluster, error) {
			if built.Kubeconfig == "" || session == nil {
				return managementCluster{}, errors.New("the site controller was not built")
			}
			return managementCluster{
				Kubeconfig:  built.Kubeconfig,
				TrustDomain: site.TrustDomain(lane.name),
				Relay:       sessionRelay(session),
			}, nil
		}),
			// Labeled nico-forge because it is: the same tenant the
			// standalone lane creates, on a site this lane built rather than
			// one that already existed. Selecting between the two is
			// site-controller, which accumulates from the container above and
			// so is on this entry and not on that one.
			//
			// What keeps a stray label filter from building a site is
			// -e2e.site-stage, not the absence of a label: unset, BeforeAll
			// skips this whole container before any machine is asked for.
			Entry("nico-forge", Label("nico-forge"), nicoForgeTenant()),
		)
	}

	// Tenants first: their machines are Forge instances this site holds, and
	// once the site is gone nothing is left to release them.
	AfterAll(func(ctx SpecContext) {
		if session == nil {
			return
		}
		if keepSite || (lane.cfg.Lifecycle.PreserveOnFailure && failed) {
			GinkgoLogr.Info("keeping the site controller", "name", node.Name, "ip", node.IP,
				"jumpHost", lane.site.JumpHost, "kubeconfig", built.Kubeconfig)
			return
		}
		detached := context.WithoutCancel(ctx)
		// The bootstrap spec collects these itself, and this is a no-op when it
		// did. It is here for everything after it: a tenant that never reaches
		// Ready is a failure of the site's control plane, and the node holding
		// that evidence is released three lines down.
		if failed {
			collectSiteDiagnostics(detached, session, lane.placedSecrets()...)
		}
		if built.Kubeconfig != "" {
			if err := sweepSiteTenants(detached, built.Kubeconfig); err != nil {
				GinkgoLogr.Error(err, "tenant sweep failed; releasing the site anyway, and its tenants' instances may need releasing by hand")
			}
		}
		Expect(session.Teardown(detached)).To(Succeed(), "should release the site controller's node")
	}, NodeTimeout(45*time.Minute))
})

// siteLane is what the lane resolves once, before it creates anything, so a
// missing input fails the run before a machine exists.
type siteLane struct {
	cfg  config.Config
	name string
	env  *environment.Forge
	site environment.ForgeSite
	// capabilities is what the site node is provisioned with, which is a CPU
	// machine of the configured architecture rather than what the run asked
	// to qualify. The run's own capabilities pick the site, not this machine.
	capabilities map[string]string
	apiKey       string
	provider     infra.Provider
	// Generated once for the lane rather than at deploy time, so that
	// everything the run puts on the node is known to placedSecrets and can
	// be taken back out of the diagnostics.
	tenantPassword string
	dbPassword     string
}

// placedSecrets is every credential the run writes onto the node, for
// redacting the diagnostics archive before it is uploaded. Only values worth
// hiding: the tenant login's user name travels with them but is not a
// credential, and redacting it would only make the journal harder to read.
func (l siteLane) placedSecrets() []string {
	return []string{l.apiKey, os.Getenv(envRegistryToken), l.tenantPassword, l.dbPassword}
}

func newSiteLane() (siteLane, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return siteLane{}, err
	}
	env, err := runEnvironment()
	if err != nil {
		return siteLane{}, err
	}
	// Projected here rather than by the suite: this lane is one of the two
	// places that speak Forge, and the suite only knows that -e2e.environment
	// named a file.
	forgeEnv, err := env.Forge()
	if err != nil {
		return siteLane{}, err
	}
	capabilities, err := runCapabilities()
	if err != nil {
		return siteLane{}, err
	}
	// The run's capabilities choose the site, not a name: a stream asks for
	// {gpu: gb200, arch: amd64} and which site owns both such machines is
	// infrastructure. Resolving here also makes capabilities nothing can
	// satisfy fail before a machine exists rather than a minute into
	// provisioning.
	selection, err := forgeEnv.Resolve(capabilities)
	if err != nil {
		return siteLane{}, err
	}
	forgeSite := selection.Site
	// The site node is the run's CPU shape, never its GPU one. The site
	// controller supports the tenants this site will host and runs no
	// workload of its own, so passing the run's capabilities straight through
	// would put it on a GB200. Its architecture is still the run's: Resolve
	// matched this shape on the `arch` capability, so asking for what the
	// shape provides asks for that architecture.
	//
	// The shape's own `provides` rather than a map rebuilt here. The provider
	// matches again when it creates the machine, and handing it the matched
	// shape's description is what makes the second match land on the first
	// one's answer.
	siteCapabilities := selection.CPU.Provides
	apiKey := os.Getenv(envForgeAPIKey)
	if apiKey == "" {
		return siteLane{}, fmt.Errorf("%s is not set", envForgeAPIKey)
	}
	// Per run, never configured: it is the cluster name and the SPIRE trust
	// domain, and two runs sharing one would collide on both.
	suffix, err := randomHex(3)
	if err != nil {
		return siteLane{}, err
	}
	name := "nke-e2e-site-" + suffix
	tenantPassword, err := randomHex(16)
	if err != nil {
		return siteLane{}, err
	}
	dbPassword, err := randomHex(16)
	if err != nil {
		return siteLane{}, err
	}

	kubernetesVersion := cmp.Or(optionalVersion("kube"), forgeDefaultKubernetesVersion)
	dialer := &teleport.Dialer{
		Login:        forgeSite.TeleportLogin,
		IdentityFile: cfg.Runner.TeleportIdentityFile,
	}
	// Teleport is the route; the password is the interim one, for a site
	// whose jump host is not enrolled yet. Selected by the secret being
	// present, so enrolling the jump host later means dropping
	// JUMP_HOST_PASSWORD rather than editing anything. The machines beyond it
	// are reached with the run's own key either way.
	if password := os.Getenv(envJumpHostPassword); password != "" {
		if jerr := dialer.JumpOverSSH(teleport.SSHJumpHost{
			User:               cmp.Or(forgeSite.JumpHostUser, forgeSite.TeleportLogin),
			Password:           password,
			HostKeyFingerprint: forgeSite.JumpHostKeyFingerprint,
		}); jerr != nil {
			return siteLane{}, jerr
		}
	}
	provider, err := forge.New(forge.Options{
		Env:      forgeEnv,
		Site:     forgeSite.Key,
		APIKey:   apiKey,
		JumpHost: forgeSite.JumpHost,
		NodeUser: cfg.Node.User,
		// The site node boots the OS image built for its own architecture,
		// which is the same image a tenant control plane of that architecture
		// boots. A GPU tenant's workers boot a different one.
		Images: func(_, arch string) (string, string, error) {
			return requireNodeArtifacts(arch)
		},
		Dialer: forge.Teleport(dialer),
		Node:   nodeInputs(kubernetesVersion),
		Tools: forge.HostTools{
			NerdctlVersion:    cmp.Or(optionalVersion("nerdctl"), siteDefaultNerdctlVersion),
			KindVersion:       cmp.Or(optionalVersion("kind"), siteDefaultKindVersion),
			ClusterctlVersion: cmp.Or(optionalVersion("clusterctl"), siteDefaultClusterctlVersion),
		},
		DisableMTLS: siteDisableMTLS,
	})
	if err != nil {
		return siteLane{}, err
	}
	return siteLane{
		cfg: cfg, name: name, env: forgeEnv, site: forgeSite,
		capabilities: siteCapabilities, apiKey: apiKey, provider: provider,
		tenantPassword: tenantPassword, dbPassword: dbPassword,
	}, nil
}

// sessionRelay tunnels a tenant's API server through the site node, which is
// the only machine in the run that can reach it.
//
// One tunnel per address, kept for the life of the session. Every tenant
// client asks for a REST config of its own -- twenty-odd specs do, some of
// them repeatedly inside an Eventually -- and a tunnel each would leak
// listeners for the hour the lane runs.
func sessionRelay(session infra.Session) kube.EndpointRelay {
	var mu sync.Mutex
	forwards := map[string]string{}
	return func(ctx context.Context, hostPort string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if local, ok := forwards[hostPort]; ok {
			return local, nil
		}
		local, err := session.Forward(ctx, hostPort)
		if err != nil {
			return "", fmt.Errorf("forward the tenant API server at %s through the site node: %w", hostPort, err)
		}
		GinkgoLogr.Info("relaying the tenant API server", "published", hostPort, "local", local)
		forwards[hostPort] = local
		return local, nil
	}
}

// siteDiagnostics is written next to site-e2e.log, in the directory the
// suite runs from, so CI picks it up with the rest of the run's record.
const siteDiagnostics = "site-diagnostics.tar.gz"

// siteDiagnosticsCollected is set once this run has written siteDiagnostics.
var siteDiagnosticsCollected bool

// collectSiteDiagnostics pulls the node's diagnostics off before teardown
// releases it. Best effort and never fails the spec: the failure that
// prompted it is the finding, and losing the diagnostics must not replace it
// with a second, less useful one.
//
// Callable twice -- the bootstrap spec and the teardown both reach it -- and
// the first archive wins, since it was written closest to the failure.
//
// secrets is what the run wrote onto the node, taken back out of the archive
// before it is saved: the workflow uploads it as an artifact, so it outlives
// the node and the credentials' scope.
func collectSiteDiagnostics(ctx context.Context, session infra.Session, secrets ...string) {
	if siteDiagnosticsCollected {
		return
	}
	// Detached, because a spec that failed on its own timeout has a context
	// that is already done, which is exactly when the diagnostics matter.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	switch err := site.CollectDiagnostics(ctx, session, siteDiagnostics, secrets...); {
	case errors.Is(err, site.ErrNoDiagnostics):
		GinkgoLogr.Info("the node wrote no diagnostics, so the failure is after the deploy")
		return
	case err != nil:
		GinkgoLogr.Error(err, "could not collect the node's diagnostics")
		return
	}
	siteDiagnosticsCollected = true
	GinkgoLogr.Info("collected the node's diagnostics", "path", siteDiagnostics)
}

// nodeInputs is what cloud-init writes onto the node. The agent's arguments
// are decided here because they have to agree with the bootstrap's
// --disable-mtls, which comes from the same constant: the two sides
// negotiate with each other, and a mismatch surfaces as an opaque TLS
// handshake error rather than as a configuration problem.
func nodeInputs(kubernetesVersion string) nke.NodeInputs {
	inputs := nke.NodeInputs{
		KubernetesVersion: kubernetesVersion,
		CNIVersion:        cmp.Or(optionalVersion("nke-cni-ovn"), forgeDefaultCNIVersion),
		SpireVersion:      cmp.Or(optionalVersion("spire-agent"), forgeDefaultSpireVersion),
	}
	if siteDisableMTLS {
		inputs.AgentArgs = nke.AgentArgsNoMTLS
	}
	return inputs
}

// candidate is what the site is built from, resolved once the node exists.
// Only the full stage deploys the stack; the bootstrap stage checks the API
// server instead of CWE.
func (l siteLane) candidate(node infra.Node) (site.Candidate, site.Options, error) {
	top, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return site.Candidate{}, site.Options{}, fmt.Errorf("find the clone to export the deploy path from: %w", err)
	}
	repo := strings.TrimSpace(string(top))
	nkectl := cmp.Or(l.cfg.Site.Nkectl, filepath.Join("cli", "dist", "nkectl-linux-"+node.Arch))
	version, err := requireVersion("controller")
	if err != nil {
		return site.Candidate{}, site.Options{}, err
	}
	images := l.cfg.Site.Images
	controller := site.Image{Repository: images.Controller.Repository, Tag: version}
	provisioned := site.Image{
		Repository: images.CAPIProvisioned.Repository,
		Tag:        cmp.Or(optionalVersion("capi-provisioned"), siteDefaultCAPIProvisionedVersion),
	}

	ref, err := deployPathRef(repo, l.cfg.Site.DeployPathRef, version)
	if err != nil {
		return site.Candidate{}, site.Options{}, err
	}
	assets, err := site.ExportRef(context.Background(), repo, ref, site.DeployAssetPaths())
	if err != nil {
		return site.Candidate{}, site.Options{}, err
	}
	DeferCleanup(func() { _ = os.RemoveAll(assets) })

	c := site.Candidate{
		SiteName:   l.name,
		Nkectl:     inRepo(repo, nkectl),
		Assets:     assets,
		AssetPaths: site.DeployAssetPaths(),
		Images: []string{
			inRepo(repo, images.Controller.Archive),
			inRepo(repo, images.CAPIProvisioned.Archive),
			inRepo(repo, images.Profiles.Archive),
		},
		KubernetesVersion: cmp.Or(optionalVersion("kube"), forgeDefaultKubernetesVersion),
		CNI:               siteCNI,
		DisableMTLS:       siteDisableMTLS,
		// The references the archives carry, so the bootstrap's kind cluster
		// resolves them from containerd rather than a registry.
		BootstrapEnv: map[string]string{
			"NKECTL_CWE_IMAGE":              controller.Ref(),
			"NKECTL_CAPI_PROVISIONED_IMAGE": provisioned.Ref(),
		},
	}
	if siteStage != siteStageFull {
		c.Deploy = noDeploy{}
		return c, site.Options{Probe: probeAPIServer}, nil
	}

	deployer, err := l.sbom(node, controller)
	if err != nil {
		return site.Candidate{}, site.Options{}, err
	}
	c.Deploy, c.ControllerVersion = deployer, version
	return c, site.Options{}, nil
}

// deployPathRef is the commit the deploy path is exported from. It carries
// the controller's own chart and overlay, so unless configured it follows the
// controller: the commit itself when the controller was built from one, else
// that version's release tag.
func deployPathRef(repo, configured, controllerVersion string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	candidates := []string{controllerVersion, "controller/" + controllerVersion}
	for _, ref := range candidates {
		if exec.CommandContext(context.Background(), "git", "-C", repo, "cat-file", "-e", ref+"^{commit}").Run() == nil {
			return ref, nil
		}
	}
	return "", fmt.Errorf("the deploy path follows the controller version, and neither %s resolves in %s; "+
		"fetch the release tag or set site.deployPathRef", strings.Join(candidates, " nor "), repo)
}

func (l siteLane) sbom(node infra.Node, controller site.Image) (site.SBOM, error) {
	images := l.cfg.Site.Images
	profiles := site.Image{Repository: images.Profiles.Repository, Tag: images.Profiles.Tag}
	user, token := os.Getenv(envRegistryUser), os.Getenv(envRegistryToken)
	if user == "" || token == "" {
		return site.SBOM{}, fmt.Errorf("%s and %s are required for the node to pull CAPNICo from %s", envRegistryUser, envRegistryToken, siteCAPNICoRegistry)
	}
	unameArch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[node.Arch]
	GinkgoLogr.Info("deploying the stack", "node", node.Name, "controller", controller.Ref(), "profiles", profiles.Ref())
	return site.SBOM{
		Controller: controller,
		Profiles:   profiles,
		Nico: site.Nico{
			Endpoint: forge.DefaultEndpoint,
			OrgID:    l.env.Org.ID,
			Token:    l.apiKey,
		},
		Registry: site.Registry{Server: siteCAPNICoRegistry, Username: user, Token: token},
		// The login baked into tenant nodes' user-data; nothing outside the
		// run needs it.
		Tenant: site.Tenant{User: "nke-tenant", Password: l.tenantPassword},
		// Set here rather than left for the deployer to generate, so the
		// value is one placedSecrets can name.
		DatabasePassword: l.dbPassword,
		Arch:             unameArch,
	}, nil
}

// noDeploy is the bootstrap stage's deployer: Kubernetes only.
type noDeploy struct{}

func (noDeploy) Deploy(context.Context, infra.Session, site.DeployTarget) error { return nil }

// probeAPIServer stands in for the CWE probe when nothing is deployed: the
// forwarded kubeconfig has to reach the API server.
func probeAPIServer(_ context.Context, kubeconfig string) (string, error) {
	restConfig, err := kube.RESTConfig(kubeconfig, "")
	if err != nil {
		return "", err
	}
	client, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return "", err
	}
	if _, err := client.ServerVersion(); err != nil {
		return "", err
	}
	return "", nil
}

// Budgets for the teardown sweep, sized against the AfterAll's 45-minute
// NodeTimeout that contains it. They were 40 and 30 minutes, which left the
// site release five minutes of that node -- so a slow sweep starved the one
// step that must not be skipped. Releasing the site is what stops its Forge
// instance leaking, and a leaked site is worse than a leaked tenant: it is the
// larger instance, and its CWE is the only thing that can still delete the
// tenants. Waiting at all is still right, because releasing the site while a
// tenant delete is in flight strands that tenant's instances with nothing left
// to release them.
const (
	sweepBudget       = 20 * time.Minute // whole sweep, all tenants
	sweepDeleteBudget = 15 * time.Minute // any one tenant within it
)

func sweepSiteTenants(ctx context.Context, kubeconfig string) error {
	restConfig, err := kube.RESTConfig(kubeconfig, "")
	if err != nil {
		return err
	}
	client, err := nke.NewClient(ctx, restConfig, nke.Options{Delete: sweepDeleteBudget, Poll: 30 * time.Second})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sweepBudget)
	defer cancel()
	return site.SweepTenants(ctx, client)
}

func inRepo(repo, path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(repo, path)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func TestDeployPathRefFollowsTheController(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(context.Background(), "git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("-c", "user.name=e2e", "-c", "user.email=e2e@example.com", "commit", "-q", "--allow-empty", "-m", "first")
	git("tag", "controller/v0.1.1-rc.8")
	commit := git("rev-parse", "--short=12", "HEAD")

	for _, tc := range []struct{ name, configured, version, want string }{
		{"built from a commit", "", commit, commit},
		{"published version", "", "v0.1.1-rc.8", "controller/v0.1.1-rc.8"},
		{"configured wins", "abc", "v0.1.1-rc.8", "abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deployPathRef(repo, tc.configured, tc.version)
			if err != nil || got != tc.want {
				t.Errorf("deployPathRef = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := deployPathRef(repo, "", "v9.9.9"); err == nil || !strings.Contains(err.Error(), "controller/v9.9.9") {
		t.Errorf("an unresolvable version should fail naming its tag, got %v", err)
	}
}

func expectSucceed2[A, B any](a A, b B, err error, messages ...any) (A, B) {
	GinkgoHelper()
	Expect(err).NotTo(HaveOccurred(), messages...)
	return a, b
}
