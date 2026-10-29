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

	openapi_types "github.com/oapi-codegen/runtime/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/environment"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/dsx"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/forge"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/provision"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/teleport"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nktp"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/site"
)

// The stages of the site controller run, each doing everything the one before it did.
const (
	siteStageProvision = "provision" // a node, reachable, then released
	siteStageBootstrap = "bootstrap" // plus vanilla Kubernetes from nvkectl
	siteStageFull      = "full"      // plus the NVKE stack, the controller, and a tenant
)

// Secrets the site controller run reads from the environment. They are never written to
// the config file, a values file on the runner, or a command line.
const (
	envForgeAPIKey = "FORGE_API_KEY"
	// envJumpHostPassword selects the plain-SSH route to the jump host.
	// Unset uses Teleport.
	envJumpHostPassword = "JUMP_HOST_PASSWORD"
)

// withheldFromEnvironment is every value in the run's environment that must
// not reach an artifact: the jump hosts and the SSA issuer, which the
// workflow's own step seds out of the text files it uploads, and the jump host
// password when a run uses one. The archives the suite writes cannot be
// sed-ed, so the suite carries the same list into its redaction. Each value
// raw and without a trailing slash, as the workflow matches it.
func withheldFromEnvironment() []string {
	var values []string
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || value == "" {
			continue
		}
		if !strings.HasSuffix(name, "_JUMP_HOST") && name != "SSA_ISSUER" && name != envJumpHostPassword {
			continue
		}
		values = append(values, value)
		if trimmed := strings.TrimRight(value, "/"); trimmed != value {
			values = append(values, trimmed)
		}
	}
	return values
}

// The versions a site run is given in -e2e.versions, all required. A run that
// names none of a version has not said what it qualifies, so the suite fails
// before a machine exists instead of falling back to a constant the record
// cannot see.
const (
	versionController      = "controller"
	versionCAPIProvisioned = "capi-provisioned"
	versionKube            = "kube"
	versionCNIOVN          = "cni-ovn"
	versionKind            = "kind"
	versionClusterctl      = "clusterctl"
	versionNerdctl         = "nerdctl"
)

// The shape of the site this run bootstraps.
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
)

var (
	siteStage string
	keepSite  bool
)

func init() {
	flag.StringVar(&siteStage, "e2e.site-stage", "",
		"build a site controller up to this stage: provision, bootstrap or full (empty skips the site controller)")
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

// The container is serial: one site, built once, holding the tenant specs that run
// against it. ContinueOnFailure lets teardown and the tenant sweep run after a
// failure; a failed build still skips the tenants, which check for a site.
//
// The needs: labels are every machine this container holds at once, which is
// what the Capacity Provider checks for before a run starts: the site node and
// the tenant's control plane on CPU shapes, and the tenant's worker on the GPU
// shape every stream in release/policy.yaml names. On the container rather
// than the tenant entry because the site outlives every spec beneath it, and
// the provider takes the maximum per machine type, not the sum.
var _ = Describe("Site controller", Ordered, ContinueOnFailure, Label("site-controller", "needs:cpu=2", "needs:gpu=1"), func() {
	var (
		cfg config.Config
		// provider creates the site node. Settled before anything is created,
		// so an environment no site can satisfy fails without taking a machine.
		provider infra.Provider
		// capabilities is what the site node is created with: the run's own,
		// and the source of the architecture every artifact is chosen by, since
		// siteCapabilities defaults the key so it is never empty.
		// less the GPU. The site controller supports the tenants this site
		// will host and runs no workload of its own, so asking for the run's
		// accelerator would put it on one. Nothing insists on a CPU machine
		// either, so a site offering only GPU shapes can still host one.
		capabilities map[string]string
		// secrets is every value to redact from the diagnostics archives:
		// what the environment withholds from artifacts at every stage, and
		// from the full stage on, every credential the deploy places on the
		// node. Bootstrap itself stages a binary, an archive, images and a
		// host list, and none of them is a credential.
		//
		// The deploy's are named by newStack, which reads or generates every
		// one of them.
		secrets []string
		// spec is the site to build, resolved in BeforeAll so a missing input
		// fails the run before a machine exists.
		spec site.Spec
		// stack is what gets installed onto it, resolved by the stage that
		// installs it. It carries the credentials the deploy places, so it is
		// held rather than local: teardown redacts them from the diagnostics,
		// and a zero one before the deploy names nothing, which is right
		// because nothing has been placed yet.
		stack   site.Stack
		node    infra.Node
		session infra.Session
		cluster site.Cluster
		failed  bool
		// deployed records that the install returned without error. The
		// controller tag is set before Deploy runs, so it cannot stand in for
		// this: after a failed install it is non-empty and the controller probe would
		// poll its full timeout against a stack that was never installed.
		deployed bool
	)

	BeforeAll(func() {
		switch siteStage {
		case "":
			Skip("-e2e.site-stage is not set")
		case siteStageProvision, siteStageBootstrap:
		case siteStageFull:
			if err := fullStageSupported(expectSucceed(runEnvironment()).Provider); err != nil {
				Fail(err.Error())
			}
		default:
			Fail(fmt.Sprintf("-e2e.site-stage %q is not provision, bootstrap or full", siteStage))
		}
		// An archive an earlier run left behind describes another node.
		if err := os.Remove(siteDiagnostics); err != nil && !errors.Is(err, os.ErrNotExist) {
			Fail(fmt.Sprintf("removing a stale %s: %v", siteDiagnostics, err))
		}
		removeStaleBundles()
		cfg = expectSucceed(config.Load(cfgPath))
		provider = expectSucceed(newInfraProvider(cfg))
		capabilities = siteCapabilities(expectSucceed(runCapabilities()))
		name := expectSucceed(newSiteName())
		spec = expectSucceed(siteSpec(cfg, name))
		// Every full-stage input check lives in newStack: the deploy path's
		// controller name, the tag it is exported from, the Forge API key and
		// the registry credentials. Resolved here so a misconfigured run fails
		// before a machine is provisioned rather than twenty minutes into one.
		//
		// Capacity is the same kind of check. CAPNICo will not create an
		// instance while unusedUsable is zero, and the tenant wait then spends
		// its whole provision timeout reporting that. The site node has not
		// been asked for yet, so a shortfall here releases nothing.
		secrets = withheldFromEnvironment()
		if siteStage == siteStageFull {
			var placed []string
			stack, placed = expectSucceed2(newStack(cfg, capabilities[infra.CapabilityArch]))
			secrets = append(secrets, placed...)
			Expect(requireRunCapacity(context.Background(), provider)).To(Succeed())
		}
	})

	AfterEach(func() {
		bundled := bundledThisSpec
		bundledThisSpec = false
		if !CurrentSpecReport().Failed() {
			return
		}
		failed = true
		// The spec that collected the bundle is already in it. A failure in
		// a later spec is what makes a second one, from the teardown, worth
		// its minutes.
		if siteBundleCollected && !bundled {
			failedSinceBundle = true
		}
	})

	It("provisions a node", func(ctx SpecContext) {
		var err error
		node, session, err = provider.ProvisionNode(ctx, infra.NodeRequest{
			Name:         spec.Name,
			Capabilities: capabilities,
			Labels:       map[string]string{"purpose": "nke-e2e-site-controller"},
		})
		Expect(err).NotTo(HaveOccurred(), "should provision and reach a node")
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

	// The stage gates by calling or not calling, and nothing else. A stage
	// that stops earlier runs fewer specs rather than running the same spec
	// with a collaborator neutered, which is what a do-nothing deployer and a
	// substitute probe used to express.
	if siteStage == siteStageBootstrap || siteStage == siteStageFull {
		It("bootstraps the site controller", func(ctx SpecContext) {
			if session == nil {
				Skip("no node")
			}
			nvkectl := expectSucceed(nvkectlBinary(cfg, capabilities[infra.CapabilityArch]))
			// From before the bootstrap, because the question a timeline
			// answers -- which network a pod landed on, and when -- is
			// decided while kubeadm and the CNI come up.
			startSiteSampler(ctx, session)
			var err error
			cluster, err = site.Bootstrap(ctx, node, session, nvkectl, spec)
			if err != nil {
				collectSiteDiagnostics(ctx, session, secrets...)
				collectSiteClusterState(ctx, session, secrets...)
				collectSiteBundle(ctx, session, siteBundle, siteBundleBudget, secrets...)
			}
			Expect(err).NotTo(HaveOccurred(), "should bootstrap Kubernetes on the node")
			GinkgoLogr.Info("site controller", "kubeconfig", cluster.Kubeconfig)
		}, NodeTimeout(3*time.Hour))

		// Asserted in both stages, which the old shape could not do: the
		// substitute probe only ran on the path that skipped the deploy, so
		// the full stage never checked the API server was up before
		// installing onto it.
		It("reaches its API server", func(_ SpecContext) {
			if cluster.Kubeconfig == "" {
				Skip("the site controller was not bootstrapped")
			}
			Expect(apiServerReachable(cluster.Kubeconfig)).To(Succeed(),
				"the forwarded kubeconfig should reach the API server")
		}, NodeTimeout(5*time.Minute))
	}

	if siteStage == siteStageFull {
		It("deploys the stack", func(ctx SpecContext) {
			if cluster.Kubeconfig == "" || session == nil {
				Skip("the site controller was not bootstrapped")
			}
			err := stack.Deploy(ctx, session, site.DeployTarget{
				Node:        node,
				SiteName:    spec.Name,
				TrustDomain: site.TrustDomain(spec.Name),
				Cluster:     cluster,
			})
			if err != nil {
				collectSiteDiagnostics(ctx, session, secrets...)
				collectSiteClusterState(ctx, session, secrets...)
				collectSiteBundle(ctx, session, siteBundle, siteBundleBudget, secrets...)
			}
			Expect(err).NotTo(HaveOccurred(), "should install the NVKE stack")
			deployed = true
		}, NodeTimeout(1*time.Hour))

		It("reports its controller version", func(ctx SpecContext) {
			// ContinueOnFailure reaches this spec after a failed bootstrap or
			// a failed install. Probing either would poll the whole readiness
			// timeout against a cluster with no controller on it, and teardown needs
			// those minutes to release the node.
			if !deployed {
				Skip("the stack was not deployed")
			}
			version, err := site.ProbeController(ctx, cluster.Kubeconfig, 0, 0)
			Expect(err).NotTo(HaveOccurred(), "the controller should become healthy")
			GinkgoLogr.Info("site controller", "controllerVersion", version)
			Expect(version).To(Equal(stack.Controller.Tag),
				"the controller should report the version the run deployed")
		}, NodeTimeout(15*time.Minute))

		validateSitePostDeploy(func() (site.Cluster, site.Spec, bool) {
			return cluster, spec, deployed
		}, siteLaneChecks)
		// The validation image's behavior checks, on the site this run built.
		// They skip after any failure, as the checks above do, so a broken
		// site keeps the minutes its teardown needs.
		validateSiteBehavior(func() siteValidateSettings {
			if !deployed {
				Skip("the stack was not deployed")
			}
			if failed {
				Skip("an earlier site check failed")
			}
			checks := siteLaneChecks
			checks.restConfig = func() (*rest.Config, error) { return kube.RESTConfig(cluster.Kubeconfig, "") }
			return siteValidateSettings{enabled: true, kubernetesVersion: spec.KubernetesVersion, checks: checks}
		})

		It("delivers the site's telemetry through NKTP", func(ctx SpecContext) {
			if !deployed {
				Skip("the stack was not deployed")
			}
			restConfig := expectSucceed(kube.RESTConfig(cluster.Kubeconfig, ""))
			// This spec's own request, so a trace the install left cannot pass.
			traceID, err := site.ControllerTraceID(ctx, cluster.Kubeconfig)
			Expect(err).NotTo(HaveOccurred(), "the controller should trace a request")
			v, err := nktp.New(ctx, restConfig, spec.Name, GinkgoLogr)
			Expect(err).NotTo(HaveOccurred(), "the NKTP backend should be reachable")
			DeferCleanup(v.Close)
			checks := siteTelemetry(v, traceID)
			// Signals get eight minutes to show up. The "nothing bad" checks then
			// get three to hold, so failures from a rollout age out of their
			// window, and must keep holding for one more.
			Eventually(v.Missing).WithContext(ctx).WithArguments(checks).WithTimeout(8*time.Minute).WithPolling(10*time.Second).
				Should(BeEmpty(), "every signal should arrive")
			Eventually(v.Unwanted).WithContext(ctx).WithArguments(checks).WithTimeout(3*time.Minute).WithPolling(10*time.Second).
				Should(BeEmpty(), "nothing NKTP must not deliver should be arriving")
			Consistently(v.Unwanted).WithContext(ctx).WithArguments(checks).WithTimeout(time.Minute).WithPolling(10*time.Second).
				Should(BeEmpty(), "nothing NKTP must not deliver should start arriving")
		}, NodeTimeout(20*time.Minute))
	}

	if siteStage == siteStageFull {
		DescribeTableSubtree("tenant on the built site", tenantSubtree(func() (managementCluster, error) {
			if cluster.Kubeconfig == "" || session == nil {
				return managementCluster{}, errors.New("the site controller was not built")
			}
			waiter, _ := provider.(terminationWaiter)
			return managementCluster{
				Kubeconfig:  cluster.Kubeconfig,
				TrustDomain: site.TrustDomain(spec.Name),
				Relay:       sessionRelay(session),
				Instances:   waiter,
			}, nil
		}),
			// Labeled nico-forge because it is: the same tenant the
			// standalone entry creates, on a site this run built rather than
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
		// The kubeconfig is an admin credential in a temporary directory
		// nothing else removes. Deferred so it goes on every path out of this
		// function, and after the sweep and teardown below, which read it.
		//
		// Not DeferCleanup inside a spec: Ginkgo runs that when the spec ends,
		// not when this container does, so the specs after the bootstrap would
		// read a deleted file.
		defer func() {
			if cluster.Kubeconfig != "" {
				_ = os.RemoveAll(filepath.Dir(cluster.Kubeconfig))
			}
		}()
		if session == nil {
			return
		}
		detached := context.WithoutCancel(ctx)
		// On every path, a kept site included: the timeline it wrote is read
		// by the bundle below, and nothing should keep sampling a node that
		// is left for a person to look at.
		stopSiteSampler(detached, session)
		// The failing spec collects these itself, and the first two are a
		// no-op when it did. They are here for everything after it: a tenant
		// that never reaches Ready is a failure of the site's control plane,
		// and the node holding that evidence is released below. Before the
		// keep check, not after: a kept node is kept to be read, and the
		// bundle is the record of what it looked like when the run failed.
		if failed {
			collectSiteDiagnostics(detached, session, secrets...)
			collectSiteClusterState(detached, session, secrets...)
			if !siteBundleCollected || failedSinceBundle {
				collectSiteBundle(detached, session, siteBundleFinal, siteBundleFinalBudget, secrets...)
			}
		}
		// SkipDelete as well as the other two. It keeps the tenant and skips
		// the nvkectl sweep, and this node is what those tenants' instances
		// hang off, so releasing it either deletes them through the sweep --
		// silently undoing the flag -- or orphans them with no controller left to
		// release them. Whatever keeps a tenant has to keep its site.
		if keepSite || cfg.Lifecycle.SkipDelete || (cfg.Lifecycle.PreserveOnFailure && failed) {
			GinkgoLogr.Info("keeping the site controller", "name", node.Name, "ip", node.IP,
				"kubeconfig", cluster.Kubeconfig,
				"reason", keepReason(keepSite, cfg.Lifecycle.SkipDelete))
			GinkgoLogr.Info("the instance survives this run and must be released by hand", "name", node.Name)
			return
		}
		if cluster.Kubeconfig != "" {
			if err := sweepSiteTenants(detached, cluster.Kubeconfig); err != nil {
				GinkgoLogr.Error(err, "tenant sweep failed; releasing the site anyway, and its tenants' instances may need releasing by hand")
			}
		}
		Expect(session.Teardown(detached)).To(Succeed(), "should release the site controller's node")
	}, NodeTimeout(45*time.Minute))
})

// newSiteName is the run's identity. Per run and never configured: it is the
// cluster name and, through site.TrustDomain, the SPIRE trust domain, and two
// runs sharing one would collide on both.
func newSiteName() (string, error) {
	suffix, err := randomHex(3)
	if err != nil {
		return "", err
	}
	return "nke-e2e-site-" + suffix, nil
}

// siteCapabilities is what the site node is created with, from what the run
// asked to qualify.
//
// The GPU goes and the architecture is defaulted the way matching defaults it,
// so a run naming neither still resolves. Everything else is carried through
// untouched: a key no machine answers is ignored when matching, and dropping
// keys here would mean this function had to know which ones those are.
//
// Dropping the GPU is not the same as asking for a machine without one. A GPU
// shape provides an architecture too, so {arch: arm64} matches both cpu.arm
// and gpu.gb200, and MatchInstanceType takes the first declared. The site
// controller lands on the CPU today because of the order they are listed in,
// which is not something this should depend on.
func siteCapabilities(run map[string]string) map[string]string {
	node := make(map[string]string, len(run))
	for key, value := range run {
		if key == infra.CapabilityGPU {
			continue
		}
		node[key] = value
	}
	node[infra.CapabilityArch] = cmp.Or(node[infra.CapabilityArch], infra.DefaultArch)
	node[infra.CapabilityMachineType] = infra.MachineTypeCPU
	return node
}

// requireRunCapacity asks Forge whether the shapes this full run will create
// have usable allocations: the site node and the tenant control plane on the
// CPU shape, and one worker on the worker shape.
func requireRunCapacity(ctx context.Context, provider infra.Provider) error {
	checker, ok := provider.(interface {
		RequireCapacity(ctx context.Context, selector map[string]string, cpu, workers int) error
	})
	if !ok {
		return fmt.Errorf("provider %T cannot check Forge allocation stats", provider)
	}
	capabilities, err := runCapabilities()
	if err != nil {
		return err
	}
	return checker.RequireCapacity(ctx, capabilities, 2, 1)
}

// newInfraProvider builds what creates this run's site node, as the run's
// environment declares it.
//
// One of two factories, and the only one that speaks a provider's vocabulary
// about machines. Creating a machine now and configuring the controller that
// will create machines later are separate concerns with separate lifetimes,
// so they are resolved separately and neither knows the other ran.
//
// A switch rather than a registry the providers enlist in. The set is closed
// and small, and a package that defines an interface should not know who
// implements it -- which is also why this cannot live in infra, since forge
// already imports it.
func newInfraProvider(cfg config.Config) (infra.Provider, error) {
	env, err := runEnvironment()
	if err != nil {
		return nil, err
	}
	switch env.Provider {
	case forge.ProviderName:
		return newForgeProvider(cfg, env)
	case dsx.ProviderName:
		return newDSXProvider(cfg, env)
	default:
		return nil, fmt.Errorf("environment %q names provider %q, which this suite does not implement", env.Name, env.Provider)
	}
}

// fullStageSupported reports whether a full run can build this provider's
// tenant, which is what separates it from bootstrap.
//
// The tenant half is nicoForgeSpec, and it decodes the environment as a Forge
// declaration with unknown fields rejected. A DSX environment carries
// tenantID and nodeUser, which that decoder refuses, so the tenant fails every
// time however healthy the datahall is.
//
// Checked here because the cost is the point. Without it a full run takes a
// machine, provisions it and installs the whole stack --- forty minutes, and
// on redstone-dev1-dh2 the only CPU machine there is, so nothing else can run
// meanwhile --- to reach a failure that was certain before it started.
//
// Not Forge rather than is-DSX: the limitation is that the tenant spec speaks
// one dialect, so a third provider meets the same wall, and naming DSX would
// wave it through to the same deterministic failure.
func fullStageSupported(provider string) error {
	if provider == forge.ProviderName {
		return nil
	}
	return fmt.Errorf("-e2e.site-stage full needs a tenant spec for provider %q, and the only one is Forge's: "+
		"the tenant half decodes the environment as a Forge declaration, which a %s environment is not. "+
		"Run this provider at stage provision or bootstrap until a tenant spec for it exists", provider, provider)
}

// newDeployConfig builds what the Cluster API provider the stack installs
// authenticates to its infrastructure with.
//
// The other factory. It shares an account and a credential with the provider
// above and nothing else: this one outlives the run, as a Secret in the
// cluster the run leaves behind.

func newDeployConfig() (site.CAPIProviderConfig, error) {
	env, err := runEnvironment()
	if err != nil {
		return nil, err
	}
	switch env.Provider {
	case forge.ProviderName:
		return newNicoConfig(env)
	case dsx.ProviderName:
		return newDSXNicoConfig(env)
	default:
		return nil, fmt.Errorf("environment %q names provider %q, which this suite does not implement", env.Name, env.Provider)
	}
}

// newNicoConfig is how the site's CAPNICo reaches the same Forge org the
// provider created the site node in.
func newNicoConfig(env *environment.Environment) (site.CAPIProviderConfig, error) {
	forgeEnv, err := forge.Decode(env.Infrastructure)
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", env.Name, err)
	}
	apiKey := os.Getenv(envForgeAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("%s is not set", envForgeAPIKey)
	}
	return site.NewNico(forge.DefaultEndpoint, forgeEnv.Org.ID, apiKey, forge.APIName), nil
}

// newForgeProvider projects the environment into Forge's dialect and builds a
// provider from it.
func newForgeProvider(cfg config.Config, env *environment.Environment) (infra.Provider, error) {
	forgeEnv, err := forge.Decode(env.Infrastructure)
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", env.Name, err)
	}
	capabilities, err := runCapabilities()
	if err != nil {
		return nil, err
	}
	// The run's capabilities choose the site, not a name: a stream asks for
	// {gpu: gb200, arch: amd64} and which site owns both such machines is
	// infrastructure. Resolving here also makes capabilities nothing can
	// satisfy fail before a machine exists rather than a minute into
	// provisioning.
	selection, err := forgeEnv.Resolve(capabilities)
	if err != nil {
		return nil, err
	}
	forgeSite := selection.Site
	apiKey := os.Getenv(envForgeAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("%s is not set", envForgeAPIKey)
	}

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
			return nil, jerr
		}
	}

	versions, err := requireVersions(versionKube, versionCNIOVN, versionNerdctl, versionKind, versionClusterctl)
	if err != nil {
		return nil, err
	}
	provider, err := forge.New(forge.Options{
		Env:      forgeEnv,
		SiteName: forgeSite.Key,
		APIKey:   apiKey,
		JumpHost: forgeSite.JumpHost,
		NodeUser: cfg.Node.User,
		// The site node boots the OS image built for its own architecture,
		// which is the same image a tenant control plane of that architecture
		// boots. A GPU tenant's workers boot a different one.
		Images: func(_, arch string) (string, string, error) {
			return requireNodeArtifacts(arch)
		},
		Dialer: provision.Teleport(dialer),
		Node:   nodeInputs(versions[versionKube], versions[versionCNIOVN]),
		Tools: provision.HostTools{
			NerdctlVersion:    versions[versionNerdctl],
			KindVersion:       versions[versionKind],
			ClusterctlVersion: versions[versionClusterctl],
		},
		KeepOnFailure: keepFailedNode(cfg),
		DisableMTLS:   siteDisableMTLS,
	})
	if err != nil {
		return nil, err
	}
	return provider, nil
}

// keepFailedNode reports whether a node that fails to provision should be left
// in place. A failed provision is the one case the node itself can answer --
// whether cloud-init ran, what it wrote, what the console saw -- and the
// provider releases it by default, which destroys that.
//
// Both settings mean it: -e2e.keep-site keeps whatever the run made, and
// preserveOnFailure keeps a failed run's machines, which this is one of.
func keepFailedNode(cfg config.Config) bool {
	return keepSite || cfg.Lifecycle.SkipDelete || cfg.Lifecycle.PreserveOnFailure
}

// newDSXProvider projects the environment into DSX's dialect and builds a
// provider from it.
//
// It takes no credential as an argument, but one is required: the provider
// mints its own token from DSX_<DATAHALL>_SSA_CLIENT_ID and _SECRET, named for
// the key the resolved site is filed under, and fails naming both when either
// is empty. A datahall also keeps a bootstrap client of its own in its cluster,
// which both recorded manual runs borrowed, but the provider does not read it:
// that identity belongs to the site rather than to whoever is running, it needs
// Kubernetes access, and it bypasses the org scoping a run stays inside.
func newDSXProvider(cfg config.Config, env *environment.Environment) (infra.Provider, error) {
	dsxEnv, err := dsx.Decode(env.Infrastructure)
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", env.Name, err)
	}
	capabilities, err := runCapabilities()
	if err != nil {
		return nil, err
	}
	// Resolved here for the same reason Forge resolves here: capabilities
	// nothing can satisfy fail before a machine exists.
	selection, err := dsxEnv.Resolve(capabilities)
	if err != nil {
		return nil, err
	}
	dsxSite := selection.Site

	dialer := &teleport.Dialer{
		Login:        dsxSite.TeleportLogin,
		IdentityFile: cfg.Runner.TeleportIdentityFile,
	}
	if password := os.Getenv(envJumpHostPassword); password != "" {
		if jerr := dialer.JumpOverSSH(teleport.SSHJumpHost{
			User:               cmp.Or(dsxSite.JumpHostUser, dsxSite.TeleportLogin),
			Password:           password,
			HostKeyFingerprint: dsxSite.JumpHostKeyFingerprint,
		}); jerr != nil {
			return nil, jerr
		}
	}

	versions, err := requireVersions(versionKube, versionCNIOVN, versionNerdctl, versionKind, versionClusterctl)
	if err != nil {
		return nil, err
	}
	return dsx.New(dsx.Options{
		Env:      dsxEnv,
		SiteName: dsxSite.Key,
		// The site node boots the OS image built for its own architecture,
		// which is the same image a tenant control plane of that architecture
		// boots. A GPU tenant's workers boot a different one.
		Images: func(_, arch string) (string, string, error) {
			return requireNodeArtifacts(arch)
		},
		Dialer: provision.Teleport(dialer),
		Node:   nodeInputs(versions[versionKube], versions[versionCNIOVN]),
		Tools: provision.HostTools{
			NerdctlVersion:    versions[versionNerdctl],
			KindVersion:       versions[versionKind],
			ClusterctlVersion: versions[versionClusterctl],
		},
		KeepOnFailure: keepFailedNode(cfg),
		DisableMTLS:   siteDisableMTLS,
	})
}

// newDSXNicoConfig is how the site's CAPNICo reaches the same datahall the
// provider created the site node in.
//
// The token is minted once and written into a Secret, where it lives as a
// static string. On this substrate it expires in fifteen minutes, and CAPNICo
// has no refresh path: the install itself needs no live authentication, so it
// completes, but a tenant create afterwards will fail on an expired token
// until the Secret is patched. The fix is a credential that outlives the
// run, which is a provisioning question rather than a code one -- see the
// note on dsx.MintToken.
func newDSXNicoConfig(env *environment.Environment) (site.CAPIProviderConfig, error) {
	dsxEnv, err := dsx.Decode(env.Infrastructure)
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", env.Name, err)
	}
	capabilities, err := runCapabilities()
	if err != nil {
		return nil, err
	}
	selection, err := dsxEnv.Resolve(capabilities)
	if err != nil {
		return nil, err
	}
	dsxSite := selection.Site

	token, err := dsx.MintToken(context.Background(), dsxSite)
	if err != nil {
		return nil, err
	}
	return site.NewNico(dsxSite.Endpoint(), dsxEnv.Org.ID, token, dsx.APIName), nil
}

// keepReason names which setting held the node, so the log says what to
// change rather than only that something did.
func keepReason(keepSite, skipDelete bool) string {
	switch {
	case keepSite:
		return "-e2e.keep-site"
	case skipDelete:
		return "lifecycle.skipDelete"
	default:
		return "lifecycle.preserveOnFailure after a failure"
	}
}

// sessionRelay tunnels a tenant's API server through the site node, which is
// the only machine in the run that can reach it.
//
// One tunnel per address, kept for the life of the session. Every tenant
// client asks for a REST config of its own -- twenty-odd specs do, some of
// them repeatedly inside an Eventually -- and a tunnel each would leak
// listeners for the hour the run lasts.
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

// siteDiagnostics is written next to qualify-e2e.log, in the directory the
// suite runs from, so CI picks it up with the rest of the run's record.
const siteDiagnostics = "nvkectl-diagnostics.tar.gz"

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

// siteClusterState is written beside siteDiagnostics, and is plain text
// rather than a tarball so a reader can open it from the artifact listing.
const siteClusterState = "site-cluster-state.txt"

// siteClusterStateCollected is set once this run has written siteClusterState.
var siteClusterStateCollected bool

// collectSiteClusterState records what the site cluster says about the controller.
//
// Separate from collectSiteDiagnostics because the two cover different
// failures and neither substitutes for the other: nvkectl writes diagnostics
// when the bootstrap fails, so a run that bootstrapped and then never saw the controller
// answer gets ErrNoDiagnostics and, before this, kept nothing.
//
// Best effort and never fails the spec, for the same reason as the
// diagnostics: the failure that prompted it is the finding.
func collectSiteClusterState(ctx context.Context, session infra.Session, secrets ...string) {
	if siteClusterStateCollected {
		return
	}
	// Detached and short. The cluster is reached from the node, so this does
	// not wait on anything the failing probe waits on, and a spec that died
	// on its own timeout arrives here with a context already done.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	if err := site.CollectClusterState(ctx, session, siteClusterState, secrets...); err != nil {
		GinkgoLogr.Error(err, "could not collect the site cluster's state")
		return
	}
	siteClusterStateCollected = true
	GinkgoLogr.Info("collected the site cluster's state", "path", siteClusterState)
}

// The failure bundles, beside the other two files: the node's state and logs,
// the cluster's objects, events and pod logs across every namespace, and the
// sampler's timeline. siteBundle is taken at the spec that failed, and
// siteBundleFinal from the teardown when a later spec failed too. A note with
// the same prefix takes a bundle's place when it could not be collected or
// could not be shown clean, so an absent tarball in the artifact always comes
// with a reason.
const (
	siteBundlePrefix = "site-bundle"
	siteBundle       = siteBundlePrefix + ".tar.gz"
	siteBundleFinal  = siteBundlePrefix + "-final.tar.gz"
	// siteBundleBudget bounds a collection at a failing spec. The teardown's
	// shares its 45 minutes with the tenant sweep and gets less.
	siteBundleBudget      = 9 * time.Minute
	siteBundleFinalBudget = 7 * time.Minute
	// siteSamplerBudget bounds starting and stopping the sampler, which is a
	// unit file and one systemctl call each way.
	siteSamplerBudget = time.Minute
)

var (
	// siteBundleCollected is set once a bundle has been written this run.
	siteBundleCollected bool
	// failedSinceBundle is set when a spec failed after that.
	failedSinceBundle bool
	// bundledThisSpec is set by a collection inside a spec and read, then
	// cleared, by that spec's AfterEach, which otherwise could not tell the
	// failure that was just bundled from a later one.
	bundledThisSpec bool
)

// removeStaleBundles clears the bundles and notes an earlier run left in the
// working directory, which describe another node.
func removeStaleBundles() {
	stale, _ := filepath.Glob(siteBundlePrefix + "*")
	for _, p := range stale {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			Fail(fmt.Sprintf("removing a stale %s: %v", p, err))
		}
	}
}

// collectSiteBundle writes dest from the node. Best effort and never fails
// the spec, like the two collectors above.
//
// Bounded by the budget and by the caller's context, both: a spec that dies
// on its timeout has Ginkgo moving on after a grace period, and a collection
// that kept running would overlap the teardown's own on the same node. The
// teardown passes a context of its own, detached from the specs', so its
// collection runs on its own clock.
func collectSiteBundle(ctx context.Context, session infra.Session, dest string, budget time.Duration, secrets ...string) {
	if ctx.Err() != nil {
		GinkgoLogr.Info("not collecting the site bundle from a spec whose time is up; the teardown will", "path", dest)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	report, err := site.CollectBundle(ctx, session, dest, budget, secrets...)
	if err != nil {
		GinkgoLogr.Error(err, "could not collect the site bundle", "path", dest)
		// The redaction failure writes its own note, naming what it found.
		if !errors.Is(err, site.ErrBundleRedaction) {
			_ = os.WriteFile(dest+".collection-failed.txt", []byte("the site bundle was not collected: "+err.Error()+"\n"), 0o600)
		}
		return
	}
	siteBundleCollected = true
	failedSinceBundle = false
	bundledThisSpec = true
	GinkgoLogr.Info("collected the site bundle", "path", dest, "bytes", report.Bytes,
		"sections", report.Sections, "failed", report.Failed, "skipped", report.Skipped,
		"partial", report.Partial, "interrupted", report.Interrupted)
}

// startSiteSampler starts the timeline on the node. Best effort: a run
// without a timeline still has everything else.
func startSiteSampler(ctx context.Context, session infra.Session) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), siteSamplerBudget)
	defer cancel()
	if err := site.StartSampler(ctx, session); err != nil {
		GinkgoLogr.Error(err, "could not start the site sampler; the bundle will have no timeline")
		return
	}
	GinkgoLogr.Info("started the site sampler")
}

// stopSiteSampler stops it, leaving the timeline on the node for the bundle.
func stopSiteSampler(ctx context.Context, session infra.Session) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), siteSamplerBudget)
	defer cancel()
	if err := site.StopSampler(ctx, session); err != nil {
		GinkgoLogr.Error(err, "could not stop the site sampler")
	}
}

// nodeInputs is what cloud-init writes onto the node. The agent's arguments
// are decided here because they have to agree with the bootstrap's
// --disable-mtls, which comes from the same constant: the two sides
// negotiate with each other, and a mismatch surfaces as an opaque TLS
// handshake error rather than as a configuration problem.
func nodeInputs(kubernetesVersion, cniVersion string) nvke.NodeInputs {
	inputs := nvke.NodeInputs{
		KubernetesVersion: kubernetesVersion,
		CNIVersion:        cniVersion,
	}
	if siteDisableMTLS {
		inputs.AgentArgs = nvke.AgentArgsNoMTLS
	}
	return inputs
}

// candidate is what the site is built from, resolved once the node exists.
// Only the full stage deploys the stack; the bootstrap stage checks the API
// server instead of the controller.
// nvkectlBinary is the linux nvkectl for an architecture, on the runner. Staged
// rather than downloaded: every release is a pre-release, which the
// latest-release URL skips.
func nvkectlBinary(cfg config.Config, arch string) (string, error) {
	repo, err := cloneRoot()
	if err != nil {
		return "", err
	}
	return inRepo(repo, cmp.Or(cfg.Nvkectl.Binary, filepath.Join("cli", "dist", "nvkectl-linux-"+arch))), nil
}

// siteSpec is what the node is staged with: the image
// archives. It says nothing about what is installed afterwards, which is
// stack's business, so a run that stops at Kubernetes calls only this one.
func siteSpec(cfg config.Config, name string) (site.Spec, error) {
	repo, err := cloneRoot()
	if err != nil {
		return site.Spec{}, err
	}
	versions, err := requireVersions(versionController, versionCAPIProvisioned, versionKube, versionCNIOVN)
	if err != nil {
		return site.Spec{}, err
	}
	images := cfg.Site.Images
	controller := site.Image{Repository: images.Controller.Repository, Tag: versions[versionController]}
	provisioned := site.Image{Repository: images.CAPIProvisioned.Repository, Tag: versions[versionCAPIProvisioned]}

	return site.Spec{
		Name: name,
		ImageArchives: []string{
			inRepo(repo, images.Controller.Archive),
			inRepo(repo, images.CAPIProvisioned.Archive),
			inRepo(repo, images.Profiles.Archive),
		},
		KubernetesVersion:    versions[versionKube],
		CNI:                  siteCNI,
		CNIVersion:           versions[versionCNIOVN],
		DisableMTLS:          siteDisableMTLS,
		ControllerImage:      controller.Ref(),
		CAPIProvisionedImage: provisioned.Ref(),
	}, nil
}

// stack is what gets installed once Kubernetes is up, and the controller
// version the controller is then expected to report.

// cloneRoot is the checkout the deploy path is exported from.
func cloneRoot() (string, error) {
	top, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("find the clone to export the deploy path from: %w", err)
	}
	return strings.TrimSpace(string(top)), nil
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

// newStack builds the stack and names the credentials it put in it, for
// redacting the diagnostics archive. Returned rather than asked of the stack
// afterwards: this is where every one of them is read or generated, so nothing
// else has to know which fields are credentials, and a provider configuration
// that could be asked for its own would have to hand them to anything holding
// it.
func newStack(cfg config.Config, arch string) (site.Stack, []string, error) {
	deployConfig, err := newDeployConfig()
	if err != nil {
		return site.Stack{}, nil, err
	}
	version, err := requireVersion("controller")
	if err != nil {
		return site.Stack{}, nil, err
	}
	controller := site.Image{Repository: cfg.Site.Images.Controller.Repository, Tag: version}
	repo, err := cloneRoot()
	if err != nil {
		return site.Stack{}, nil, err
	}
	// The deploy path carries the controller's own chart, so it is exported
	// from the commit that controller was built from rather than from whatever
	// is checked out.
	ref, err := deployPathRef(repo, cfg.Site.DeployPathRef, version)
	if err != nil {
		return site.Stack{}, nil, err
	}
	// Here, in BeforeAll, rather than when the stack deploys: this also
	// checks the controller's name against the release, which has to fail
	// before a machine is taken.
	assets, cleanupAssets, err := site.ExportDeployAssets(context.Background(), repo, ref)
	if err != nil {
		return site.Stack{}, nil, err
	}
	DeferCleanup(cleanupAssets)

	images := cfg.Site.Images
	profiles := site.Image{Repository: images.Profiles.Repository, Tag: images.Profiles.Tag}
	tenantPassword, err := randomHex(16)
	if err != nil {
		return site.Stack{}, nil, err
	}
	dbPassword, err := randomHex(16)
	if err != nil {
		return site.Stack{}, nil, err
	}
	unameArch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[arch]
	GinkgoLogr.Info("deploying the stack", "controller", controller.Ref(), "profiles", profiles.Ref())
	return site.Stack{
		Assets:     assets,
		Controller: controller,
		Profiles:   profiles,
		Provider:   deployConfig,
		// The login baked into tenant nodes' user-data. Nothing outside the
		// run needs it.
		Tenant: site.Tenant{User: "nvke-tenant", Password: tenantPassword},
		// Set here rather than left for the deployer to generate, so the
		// value is one the redaction list can name.
		DatabasePassword: dbPassword,
		Arch:             unameArch,
		// The provider configuration names its own credential, so a DSX run's
		// minted token is redacted the same way the Forge key is. Asked of the
		// config rather than listed here: this function cannot know which
		// field of a provider it does not name is a secret.
	}, append([]string{os.Getenv(envForgeAPIKey), tenantPassword, dbPassword}, deployConfig.Credentials()...), nil
}

// apiServerReachable reports whether the forwarded kubeconfig reaches the
// cluster's API server. Asserted in its own spec rather than substituted for
// the controller probe, so it holds in every stage that bootstraps.
func apiServerReachable(kubeconfig string) error {
	restConfig, err := kube.RESTConfig(kubeconfig, "")
	if err != nil {
		return err
	}
	client, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return err
	}
	_, err = client.ServerVersion()
	return err
}

// Budgets for the teardown sweep, sized against the AfterAll's 45-minute
// NodeTimeout that contains it. They were 40 and 30 minutes, which left the
// site release five minutes of that node -- so a slow sweep starved the one
// step that must not be skipped. Releasing the site is what stops its Forge
// instance leaking, and a leaked site is worse than a leaked tenant: it is the
// larger instance, and its controller is the only thing that can still delete the
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
	client, err := nvke.NewClient(ctx, restConfig, nvke.Options{Delete: sweepDeleteBudget, Poll: 30 * time.Second})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sweepBudget)
	defer cancel()
	return site.SweepTenants(ctx, terminatingAware{client})
}

// terminatingAware skips the wait for a tenant whose machines Forge already
// reported Terminating. Releasing the site then strands nothing: Forge owns
// those instances' release whether or not the controller that asked is still
// there.
type terminatingAware struct{ *nvke.Client }

func (c terminatingAware) WaitForClusterDeleted(ctx context.Context, id openapi_types.UUID) error {
	if terminatingTenants[id] {
		GinkgoLogr.Info("tenant instances already Terminating; not waiting for the cluster to go", "cluster", id)
		return nil
	}
	return c.Client.WaitForClusterDeleted(ctx, id)
}

func inRepo(repo, path string) string {
	if relative, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, relative)
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
	git("-c", "user.name=e2e", "-c", "user.email=e2e@example.com", "-c", "commit.gpgsign=false",
		"commit", "-q", "--allow-empty", "-m", "first")
	git("-c", "tag.gpgsign=false", "tag", "controller/v0.1.1-rc.8")
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

// TestSiteCapabilitiesAskForAMachineWithoutAGPU pins the key that keeps the
// site controller off GPU hardware.
//
// A GPU shape provides an architecture as well as a model, so matching on
// architecture alone is satisfied by one. site-2 declares cpu.arm before
// gpu.gb200 and MatchInstanceType takes the first, so the site controller
// lands on the CPU by declaration order. Reordering that file, or adding a
// second arm64 GPU above it, would put a GB200 under the site controller.
func TestSiteCapabilitiesAskForAMachineWithoutAGPU(t *testing.T) {
	for name, run := range map[string]map[string]string{
		"gpu run":       {infra.CapabilityArch: "arm64", infra.CapabilityGPU: "gb200"},
		"cpu run":       {infra.CapabilityArch: "amd64"},
		"no arch named": {},
	} {
		t.Run(name, func(t *testing.T) {
			node := siteCapabilities(run)
			if got := node[infra.CapabilityMachineType]; got != infra.MachineTypeCPU {
				t.Errorf("machineType = %q, want %q: the site controller must not match a GPU shape", got, infra.MachineTypeCPU)
			}
			if _, ok := node[infra.CapabilityGPU]; ok {
				t.Errorf("the GPU survived into the site's capabilities: %v", node)
			}
			if node[infra.CapabilityArch] == "" {
				t.Errorf("architecture is empty, so matching cannot default it: %v", node)
			}
		})
	}
}
