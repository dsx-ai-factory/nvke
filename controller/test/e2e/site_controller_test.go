// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
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
// the Forge hardware. Each can be overridden through -e2e.versions.
const (
	siteDefaultNerdctlVersion    = "2.3.5"
	siteDefaultKindVersion       = "v0.30.0"
	siteDefaultClusterctlVersion = "v1.12.3"
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
				"jumpHost", lane.cfg.Site.JumpHost, "kubeconfig", built.Kubeconfig)
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
	cfg          config.Config
	name         string
	env          *environment.Forge
	site         environment.ForgeSite
	instanceType environment.ForgeInstanceType
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
	env, err := forgeEnvironment()
	if err != nil {
		return siteLane{}, err
	}
	forgeSite, err := env.Site("")
	if err != nil {
		return siteLane{}, err
	}
	capabilities, err := runCapabilities()
	if err != nil {
		return siteLane{}, err
	}
	instanceType, err := forgeSite.MatchInstanceType(capabilities)
	if err != nil {
		return siteLane{}, err
	}
	apiKey := os.Getenv(envForgeAPIKey)
	if apiKey == "" {
		return siteLane{}, fmt.Errorf("%s is not set", envForgeAPIKey)
	}
	name := cfg.Site.Name
	if name == "" {
		suffix, rerr := randomHex(3)
		if rerr != nil {
			return siteLane{}, rerr
		}
		name = "nke-e2e-site-" + suffix
	}
	tenantPassword, err := randomHex(16)
	if err != nil {
		return siteLane{}, err
	}
	dbPassword, err := randomHex(16)
	if err != nil {
		return siteLane{}, err
	}

	kubernetesVersion := defaultString(optionalVersion("kube"), forgeDefaultKubernetesVersion)
	dialer := &teleport.Dialer{
		Login:        cfg.Site.TeleportLogin,
		IdentityFile: cfg.Site.TeleportIdentityFile,
	}
	// Teleport is the route; the password is the interim one, for a site
	// whose jump host is not enrolled yet. Selected by the secret being
	// present, so enrolling the jump host later means dropping
	// JUMP_HOST_PASSWORD rather than editing anything. The machines beyond it
	// are reached with the run's own key either way.
	if password := os.Getenv(envJumpHostPassword); password != "" {
		if jerr := dialer.JumpOverSSH(teleport.SSHJumpHost{
			User:               defaultString(cfg.Site.JumpHostUser, cfg.Site.TeleportLogin),
			Password:           password,
			HostKeyFingerprint: cfg.Site.JumpHostKeyFingerprint,
		}); jerr != nil {
			return siteLane{}, jerr
		}
	}
	provider, err := forge.New(forge.Options{
		Env:      env,
		Site:     forgeSite.Key,
		APIKey:   apiKey,
		JumpHost: cfg.Site.JumpHost,
		// The site node boots the same OS image as the tenants it will host.
		Images: func(string) (string, string, error) {
			bootLoaderURL, berr := requireVersion("os-image-bootloader")
			if berr != nil {
				return "", "", berr
			}
			imageURL, ierr := requireVersion("os-image")
			return bootLoaderURL, imageURL, ierr
		},
		Dialer: forge.Teleport(dialer),
		Node:   nodeInputs(cfg, kubernetesVersion),
		Tools: forge.HostTools{
			NerdctlVersion:    defaultString(optionalVersion("nerdctl"), siteDefaultNerdctlVersion),
			KindVersion:       defaultString(optionalVersion("kind"), siteDefaultKindVersion),
			ClusterctlVersion: defaultString(optionalVersion("clusterctl"), siteDefaultClusterctlVersion),
		},
		DisableMTLS: cfg.Site.DisableMTLS,
	})
	if err != nil {
		return siteLane{}, err
	}
	return siteLane{
		cfg: cfg, name: name, env: env, site: forgeSite, instanceType: instanceType,
		capabilities: capabilities, apiKey: apiKey, provider: provider,
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
	if _, err := os.Stat(siteDiagnostics); err == nil {
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
	GinkgoLogr.Info("collected the node's diagnostics", "path", siteDiagnostics)
}

// nodeInputs is what cloud-init writes onto the node. The agent's arguments
// are decided here because they have to agree with the bootstrap's
// --disable-mtls, which comes from the same config field: the two sides
// negotiate with each other, and a mismatch surfaces as an opaque TLS
// handshake error rather than as a configuration problem.
func nodeInputs(cfg config.Config, kubernetesVersion string) nke.ForgeNodeInputs {
	inputs := nke.ForgeNodeInputs{
		KubernetesVersion: kubernetesVersion,
		CNIVersion:        defaultString(optionalVersion("nke-cni-ovn"), forgeDefaultCNIVersion),
		SpireVersion:      defaultString(optionalVersion("spire-agent"), forgeDefaultSpireVersion),
	}
	if cfg.Site.DisableMTLS {
		inputs.AgentArgs = nke.AgentArgsNoMTLS
	}
	return inputs
}

// candidate is what the site is built from, resolved once the node exists.
// Only the full stage deploys the stack; the bootstrap stage checks the API
// server instead of CWE.
func (l siteLane) candidate(node infra.Node) (site.Candidate, site.Options, error) {
	repo := l.cfg.Site.Repo
	if repo == "" {
		top, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return site.Candidate{}, site.Options{}, fmt.Errorf("find the clone to export the deploy path from: %w", err)
		}
		repo = strings.TrimSpace(string(top))
	}
	arch := l.instanceType.Provides["arch"]
	nkectl := defaultString(l.cfg.Site.Nkectl, filepath.Join("cli", "dist", "nkectl-linux-"+arch))
	images := make([]string, 0, len(l.cfg.Site.Images))
	for _, image := range l.cfg.Site.Images {
		images = append(images, inRepo(repo, image))
	}

	ref := defaultString(l.cfg.Site.DeployPathRef, site.DeployPathRef)
	assets, err := site.ExportRef(context.Background(), repo, ref, site.DeployAssetPaths())
	if err != nil {
		return site.Candidate{}, site.Options{}, err
	}
	DeferCleanup(func() { _ = os.RemoveAll(assets) })

	c := site.Candidate{
		SiteName:          l.name,
		Nkectl:            inRepo(repo, nkectl),
		Assets:            assets,
		AssetPaths:        site.DeployAssetPaths(),
		Images:            images,
		KubernetesVersion: defaultString(optionalVersion("kube"), forgeDefaultKubernetesVersion),
		CNI:               l.cfg.Site.CNI,
		DisableMTLS:       l.cfg.Site.DisableMTLS,
		BootstrapEnv:      l.cfg.Site.BootstrapEnv,
	}
	if siteStage != siteStageFull {
		c.Deploy = noDeploy{}
		return c, site.Options{Probe: probeAPIServer}, nil
	}

	deployer, version, err := l.sbom(node, arch)
	if err != nil {
		return site.Candidate{}, site.Options{}, err
	}
	c.Deploy, c.ControllerVersion = deployer, version
	return c, site.Options{}, nil
}

func (l siteLane) sbom(node infra.Node, arch string) (site.SBOM, string, error) {
	version, err := requireVersion("controller")
	if err != nil {
		return site.SBOM{}, "", err
	}
	profiles, err := splitImage(l.cfg.Site.ProfilesImage)
	if err != nil {
		return site.SBOM{}, "", fmt.Errorf("site.profilesImage: %w", err)
	}
	user, token := os.Getenv(envRegistryUser), os.Getenv(envRegistryToken)
	if user == "" || token == "" {
		return site.SBOM{}, "", fmt.Errorf("%s and %s are required to pull the private images", envRegistryUser, envRegistryToken)
	}
	unameArch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[arch]
	GinkgoLogr.Info("deploying the stack", "node", node.Name, "controller", version, "profiles", l.cfg.Site.ProfilesImage)
	return site.SBOM{
		Controller: site.Image{Repository: l.cfg.Site.ControllerRepository, Tag: version},
		Profiles:   profiles,
		Nico: site.Nico{
			Endpoint: forge.DefaultEndpoint,
			OrgID:    l.env.Org.ID,
			Token:    l.apiKey,
		},
		Registry: site.Registry{Server: l.cfg.Site.RegistryServer, Username: user, Token: token},
		// The login baked into tenant nodes' user-data; nothing outside the
		// run needs it.
		Tenant: site.Tenant{User: "nke-tenant", Password: l.tenantPassword},
		// Set here rather than left for the deployer to generate, so the
		// value is one placedSecrets can name.
		DatabasePassword: l.dbPassword,
		Arch:             unameArch,
	}, version, nil
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

func splitImage(ref string) (site.Image, error) {
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if ref == "" || colon <= slash {
		return site.Image{}, fmt.Errorf("%q is not repository:tag", ref)
	}
	return site.Image{Repository: ref[:colon], Tag: ref[colon+1:]}, nil
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

func expectSucceed2[A, B any](a A, b B, err error, messages ...any) (A, B) {
	GinkgoHelper()
	Expect(err).NotTo(HaveOccurred(), messages...)
	return a, b
}
