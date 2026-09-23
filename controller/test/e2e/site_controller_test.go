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
)

// Host tools the node installs for nkectl, pinned to what has built a site on
// az61. Each can be overridden through -e2e.versions.
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
			Expect(err).NotTo(HaveOccurred(), "should build the site controller")
			GinkgoLogr.Info("site controller", "kubeconfig", built.Kubeconfig, "cweVersion", built.Version)
		}, NodeTimeout(3*time.Hour))
	}

	if siteStage == siteStageFull {
		DescribeTableSubtree("tenant on the built site", tenantSubtree(func() (managementCluster, error) {
			if built.Kubeconfig == "" {
				return managementCluster{}, errors.New("the site controller was not built")
			}
			return managementCluster{Kubeconfig: built.Kubeconfig, TrustDomain: site.TrustDomain(lane.name)}, nil
		}),
			// Not labeled nico-forge: that label selects the lane against an
			// existing site, which must not start building one.
			Entry("nico-forge", nicoForgeTenant),
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

	kubernetesVersion := defaultString(optionalVersion("kube"), forgeDefaultKubernetesVersion)
	dialer := &teleport.Dialer{
		Login:        cfg.Site.TeleportLogin,
		IdentityFile: cfg.Site.TeleportIdentityFile,
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
		Node: nke.ForgeNodeInputs{
			KubernetesVersion: kubernetesVersion,
			CNIVersion:        defaultString(optionalVersion("nke-cni-ovn"), forgeDefaultCNIVersion),
			SpireVersion:      defaultString(optionalVersion("spire-agent"), forgeDefaultSpireVersion),
		},
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
	}, nil
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
	tenantPassword, err := randomHex(16)
	if err != nil {
		return site.SBOM{}, "", err
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
		Tenant: site.Tenant{User: "nke-tenant", Password: tenantPassword},
		Arch:   unameArch,
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

func sweepSiteTenants(ctx context.Context, kubeconfig string) error {
	restConfig, err := kube.RESTConfig(kubeconfig, "")
	if err != nil {
		return err
	}
	client, err := nke.NewClient(ctx, restConfig, nke.Options{Delete: 30 * time.Minute, Poll: 30 * time.Second})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Minute)
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
