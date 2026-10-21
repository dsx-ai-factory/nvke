// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/controllerdeploy"
	// Aliased: this file has a local variable named engine.
	bootstrapengine "github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/engine"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/etcdkey"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/nvkrrelease"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitecluster"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/spireentrydump"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/untaint"
	"github.com/dsx-ai-factory/nvke/cli/internal/clients"
	"github.com/dsx-ai-factory/nvke/cli/internal/config"
	"github.com/dsx-ai-factory/nvke/cli/internal/envalias"
	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
)

const (
	// siteSubcommand is the name of the "bootstrap site" subcommand.
	siteSubcommand = "site"
)

// newBootstrapCmd is the "bootstrap" command group. Site bootstrap is the only
// subcommand today; the group leaves room for further bootstrap targets.
func newBootstrapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap NVKE clusters from a declarative site spec",
	}
	cmd.AddCommand(newBootstrapSiteCmd())
	return cmd
}

// bootstrapSiteOptions holds the flag values for "bootstrap site".
type bootstrapSiteOptions struct {
	siteSpec string
	dryRun   bool
	// controllerImage and provisionedProviderImage are the --nvke-controller-image and
	// --provisioned-provider-image flags. A flag wins over its variable.
	controllerImage          string
	provisionedProviderImage string
	// diagnosticsDir is where a failed bootstrap writes what the cluster had
	// to say for itself, before it is torn down.
	diagnosticsDir string
	// collectDiagnostics also collects them when the bootstrap succeeds. It is
	// set by -v/--verbose.
	collectDiagnostics bool
	keepBootstrapKind  bool

	// release is an NVKR release description. With infra.provider nico its
	// capnico-controller artifact replaces the CAPNICo image, as the deploy
	// path's render does. arch selects the artifact's architecture.
	release string
	arch    string

	// disableMTLS stands the site up without SPIFFE, for bring-up debugging.
	disableMTLS bool

	// clusterTimeout bounds the wait for CWE to finish provisioning the
	// cluster, from the create call to every node having joined.
	clusterTimeout time.Duration

	// kubeconfigOut is where the finished cluster's admin breakglass kubeconfig is
	// written. Empty defaults to <clusterName>.kubeconfig in the working
	// directory.
	kubeconfigOut string

	// kubeconfigTimeout bounds the wait for tenant-access to publish the
	// finished cluster's kubeconfig Secret. Zero uses controllerdeploy's default.
	kubeconfigTimeout time.Duration

	// encryptionKeyOut is where the finished cluster's etcd encryption key is
	// written, as a Secret manifest. Empty defaults to
	// <clusterName>-etcd-encryption-key.json in the working directory.
	encryptionKeyOut string

	// bootstrapRunner overrides the CommandRunner the bootstrap engine uses.
	// Nil in production (bootstrapengine.New defaults to the real exec-backed
	// runner); tests set it to a fake so this command can be exercised
	// without a real kind binary or container runtime.
	bootstrapRunner bootstrapengine.CommandRunner

	// cweRunner overrides the CommandRunner controllerdeploy uses (helm/kubectl).
	// Nil in production; tests set it to a fake for the same reason as
	// bootstrapRunner above.
	cweRunner controllerdeploy.CommandRunner

	// dumpSpireEntries overrides the SPIRE bootstrap-resource export. Nil uses
	// a Kubernetes client built from the bootstrap kubeconfig; tests replace it
	// to avoid contacting a real cluster.
	dumpSpireEntries func(context.Context, string, string) (int, error)

	// untaintControlPlane overrides the control-plane untaint. Nil uses a
	// client built from the cluster's kubeconfig; tests replace it.
	untaintControlPlane func(ctx context.Context, kubeconfigPath string, logger *slog.Logger) ([]string, error)

	// lookPath resolves the container runtime binary. Nil uses exec.LookPath;
	// tests substitute it so they do not require a real nerdctl installation,
	// the same seam bootstrapRunner and cweRunner provide for the commands.
	lookPath func(string) (string, error)

	// dialContext is how the pre-create probe reaches each site spec host.
	// Nil uses a net.Dialer; tests substitute it so no real host is dialled.
	dialContext                 func(ctx context.Context, network, address string) (net.Conn, error)
	allowControlPlaneWithoutVIP bool
}

const (
	// kubeAPIServerPort is where a control plane answers. A site spec host that
	// accepts a connection there already runs one.
	kubeAPIServerPort = "6443"

	// hostProbeTimeout bounds each dial. A host with nothing listening refuses
	// at once; the timeout is for one that drops the packet instead.
	hostProbeTimeout = 3 * time.Second
)

func newBootstrapSiteCmd() *cobra.Command {
	opts := &bootstrapSiteOptions{}

	cmd := &cobra.Command{
		Use:   siteSubcommand,
		Short: "Bootstrap a new NVKE management cluster from a site spec file",
		Long: `Bootstrap a vanilla Kubernetes cluster from the nodes described by the site
spec file passed with -f/--sitespec: pre-provisioned hosts it lists
(infra.provider provisioned, the default), or machines CAPNICo allocates from a
NICo site (infra.provider nico).

The site spec file is loaded and validated first. On any invalid or missing
field the command fails immediately with a clear error, before any workflow is
dispatched or any node is touched.

The bootstrap side-loads the images this repository builds, nke-controller:latest
and, with the provisioned provider, cluster-api-provider-provisioned:latest,
which "make -C cli images" builds. With nico, CAPNICo's image is public and
pulled by the cluster; --release pins it to an NVKR release's artifact. --nvke-controller-image and --provisioned-provider-image, or
NVKECTL_NVKE_CONTROLLER_IMAGE and NVKECTL_CAPI_PROVISIONED_IMAGE, name other images, such as
ones pushed to a registry; a flag wins over its variable. cli/README.md lists
every NVKECTL_* variable.

Every node becomes a control-plane node. Once the cluster is up, the
control-plane NoSchedule taint is removed from each node so workloads can run.

With nico, infra.nico names the NICo and the MachineImage and ClusterRelease
manifests the controller needs; the NICo token is read from the file the
NVKECTL_NICO_CREDENTIALS variable names; the login rendered into every node
comes from NVKECTL_NODE_USER and NVKECTL_NODE_PASSWORD; and
spire.externalAddress is required unless --disable-mtls is set. The nodes'
names and addresses are not printed; read them with "kubectl get nodes -o wide"
on the finished cluster.

See the bundled examples/sitespec.yaml and docs/sitespec.md for the
site spec file format.`,
		Example: `  # Validate and bootstrap from a site spec file
  nvkectl bootstrap site -f sitespec.yaml

  # Validate only — load, check, and print the site spec without dispatching
  nvkectl bootstrap site -f sitespec.yaml --dry-run

  # Deploy images you pushed to a registry instead of the local build
  nvkectl bootstrap site -f sitespec.yaml \
    --nvke-controller-image nvcr.io/<org>/nvke-controller:edge \
    --provisioned-provider-image nvcr.io/<org>/cluster-api-provider-provisioned:edge

  # Export this run's spans and log records to a collector the cluster can reach
  nvkectl bootstrap site -f sitespec.yaml --otlp-endpoint collector.example.com:4317`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if id := telemetry.RunFrom(cmd.Context()).TraceID(); id != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Trace ID: %s\n", id)
			}
			// A debug run is one somebody wants evidence from, so it keeps the
			// bundle whether or not the bootstrap succeeds.
			opts.collectDiagnostics = verbose
			// arch has a default, so only Changed tells an explicit value from none.
			if cmd.Flags().Changed("arch") && opts.release == "" {
				return errors.New("--arch selects the architecture of a --release artifact; pass --release too, or drop --arch")
			}
			return runBootstrapSite(cmd.Context(), cmd.OutOrStdout(), opts)
		},
	}

	cmd.Flags().StringVarP(&opts.siteSpec, "sitespec", "f", "",
		"Path to the site spec YAML file (required)")
	_ = cmd.MarkFlagRequired("sitespec")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false,
		"Load and validate the site spec and print it, but do not dispatch any workflow")
	cmd.Flags().StringVar(&opts.controllerImage, "nvke-controller-image", "",
		"NVKE controller image to deploy (overrides NVKECTL_NVKE_CONTROLLER_IMAGE and the manifest default)")
	cmd.Flags().StringVar(&opts.provisionedProviderImage, "provisioned-provider-image", "",
		"Provisioned-provider image to deploy (overrides NVKECTL_CAPI_PROVISIONED_IMAGE and the manifest default)")
	// resolveOTLP reads these two through the flag set, before RunE; they have no field.
	cmd.Flags().String(otlpEndpointFlag, "",
		"OTLP/gRPC endpoint, a host:port or an http(s):// URL (http implies --"+otlpInsecureFlag+"), that receives this run's spans and log records. "+
			"The temporary controller in the bootstrap cluster exports there too, so a loopback address is rejected")
	cmd.Flags().Bool(otlpInsecureFlag, false, "Send to --"+otlpEndpointFlag+" without TLS; requires --"+otlpEndpointFlag)
	cmd.Flags().StringVar(&opts.release, "release", "",
		"NVKR release description whose "+controllerdeploy.CAPNICoArtifact+" artifact replaces the bootstrap CAPNICo image "+
			"with its pinned digest, as \"render deploy-path --release\" does for the management cluster's CAPNICo. "+
			"The release description is not verified: run \"nvkr release verify\" on the same reference first. "+
			"Only with infra.provider nico.")
	cmd.Flags().StringVar(&opts.arch, "arch", "",
		"Architecture of the release artifact to use, linux/amd64 or linux/arm64; default: the bootstrap host's, "+
			"since the bootstrap CAPNICo runs in its Kind cluster. Only with --release.")
	cmd.Flags().StringVar(&opts.diagnosticsDir, "diagnostics-dir",
		envOrDefault("NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR", "cluster-diagnostics"),
		"Directory a failed bootstrap writes cluster diagnostics to before tearing the cluster down. "+
			"With --verbose a successful bootstrap writes them too. "+
			"Overrides NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR.")
	cmd.Flags().BoolVar(&opts.disableMTLS, "disable-mtls", false,
		"Stand the site up without SPIFFE: no SPIRE, and the controller dials node agents in plaintext. "+
			"The agents must be running with --disable-mtls too. Development only.")
	cmd.Flags().BoolVar(&opts.allowControlPlaneWithoutVIP, "allow-control-plane-without-vip", false,
		"Permit a multi-node control plane without a VIP and enable the override in the temporary NVKE controller. "+
			"Development only; replacing the initializing node can break API access.")
	cmd.Flags().BoolVar(&opts.keepBootstrapKind, "keep-bootstrap-cluster", false,
		"Leave the ephemeral bootstrap Kind cluster running after the command exits, for debugging")
	cmd.Flags().DurationVar(&opts.clusterTimeout, "timeout",
		envDurationOrDefault("NVKECTL_BOOTSTRAP_CLUSTER_TIMEOUT", 30*time.Minute),
		"How long to wait for the cluster to finish provisioning, which includes "+
			"every control plane node joining. Overrides NVKECTL_BOOTSTRAP_CLUSTER_TIMEOUT.")
	cmd.Flags().StringVar(&opts.kubeconfigOut, "kubeconfig-out", "",
		"Where to write the finished cluster's admin breakglass kubeconfig "+
			"(default: <clusterName>.kubeconfig in the working directory)")
	cmd.Flags().StringVar(&opts.encryptionKeyOut, "encryption-key-out", "",
		"Where to write the finished cluster's etcd encryption key, as a Secret manifest "+
			"(default: <clusterName>-etcd-encryption-key.json in the working directory)")

	return cmd
}

// bootstrapSettings is every NVKECTL_* variable `bootstrap site` reads, across
// this command and the packages it drives, for the one-release warning about
// settings given under their previous NVKECTL_* name.
func bootstrapSettings() []string {
	names := []string{"NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR", "NVKECTL_BOOTSTRAP_CLUSTER_TIMEOUT"}
	names = append(names, controllerdeploy.Settings...)
	names = append(names, bootstrapengine.Settings...)
	names = append(names, sitespec.Settings...)
	return names
}

// cleanupTimeout bounds each post-failure cleanup step. The steps run on a
// context detached from the command's, so they need a stop of their own.
const cleanupTimeout = 2 * time.Minute

// bootstrapPortMappings returns the host ports the Kind cluster publishes: the
// controller API on 127.0.0.1 only, and the SPIRE server on every address when
// the TPM attestor is enabled, since nodes outside the host dial it.
func bootstrapPortMappings(cweCfg controllerdeploy.Config) []bootstrapengine.PortMapping {
	// The bootstrap controller serves its API with no authentication and every
	// caller dials 127.0.0.1, so it is not published to the network.
	mappings := []bootstrapengine.PortMapping{{
		ContainerPort: cweCfg.CWEPort,
		HostPort:      cweCfg.CWEPort,
		ListenAddress: "127.0.0.1",
	}}
	if cweCfg.TPMAttestorEnabled() {
		mappings = append(mappings, bootstrapengine.PortMapping{
			ContainerPort: controllerdeploy.SPIREServerNodePort,
			HostPort:      controllerdeploy.SPIREServerNodePort,
		})
	}
	return mappings
}

func runBootstrapSite(ctx context.Context, out io.Writer, opts *bootstrapSiteOptions) (err error) {
	logger := slog.Default().With("component", "bootstrap-site")
	for _, s := range envalias.Deprecated(bootstrapSettings()...) {
		logger.Warn(s.Warning(), "deprecated", s.Old, "replacement", s.New)
	}

	// Step 1: load and validate the sitespec. Fail fast on any problem.
	inv, err := sitespec.Load(opts.siteSpec, sitespec.WithAllowControlPlaneWithoutVIP(opts.allowControlPlaneWithoutVIP))
	if err != nil {
		return err
	}
	logger.Info("loaded site spec", "path", opts.siteSpec, "cluster", inv.ClusterName)
	// Checked before the dry-run return so --dry-run is the complete pre-flight,
	// and together so one run reports every problem.
	if err := errors.Join(inv.CheckBootstrapMode(opts.disableMTLS), inv.ValidateBootstrapReferences()); err != nil {
		return fmt.Errorf("site spec file %s: %w", opts.siteSpec, err)
	}
	if warning := inv.NicoVIPWarning(); warning != "" {
		logger.Warn(warning)
	}
	// Resolved here too, so a bad --release or --arch fails the dry run.
	capnicoImage, err := capnicoReleaseImage(inv, opts.release, opts.arch)
	if err != nil {
		return err
	}
	if inv.Spire.CACertFile != "" {
		logger.Info("SPIRE upstream CA from the environment", "cert", inv.Spire.CACertFile, "key", inv.Spire.CAKeyFile)
	}

	if err := dumpSiteSpec(out, inv); err != nil {
		return err
	}
	// Said as soon as the default is known, dry run included: the controller
	// stack install shares it only for the default bootstrap cluster name.
	if inv.Spire.TrustDomain == "" {
		bootstrapCluster := bootstrapengine.DefaultConfig().ClusterName
		if bootstrapCluster == sitespec.DefaultBootstrapClusterName {
			logger.Info("spire.trustDomain is not set; using the default the controller stack install shares", "trustDomain", inv.EffectiveTrustDomain(bootstrapCluster))
		} else {
			logger.Warn("spire.trustDomain is not set; derived from the bootstrap cluster name. Set spire.trustDomain to this value before the controller stack install, whose default differs",
				"trustDomain", inv.EffectiveTrustDomain(bootstrapCluster), "installDefault", inv.EffectiveTrustDomain(sitespec.DefaultBootstrapClusterName))
		}
	}

	// Before the dry-run return, so a profiles image the deploy would refuse
	// fails here and not minutes into a real run, after the Kind cluster exists.
	if err := controllerdeploy.ValidateProfilesImage(os.Getenv(controllerdeploy.ProfilesImageEnv)); err != nil {
		return err
	}

	if opts.dryRun {
		fmt.Fprintln(out, "\nDry run: site spec is valid. No workflow dispatched.")
		return nil
	}

	// Before anything is stood up: the whole command runs on
	// nerdctl/containerd, and the first thing to reach for it is kind, which
	// execs it internally. That failure surfaces as kind's own "failed to list
	// clusters" rather than as a missing prerequisite, so check here where the
	// error can say what is actually wrong.
	if err := requireContainerRuntime(opts.lookPath); err != nil {
		return err
	}

	// Step 2: stand up the ephemeral bootstrap Kind cluster the controller will run on.
	// It is normally torn down on exit. After site-cluster creation starts, a
	// later failure retains it so the operator does not lose the controllers and
	// credentials needed for recovery.
	cfg := bootstrapengine.DefaultConfig()
	// A create that fails never reaches the diagnostics defer below, so the
	// engine collects for itself and honours the same two flags.
	cfg.DiagnosticsDir = opts.diagnosticsDir
	cfg.KeepFailedCluster = opts.keepBootstrapKind
	cweCfg, err := cweDeployConfig(inv, cfg.ClusterName, opts.disableMTLS, opts.controllerImage, opts.provisionedProviderImage)
	if err != nil {
		return err
	}

	cweCfg.CAPNICoImage = capnicoImage
	cweCfg.AllowControlPlaneWithoutVIP = opts.allowControlPlaneWithoutVIP
	cweCfg.Tracing = controllerTracing(ctx, logger)

	// The controller API has to be reachable from this host: this command deploys it
	// and then calls it. A host port mapping rather than the node container's
	// own address, because that address is not routable from the host on
	// every platform this runs on.
	cfg.ExtraPortMappings = append(cfg.ExtraPortMappings, bootstrapPortMappings(cweCfg)...)

	engine := bootstrapengine.New(opts.bootstrapRunner, logger)

	// Last checks before anything is built. Each refuses a state a fresh
	// bootstrap cannot run over, names it, and says what to do; each costs a
	// message here and a Kind cluster plus half an hour anywhere later. None of
	// them changes anything: what an earlier run left behind is left for the
	// operator to deal with, and for a later resume to pick up.
	//
	// A bootstrap cluster still running is the strongest sign of an earlier
	// attempt, and may hold that attempt's encryption key.
	releaseSteps := ""
	if inv.IsNico() {
		releaseSteps = nicoReleaseSteps(inv.ClusterName, cfg.ClusterName, "<its kubeconfig file>", cweCfg.CWEPort)
	}
	if err := requireNoBootstrapCluster(ctx, engine, cfg.ClusterName, releaseSteps); err != nil {
		return err
	}
	// After the check above: a retained bootstrap cluster owns the runtime's
	// bridge config, and its own message says how to recover it before anything
	// is deleted. Bridge-removal advice here would pre-empt that.
	if err := requireIsolatedRuntimeCNI(sharedCNIConfDir); err != nil {
		return err
	}
	// A key file already at the output path belongs to an earlier bootstrap of
	// this cluster name. It is never overwritten, so the run would fail at the
	// very end.
	if inv.Cluster.EtcdEncryptionEnabled() {
		if err := requireNoKeyFile(etcdKeyPath(opts, inv.ClusterName)); err != nil {
			return err
		}
	}
	// A host that already serves a control plane is either a retry over a
	// half-built site or a site spec pointed at a live cluster; provisioning
	// over either ends badly.
	// Nothing to probe when the machines are allocated rather than listed: they
	// do not exist yet, and have no addresses to dial.
	if !inv.IsNico() {
		if err := requireNoControlPlane(ctx, inv.Cluster.Nodes, opts.dialContext); err != nil {
			return err
		}
	} else if err := requireNoSiteAtVIP(ctx, inv.Cluster.ControlPlaneVIP, opts.dialContext); err != nil {
		// The VIP is the one address a nico site's control plane is known by
		// before it exists, and the only thing that tells a second bootstrap of
		// the same spec from a first.
		return err
	}
	// The two images this repository builds are side-loaded, not pulled. A
	// build that was never made fails here, before the cluster exists.
	if err := requireLocalImages(ctx, engine, nvkeImages(cweCfg)...); err != nil {
		return err
	}

	logger.Info("creating ephemeral bootstrap kind cluster", "cluster", cfg.ClusterName)
	cluster, err := engine.Create(ctx, cfg)
	if err != nil {
		return fmt.Errorf("standing up bootstrap kind cluster: %w", err)
	}
	siteClusterCreated := false
	// handedOver is set once the site cluster and every artifact the operator
	// needs are on disk. A failure after that is about the site cluster, so
	// without -v the bootstrap cluster's diagnostics are not collected and it
	// is torn down.
	handedOver := false
	defer func() {
		// Collect diagnostics before anything is torn down: this cluster is
		// removed on every exit path, so this is the only moment the evidence
		// still exists. Only on failure, or on any outcome under -v: a
		// successful bootstrap has nothing to explain unless asked.
		// Cleanup runs on the cancellation path too: a SIGINT during the
		// deploy cancels ctx, and both runners exec through
		// exec.CommandContext, so reusing ctx here would skip the diagnostics
		// and leave the Kind cluster standing to block the next bootstrap.
		// Each step gets its own bounded context detached from ctx, so slow
		// diagnostics cannot eat the deletion's budget.
		if opts.collectDiagnostics || (err != nil && !handedOver) {
			diagnosticsCtx, cancelDiagnostics := context.WithTimeout(
				context.WithoutCancel(ctx), cleanupTimeout,
			)
			dir := opts.diagnosticsDir
			if err != nil {
				logger.Info("bootstrap failed; collecting cluster diagnostics", "dir", dir)
			} else {
				logger.Info("collecting cluster diagnostics (--verbose)", "dir", dir)
			}
			controllerdeploy.New(opts.cweRunner, logger).
				DumpDiagnostics(diagnosticsCtx, cluster.KubeconfigPath, dir)
			cancelDiagnostics()
		}

		if opts.keepBootstrapKind || (siteClusterCreated && err != nil) {
			if siteClusterCreated && err != nil {
				logger.Warn("bootstrap failed after site cluster creation; retaining the bootstrap kind cluster for recovery",
					"cluster", cluster.Name, "kubeconfig", cluster.KubeconfigPath)
			} else {
				logger.Info("--keep-bootstrap-cluster set: leaving the bootstrap kind cluster running",
					"cluster", cluster.Name, "kubeconfig", cluster.KubeconfigPath)
			}
			return
		}
		logger.Info("tearing down bootstrap kind cluster", "cluster", cluster.Name)
		deleteCtx, cancelDelete := context.WithTimeout(
			context.WithoutCancel(ctx), cleanupTimeout,
		)
		defer cancelDelete()
		if deleteErr := engine.Delete(deleteCtx, cluster.Name); deleteErr != nil {
			logger.Error("failed to tear down bootstrap kind cluster",
				"cluster", cluster.Name, "err", deleteErr)
		}
	}()

	// Step 3: deploy Postgres, Temporal, SPIRE, Cluster API, and the controller onto the bootstrap
	// cluster and wait for the controller to report healthy on /readyz before going any
	// further — there is nothing to dispatch a workflow to otherwise.
	if err := deployAndWaitForCWE(ctx, engine, cluster, cweCfg, opts.cweRunner, logger); err != nil {
		return err
	}

	// Step 4: create the cluster. The controller owns everything from here — it drives
	// kubeadm on each host through nvke-agent and reconciles the Cluster API
	// objects that the provisioned infrastructure provider binds to the
	// site spec's hosts.
	siteClusterCreated, err = createSiteCluster(ctx, cluster, inv, opts, cweCfg.CWEPort, logger)
	if err != nil && siteClusterCreated {
		return retainedClusterError(err, inv, cluster, cweCfg.CWEPort)
	}
	if err == nil && !cweCfg.DisableMTLS {
		count, dumpErr := dumpSpireBootstrapEntries(
			ctx, opts, cluster.KubeconfigPath, filepath.Dir(siteKubeconfigPath(inv.ClusterName, opts)),
		)
		if dumpErr != nil {
			// The site cluster is built: the failure is advised on like any other
			// after it, so a nico run is not told to release a working site.
			return retainedClusterError(
				&clusterJoinedError{fmt.Errorf("writing SPIRE bootstrap resources: %w", dumpErr)},
				inv, cluster, cweCfg.CWEPort,
			)
		}
		logger.Info("wrote SPIRE bootstrap resources", "clusterStaticEntries", count)
	}
	if err != nil {
		return err
	}

	// Everything recovery needs is on disk by now, so the bootstrap cluster is
	// torn down even if the untaint fails.
	siteClusterCreated = false
	handedOver = true
	kubeconfigPath := siteKubeconfigPath(inv.ClusterName, opts)
	if inv.Cluster.EtcdEncryptionEnabled() {
		fmt.Fprintf(out, "\nThe etcd encryption key of cluster %q is at %s. Move that file off this host "+
			"and keep it with the cluster's other credentials.\n", inv.ClusterName, etcdKeyPath(opts, inv.ClusterName))
	}
	if err := makeControlPlaneSchedulable(ctx, inv, kubeconfigPath, opts, logger); err != nil {
		return err
	}

	// Nico mode does not list the allocated nodes here; kubectl get nodes shows them.
	fmt.Fprintf(out, "\nCluster %q is ready. Talk to it with:\n  kubectl --kubeconfig %s get nodes\n",
		inv.ClusterName, kubeconfigPath)
	return nil
}

// untaintTimeout bounds the whole untaint: one List and a few Updates.
const untaintTimeout = 2 * time.Minute

// makeControlPlaneSchedulable removes the control-plane taint from every node
// when the site spec's topology calls for it.
func makeControlPlaneSchedulable(
	ctx context.Context,
	inv *sitespec.SiteSpec,
	kubeconfigPath string,
	opts *bootstrapSiteOptions,
	logger *slog.Logger,
) error {
	if !inv.Cluster.ControlPlaneSchedulable() {
		logger.Info("the site spec declares worker nodes; the control-plane taint stays on every node")
		return nil
	}

	run := opts.untaintControlPlane
	if run == nil {
		run = func(ctx context.Context, kubeconfigPath string, logger *slog.Logger) ([]string, error) {
			client, err := untaint.NewClient(kubeconfigPath)
			if err != nil {
				return nil, err
			}
			return untaint.ControlPlane(ctx, client, logger)
		}
	}
	untaintCtx, cancel := context.WithTimeout(ctx, untaintTimeout)
	defer cancel()
	changed, err := run(untaintCtx, kubeconfigPath, logger)
	if err != nil {
		return &recoveryError{fmt.Errorf("making the control-plane nodes schedulable: %w; the cluster is up and its kubeconfig is at %s, "+
			"finish by hand with `%s --kubeconfig %s`", err, kubeconfigPath, untaint.ManualCommand, kubeconfigPath)}
	}
	logger.Info("control-plane nodes are schedulable", "untainted", len(changed))
	return nil
}

func dumpSpireBootstrapEntries(
	ctx context.Context,
	opts *bootstrapSiteOptions,
	sourceKubeconfig string,
	outputDir string,
) (int, error) {
	if opts.dumpSpireEntries != nil {
		return opts.dumpSpireEntries(ctx, sourceKubeconfig, outputDir)
	}
	dumper, err := spireentrydump.New(sourceKubeconfig)
	if err != nil {
		return 0, err
	}
	return dumper.Dump(ctx, outputDir)
}

// createSiteCluster posts the cluster to the controller, waits for it to come up, and
// writes out its kubeconfig and etcd encryption key. The boolean reports
// whether the controller accepted the create request, after which callers must preserve
// the management cluster on failure so the in-progress site cluster remains
// recoverable.
//
// The controller client is built here rather than being passed in because its address
// is only settled once the bootstrap cluster exists: it is the host port the
// NodePort Service was mapped to.
func createSiteCluster(
	ctx context.Context,
	bootstrapCluster *bootstrapengine.Cluster,
	inv *sitespec.SiteSpec,
	opts *bootstrapSiteOptions,
	cwePort int32,
	logger *slog.Logger,
) (bool, error) {
	cweURL := fmt.Sprintf("http://127.0.0.1:%d", cwePort)
	client, err := clients.New(config.SiteConfig{URL: cweURL})
	if err != nil {
		return false, fmt.Errorf("building the controller client for %s: %w", cweURL, err)
	}
	created, err := sitecluster.Create(ctx, client, inv, logger)
	if err != nil {
		// With nico, a create the controller may have accepted may already be
		// allocating NICo instances, which deleting the bootstrap cluster would
		// leave unmanaged; it is kept as after any failure past the create.
		return inv.IsNico() && errors.Is(err, sitecluster.ErrCreateUnconfirmed), err
	}

	// One budget spans both waits, because --timeout bounds the whole
	// provision: from the create call to every node having joined.
	timeout := opts.clusterTimeout
	if timeout <= 0 {
		timeout = sitecluster.DefaultWaitTimeout
	}
	deadline := time.Now().Add(timeout)

	// Logged after the default is applied, so the line states the budget the
	// command will actually keep to rather than the bare flag value.
	logger.Info("waiting for the cluster to finish provisioning",
		"cluster", created.Name, "timeout", timeout)

	if _, err := sitecluster.WaitProvisioned(ctx, client, created.ID, time.Until(deadline), logger); err != nil {
		return true, err
	}
	logger.Info("cluster provisioned", "cluster", created.Name, "id", created.ID)

	// The cluster reports Provisioned once its initial control-plane node is
	// up; the rest join afterwards, through workflows that live in the
	// bootstrap cluster this command is about to delete. Returning here would
	// tear that engine down underneath them.
	if err := sitecluster.WaitControlPlaneJoined(
		ctx, client, created.ID, inv.ControlPlaneCount(), time.Until(deadline), logger,
	); err != nil {
		return true, err
	}
	logger.Info("all control plane nodes joined", "cluster", created.Name, "nodes", inv.ControlPlaneCount())

	// The kubeconfig first, so a failure in the key capture below still leaves
	// the operator holding the admin credential of the cluster that was built.
	kubeconfigPath, err := writeTenantKubeconfig(ctx, bootstrapCluster, created.Name, opts, logger)
	if err != nil {
		return true, &clusterJoinedError{err}
	}

	// Every control-plane node holds the key now, and this is the last moment
	// the copy the controller keeps is reachable: it lives in the bootstrap cluster, which
	// goes when the command does.
	if inv.Cluster.EtcdEncryptionEnabled() {
		if err := saveEtcdEncryptionKey(ctx, bootstrapCluster, created.ID, inv.ClusterName,
			etcdKeyPath(opts, inv.ClusterName), kubeconfigPath, opts, logger); err != nil {
			return true, err
		}
	}
	return true, nil
}

// etcdKeyPath is where the cluster's etcd encryption key is written.
func etcdKeyPath(opts *bootstrapSiteOptions, clusterName string) string {
	if opts.encryptionKeyOut != "" {
		return opts.encryptionKeyOut
	}
	return clusterName + "-etcd-encryption-key.json"
}

// requireNoKeyFile refuses to start a bootstrap whose key file already exists,
// or whose key file cannot be checked for.
func requireNoKeyFile(path string) error {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return fmt.Errorf("%s already exists and holds the etcd encryption key of an earlier bootstrap of "+
			"this cluster name. If that cluster's nodes were reset, delete the file and re-run; "+
			"otherwise pass --encryption-key-out to write the new key elsewhere", path)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		// Anything else — a parent that is not a directory, a path that cannot
		// be read — would fail the write at the very end; fail here instead.
		return fmt.Errorf("checking the etcd encryption key path %s: %w", path, err)
	}
}

// saveEtcdEncryptionKey copies the key Secret the controller holds for the cluster out of
// the bootstrap cluster and onto disk, as a manifest a controller installed
// later can be given.
//
// Any failure is a keyCaptureError: the cluster is built and encrypted by
// this point, and what has gone wrong is that its key is about to become
// unreachable — which is a different situation, with different advice, from
// a bootstrap that failed to build anything.
func saveEtcdEncryptionKey(
	ctx context.Context,
	bootstrapCluster *bootstrapengine.Cluster,
	clusterID uuid.UUID,
	clusterName, path, kubeconfigPath string,
	opts *bootstrapSiteOptions,
	logger *slog.Logger,
) error {
	fail := func(err error) error {
		return &keyCaptureError{
			clusterName:    clusterName,
			keyPath:        path,
			kubeconfigPath: kubeconfigPath,
			recover:        controllerdeploy.EtcdEncryptionKeyCommand(bootstrapCluster.KubeconfigPath, clusterID, path),
			err:            err,
		}
	}

	readCtx, cancel := context.WithTimeout(ctx, etcdKeyReadTimeout)
	defer cancel()
	data, err := controllerdeploy.New(opts.cweRunner, logger).
		EtcdEncryptionKey(readCtx, bootstrapCluster.KubeconfigPath, clusterID)
	if err != nil {
		return fail(err)
	}

	manifest, err := etcdkey.Manifest(clusterID, clusterName, data)
	if err != nil {
		return fail(err)
	}
	if err := etcdkey.Write(path, manifest); err != nil {
		return fail(err)
	}

	logger.Info("wrote the etcd encryption key", "path", path, "sha256", etcdkey.Fingerprint(data))
	return nil
}

// etcdKeyReadTimeout bounds the read of the key Secret. It exists by the time
// this runs, so this is a bound on a wedged kubectl, not a wait.
const etcdKeyReadTimeout = 2 * time.Minute

// recoveryError marks a failure whose message tells the operator how to
// recover. It must reach stderr even when a signal caused the failure, so
// main prints it instead of the bare cancellation line.
type recoveryError struct{ err error }

func (e *recoveryError) Error() string { return e.err.Error() }
func (e *recoveryError) Unwrap() error { return e.err }

// clusterJoinedError marks a failure after every control-plane node joined: the
// site cluster is up and working, so what is left to recover is its outputs.
// A keyCaptureError is the same, and is not wrapped in this.
type clusterJoinedError struct{ err error }

func (e *clusterJoinedError) Error() string { return e.err.Error() }
func (e *clusterJoinedError) Unwrap() error { return e.err }

// keyCaptureError is a bootstrap that built and encrypted its cluster and then
// failed to save the key.
type keyCaptureError struct {
	clusterName    string
	keyPath        string
	kubeconfigPath string
	recover        string
	err            error
}

func (e *keyCaptureError) Error() string {
	return fmt.Sprintf("cluster %q is up and its etcd data is encrypted, but its encryption key was not saved to %s: %v. "+
		"Its kubeconfig is at %s. The bootstrap cluster still holds the key: do not delete it, and do not re-run "+
		"bootstrap, until the key is saved. Save it, as the Secret the bootstrap cluster holds, with: %s",
		e.clusterName, e.keyPath, e.err, e.kubeconfigPath, e.recover)
}

func (e *keyCaptureError) Unwrap() error { return e.err }

// writeTenantKubeconfig mints an admin breakglass kubeconfig for the finished cluster
// and writes it beside the operator.
//
// 0o600: it is a cluster-admin credential.
func writeTenantKubeconfig(
	ctx context.Context,
	bootstrapCluster *bootstrapengine.Cluster,
	clusterName string,
	opts *bootstrapSiteOptions,
	logger *slog.Logger,
) (string, error) {
	path := siteKubeconfigPath(clusterName, opts)

	adminConf, err := controllerdeploy.New(opts.cweRunner, logger).
		TenantKubeconfig(ctx, bootstrapCluster.KubeconfigPath, clusterName, opts.kubeconfigTimeout)
	if err != nil {
		return "", fmt.Errorf("reading the kubeconfig of cluster %q: %w", clusterName, err)
	}

	kubeconfig, err := sitecluster.GenerateBreakglassKubeconfig(ctx, adminConf, clusterName)
	if err != nil {
		return "", fmt.Errorf("issuing an admin breakglass kubeconfig for cluster %q: %w", clusterName, err)
	}

	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}

	logger.Info("wrote the cluster kubeconfig", "path", path)
	return path, nil
}

func siteKubeconfigPath(clusterName string, opts *bootstrapSiteOptions) string {
	if opts.kubeconfigOut != "" {
		return opts.kubeconfigOut
	}
	return clusterName + ".kubeconfig"
}

// nicoReleaseSteps says how to release the NICo instances CAPNICo allocated for
// the site cluster. Deleting the bootstrap Kind cluster does not: it runs no
// finalizers, so the instances stay allocated and unmanaged, with the tenant
// login in their user data, and every re-run allocates another set. Deleting
// the site cluster through the bootstrap cluster's controller deletes each
// node's Cluster API machine, which releases its instance.
func nicoReleaseSteps(siteCluster, bootstrapCluster, kubeconfig string, port int32) string {
	return fmt.Sprintf("release the NICo instances CAPNICo allocated first, because deleting the bootstrap cluster does not "+
		"(it leaves them allocated and unmanaged, with the tenant login in their user data): point nvkectl at this "+
		"cluster's controller with %s, run "+
		"`nvkectl delete cluster %s`, and wait until `nvkectl get clusters` no longer lists it; set-site made %s the "+
		"active site, so switch back afterwards with `nvkectl config use-site <name>`",
		nicoSetSiteCommand(bootstrapCluster, kubeconfig, port), siteCluster, bootstrapCluster)
}

// nicoUnconfirmedSteps says what to do when the create request may have reached
// the controller without its answer reaching nvkectl: find out whether the site
// cluster exists and, if it does, release its NICo instances as nicoReleaseSteps
// does.
func nicoUnconfirmedSteps(siteCluster, bootstrapCluster, kubeconfig string, port int32) string {
	return fmt.Sprintf("point nvkectl at this cluster's controller with %s and run `nvkectl get clusters`; if it lists %s, "+
		"release the NICo instances CAPNICo may be allocating for it, because deleting the bootstrap cluster does not "+
		"(it leaves them allocated and unmanaged, with the tenant login in their user data): run `nvkectl delete cluster %s` "+
		"and wait until `nvkectl get clusters` no longer lists it; set-site made %s the active site, so switch back "+
		"afterwards with `nvkectl config use-site <name>`",
		nicoSetSiteCommand(bootstrapCluster, kubeconfig, port), siteCluster, siteCluster, bootstrapCluster)
}

// nicoSetSiteCommand is the command that points nvkectl at the bootstrap
// cluster's controller.
func nicoSetSiteCommand(bootstrapCluster, kubeconfig string, port int32) string {
	return fmt.Sprintf("`nvkectl config set-site --name %s --url http://127.0.0.1:%d --kubeconfig %s`", bootstrapCluster, port, kubeconfig)
}

// retainedClusterError wraps err, from a failure after the site cluster was
// created, with what to do about the bootstrap cluster that was kept for
// recovery. With nico the advice leads with releasing the allocated instances,
// but only for a failure before every control-plane node joined: after that the
// site cluster is working, and deleting it would destroy it and its NICo
// machines, so the advice is to recover the outputs and delete only Kind.
func retainedClusterError(err error, inv *sitespec.SiteSpec, cluster *bootstrapengine.Cluster, port int32) error {
	var capture *keyCaptureError
	var joined *clusterJoinedError
	built := errors.As(err, &capture) || errors.As(err, &joined)
	release := ""
	if inv.IsNico() && !built {
		release = nicoReleaseSteps(inv.ClusterName, cluster.Name, cluster.KubeconfigPath, port)
	}
	// The env prefix is not optional: kind resolves a cluster through the
	// provider that created it, so without it the command reports the
	// cluster as missing rather than deleting it.
	deleteCommand := fmt.Sprintf(
		"KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name %s", cluster.Name,
	)
	// The advice below ends with the command that deletes the bootstrap
	// cluster. When the key was not saved, that cluster holds the only copy
	// the operator does not have, and the error says so instead.
	if capture != nil {
		kept := fmt.Errorf("%w (bootstrap cluster %q kept; its kubeconfig is %s)", err, cluster.Name, cluster.KubeconfigPath)
		if inv.IsNico() {
			kept = fmt.Errorf("%w; once the key is saved, delete only the bootstrap cluster with `%s`: the site cluster is up "+
				"and its NICo machines belong to it", kept, deleteCommand)
		}
		return &recoveryError{kept}
	}
	if joined != nil && inv.IsNico() {
		return &recoveryError{fmt.Errorf("%w; every control-plane node joined, so the site cluster is up and is not to be deleted; "+
			"bootstrap cluster %q was retained to recover what this run could not write (kubeconfig: %s); once you have, delete only "+
			"the bootstrap cluster with `%s`", err, cluster.Name, cluster.KubeconfigPath, deleteCommand)}
	}
	if release != "" && errors.Is(err, sitecluster.ErrCreateUnconfirmed) {
		return &recoveryError{fmt.Errorf("%w; no answer from the controller confirmed or refused the create, so site cluster %q "+
			"may exist and be allocating NICo instances, and bootstrap cluster %q was retained (kubeconfig: %s); %s; then delete "+
			"the bootstrap cluster with `%s`",
			err, inv.ClusterName, cluster.Name, cluster.KubeconfigPath,
			nicoUnconfirmedSteps(inv.ClusterName, cluster.Name, cluster.KubeconfigPath, port), deleteCommand)}
	}
	if release != "" {
		return &recoveryError{fmt.Errorf("%w; site cluster creation has started, so bootstrap cluster %q was retained for recovery "+
			"(kubeconfig: %s); %s; then delete it with `%s`",
			err, cluster.Name, cluster.KubeconfigPath, release, deleteCommand)}
	}
	return &recoveryError{fmt.Errorf("%w; site cluster creation has started, so bootstrap cluster %q was retained for recovery "+
		"(kubeconfig: %s); delete it after recovery with `%s`",
		err, cluster.Name, cluster.KubeconfigPath, deleteCommand)}
}

// requireNoBootstrapCluster refuses to start while a bootstrap cluster from an
// earlier run is still running.
//
// A leftover is kept on purpose — by --keep-bootstrap-cluster, or by a run that
// failed after building its site cluster. In the second case it may hold the
// only copy of that cluster's etcd encryption key that is not on a node, and
// the natural response to "already exists" is the delete that loses it. The
// message leads with saving the key. Nothing is deleted or reused here.
//
// With nico this is the re-run guard for a bootstrap that never got as far as a
// VIP to probe, and the leftover may hold a working site cluster rather than a
// failed one. releaseSteps, when set, is nicoReleaseSteps for this cluster: the
// message then separates a run that failed before every control-plane node
// joined, which is released and re-run, from one that succeeded or failed
// later, which is not re-run at all.
func requireNoBootstrapCluster(ctx context.Context, engine *bootstrapengine.Engine, name, releaseSteps string) error {
	exists, err := engine.Exists(ctx, name)
	if err != nil {
		return fmt.Errorf("checking for a bootstrap cluster from an earlier run: %w", err)
	}
	if !exists {
		return nil
	}
	deleteCommand := "KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name " + name
	saveKey := fmt.Sprintf("bootstrap cluster %q from an earlier run is still running. If that run built an encrypted "+
		"site cluster and did not save its key, the key is in this cluster: get its kubeconfig with "+
		"`KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get kubeconfig --name %s`, save every etcd-encryption-key-* Secret it "+
		"holds from the namespace it is in (`kubectl get secrets -A | grep etcd-encryption-key-`), ", name, name)
	if releaseSteps == "" {
		return fmt.Errorf("%sthen delete it with `%s` and re-run", saveKey, deleteCommand)
	}
	return fmt.Errorf("%sthen: if that run failed before every control-plane node joined, %s, delete the bootstrap cluster with "+
		"`%s`, and re-run; if it succeeded, or failed after every control-plane node joined, the site cluster is working: "+
		"recover from this cluster whatever that run did not write, delete only this cluster with `%s`, and do not re-run, "+
		"because a re-run allocates another set of NICo machines and brings up a second control plane advertising the "+
		"same controlPlaneVIP", saveKey, releaseSteps, deleteCommand, deleteCommand)
}

// requireNoSiteAtVIP refuses to bootstrap a nico site whose control-plane VIP
// already accepts connections on the Kubernetes API port.
//
// A nico bootstrap allocates its machines, so there is no host to probe; the VIP
// is the one address the site's control plane is known by. When it answers, a
// site cluster for this spec is most likely already running, and another
// bootstrap allocates another controlPlane.count NICo machines and brings up a
// second control plane whose kube-vip advertises the same address. An empty vip
// (a one-node spec without one) has nothing to probe; a dial error of any kind
// counts as nothing answering, as in requireNoControlPlane.
func requireNoSiteAtVIP(
	ctx context.Context,
	vip string,
	dial func(ctx context.Context, network, address string) (net.Conn, error),
) error {
	if vip == "" {
		return nil
	}
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	probeCtx, cancel := context.WithTimeout(ctx, hostProbeTimeout)
	defer cancel()
	conn, err := dial(probeCtx, "tcp", net.JoinHostPort(vip, kubeAPIServerPort))
	if err != nil {
		return nil
	}
	_ = conn.Close()
	return fmt.Errorf("the control-plane VIP %s is accepting connections on port %s, the Kubernetes API port: a site cluster "+
		"is most likely already running there, and bootstrapping again would allocate another set of NICo machines and bring "+
		"up a second control plane advertising the same VIP. If that is your site, do not re-run. If it is a half-built "+
		"one from a failed bootstrap, release its NICo instances first (see docs/bootstrap-troubleshooting.md), and re-run "+
		"once the VIP stops answering. If something else holds the address, change cluster.controlPlaneVIP in the site spec",
		vip, kubeAPIServerPort)
}

// requireNoControlPlane refuses to bootstrap over a host that already accepts
// connections on the Kubernetes API port.
//
// It is a TCP connect, nothing more: enough to catch a control plane left by
// an earlier attempt, which is the case a bootstrap cannot recover from on
// its own. Encryption is configured at kubeadm init, so a second attempt over
// an initialised node cannot pick up the key the node already holds, and the
// only way forward is to reset the node and start again. Saying so here, with
// the node named, beats a workflow error twenty minutes in.
//
// Any dial error counts as nothing listening, a timeout included. A
// pre-provisioned host commonly filters every port but the agent's, and
// refusing on that would block bootstraps that are fine. kubeadm's own
// preflight still rejects an initialised node; this check is the early,
// legible form of that rejection, not the guard.
func requireNoControlPlane(
	ctx context.Context,
	nodes []sitespec.Node,
	dial func(ctx context.Context, network, address string) (net.Conn, error),
) error {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}

	var serving []string
	for _, node := range nodes {
		probeCtx, cancel := context.WithTimeout(ctx, hostProbeTimeout)
		conn, err := dial(probeCtx, "tcp", net.JoinHostPort(node.IP, kubeAPIServerPort))
		cancel()
		if err != nil {
			continue
		}
		_ = conn.Close()
		serving = append(serving, fmt.Sprintf("%s (%s)", node.Name, node.IP))
	}
	if len(serving) == 0 {
		return nil
	}

	return fmt.Errorf("port %s, the Kubernetes API port, is accepting connections on: %s. "+
		"A control plane is most likely already running there. If this is a retry of a failed bootstrap, "+
		"confirm the node has kubeadm state (/etc/kubernetes/manifests), run `kubeadm reset -f` on every node "+
		"that does, and re-run. If something else is listening, check the node list in the site spec",
		kubeAPIServerPort, strings.Join(serving, ", "))
}

// envDurationOrDefault reads a duration from the environment, falling back to a
// default. A malformed or non-positive value falls back rather than failing:
// this is a flag default, and an explicit --timeout still overrides it.
func envDurationOrDefault(key string, fallback time.Duration) time.Duration {
	v := envalias.Get(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// envOrDefault reads an environment variable, falling back to a default.
func envOrDefault(key, fallback string) string {
	if value := envalias.Get(key); value != "" {
		return value
	}
	return fallback
}

// controllerTracing returns the tracing settings for the temporary controller:
// the collector this run exports to, with the run's headers. It returns the zero
// value when the run does not export, or when the endpoint, which can come from
// the environment, is a loopback address the bootstrap cluster cannot reach; the
// latter is logged once.
func controllerTracing(ctx context.Context, logger *slog.Logger) controllerdeploy.TracingConfig {
	run := telemetry.RunFrom(ctx)
	if run == nil {
		return controllerdeploy.TracingConfig{}
	}
	otlp, ok := run.OTLP()
	if !ok {
		return controllerdeploy.TracingConfig{}
	}
	if telemetry.RejectLoopback(otlp.Endpoint) != nil {
		logger.Warn("the controller in the bootstrap cluster cannot reach " + otlp.Endpoint + "; its spans are not exported")
		return controllerdeploy.TracingConfig{}
	}
	return controllerdeploy.TracingConfig{Endpoint: otlp.Endpoint, Insecure: otlp.Insecure, Headers: otlp.Headers}
}

// cweDeployConfig assembles everything the controller deploy needs into one value,
// from the site spec, the flags and the environment, before any of it is used.
//
// It is separate from deployAndWaitForCWE so that the two questions stay apart:
// this one is "what did the operator ask for", and everything it can get wrong
// is answered here, where the answer costs a message rather than a Kind cluster
// that then has to be torn down.
//
// clusterName is the ephemeral cluster's name, which is settled before the
// cluster is created; SPIRE is told it, and it seeds the default trust domain.
func cweDeployConfig(inv *sitespec.SiteSpec, clusterName string, disableMTLS bool, controllerImage, provisionedImage string) (controllerdeploy.Config, error) {
	cfg := controllerdeploy.DefaultConfig()
	// Flag, then variable, then the manifest default.
	if controllerImage != "" {
		cfg.CWEImage = controllerImage
	}
	if provisionedImage != "" {
		cfg.ProvisionedProviderImage = provisionedImage
	}

	// Both images default to the local build's tag from the bootstrap
	// manifest, so an empty value means the manifest lacks the entry.
	if cfg.CWEImage == "" {
		return cfg, errors.New("--nvke-controller-image and NVKECTL_NVKE_CONTROLLER_IMAGE are not set and the bootstrap manifest carries no default controller image")
	}
	cfg.NicoInfrastructure = inv.IsNico()
	access, err := capnicoAccess(inv)
	if err != nil {
		return cfg, err
	}
	cfg.CAPNICo = access
	if inv.IsNico() {
		user, password, err := sitespec.NodeLoginFromEnv()
		if err != nil {
			return cfg, err
		}
		cfg.DGXCTenant = controllerdeploy.TenantLogin{User: user, Password: password}
		cfg.ReleasesPath = inv.ResolvePath(inv.Infra.Nico.Releases)
	}
	cfg.SPIREExternalAddress = inv.Spire.ExternalAddress
	// CAPNICo comes from its own manifest, so the provisioned provider's image is not used.
	if cfg.ProvisionedProviderImage == "" && !cfg.NicoInfrastructure {
		return cfg, errors.New("--provisioned-provider-image and NVKECTL_CAPI_PROVISIONED_IMAGE are not set and the bootstrap manifest carries no default provisioned-provider image")
	}

	// SPIRE is told the Kind cluster's own name. The identity trust domain comes
	// from the site spec, including its environment override, or is derived from
	// the bootstrap cluster name when the operator did not set one.
	cfg.SPIFFEClusterName = clusterName
	if inv.Spire.TrustDomain != "" {
		cfg.SPIFFETrustDomain = inv.Spire.TrustDomain
	} else if cfg.SPIFFETrustDomain == "" {
		cfg.SPIFFETrustDomain = clusterName + ".nke.local"
	}

	// The CA SPIRE chains to, from NVKECTL_SPIRE_CA_CERT_FILE and NVKECTL_SPIRE_CA_KEY_FILE.
	cfg.SPIFFEUpstreamCACertPath = inv.Spire.CACertFile
	cfg.SPIFFEUpstreamCAKeyPath = inv.Spire.CAKeyFile

	// The allowlist of hardware allowed to attest, also from the sitespec. A
	// bootstrap is only entitled to admit machines it was told about, so this
	// is the whole of it: no node listed, no node admitted.
	cfg.TPMEKFingerprints = inv.Cluster.TPMEKThumbprints()

	// No SPIFFE means no SPIRE and no attestation, so the thumbprints go
	// unused; keep them off the config so nothing downstream acts on them.
	cfg.DisableMTLS = disableMTLS
	if disableMTLS {
		cfg.TPMEKFingerprints = nil
	}

	// How the hosts were configured when they were provisioned, which is why it
	// is in the site spec and not in the environment.
	cfg.NodeAgentPort = inv.Cluster.NodeAgentPort
	cfg.OIDCProviders = inv.Cluster.OIDCProviders

	return cfg, nil
}

// deployAndWaitForCWE installs Postgres, Temporal, SPIRE, Cluster API, and the controller onto cluster
// and polls the controller's /readyz until it reports healthy or a bounded timeout
// elapses. What to install is decided by cweDeployConfig; this only carries it
// out.
//
// The controller and provisioned-provider images are loaded into the kind cluster
// from the host's image store (Engine.LoadImage) rather than pulled from
// inside it: the cluster is ephemeral and has no pull secret. A reference
// with a registry host is pulled onto the host first when it is not cached;
// authenticating to that registry is the caller's job. A reference without a
// registry host must already be in the store (`make -C cli images`).
//
// SPIRE needs no such handling: its chart and images are public.

func deployAndWaitForCWE(
	ctx context.Context,
	engine *bootstrapengine.Engine,
	cluster *bootstrapengine.Cluster,
	cweCfg controllerdeploy.Config,
	cweRunner controllerdeploy.CommandRunner,
	logger *slog.Logger,
) error {
	for _, image := range nvkeImages(cweCfg) {
		if err := ensureImageAvailable(ctx, engine, cluster, image, logger); err != nil {
			return fmt.Errorf("loading %s into bootstrap kind cluster: %w", image, err)
		}
	}

	cwe := controllerdeploy.New(cweRunner, logger)
	logger.Info("deploying the controller onto bootstrap kind cluster", "cluster", cluster.Name, "image", cweCfg.CWEImage)
	if err := cwe.Deploy(ctx, cluster.KubeconfigPath, cweCfg); err != nil {
		return fmt.Errorf("deploying the controller: %w", err)
	}

	logger.Info("waiting for the controller to report healthy", "timeout", cweCfg.ReadyTimeout)
	if err := cwe.WaitReady(ctx, cluster.KubeconfigPath, cweCfg.ReadyTimeout); err != nil {
		return fmt.Errorf("waiting for the controller to become healthy: %w", err)
	}
	cweURL := fmt.Sprintf("http://127.0.0.1:%d", cweCfg.CWEPort)
	if err := waitForCWEHostEndpoint(ctx, cweURL, cweCfg.ReadyTimeout); err != nil {
		return fmt.Errorf("waiting for the published controller api: %w", err)
	}
	logger.Info("controller deployment healthy")
	return nil
}

// nvkeImages lists the images this repository builds that the bootstrap loads
// into the Kind cluster. The provisioned provider is among them only when it is
// the infrastructure provider in use.
func nvkeImages(cfg controllerdeploy.Config) []string {
	images := []string{cfg.CWEImage}
	if !cfg.NicoInfrastructure {
		images = append(images, cfg.ProvisionedProviderImage)
	}
	if cfg.ProfilesImage != "" {
		images = append(images, cfg.ProfilesImage)
	}
	return images
}

// capnicoAccess resolves the site spec's infra.nico into the values the
// bootstrap CAPNICo is given: the endpoint and organization from the spec, and
// the token from the file NVKECTL_NICO_CREDENTIALS names. The file is read here,
// before a Kind cluster exists, so an unreadable one fails at once rather than
// minutes in. It returns the zero value for any provider but nico.
func capnicoAccess(inv *sitespec.SiteSpec) (controllerdeploy.CAPNICoAccess, error) {
	if !inv.IsNico() {
		return controllerdeploy.CAPNICoAccess{}, nil
	}
	n := inv.Infra.Nico
	if n == nil || n.Remote == nil {
		return controllerdeploy.CAPNICoAccess{}, errors.New("the site spec selects infra.provider nico but has no infra.nico.remote")
	}
	path := sitespec.NicoCredentialsPath()
	if path == "" {
		return controllerdeploy.CAPNICoAccess{}, fmt.Errorf("%s must name the file holding the NICo token", sitespec.NicoCredentialsEnv)
	}
	nicoToken, err := sitespec.ReadNicoToken(path)
	if err != nil {
		return controllerdeploy.CAPNICoAccess{}, fmt.Errorf("%s: %w", sitespec.NicoCredentialsEnv, err)
	}
	return controllerdeploy.CAPNICoAccess{
		Endpoint: n.Remote.Endpoint,
		OrgID:    n.OrgID,
		APIName:  n.Remote.EffectiveAPIName(),
		Token:    nicoToken,
	}, nil
}

// capnicoReleaseImage returns the image the bootstrap CAPNICo runs when a
// release is given: the release's capnico-controller artifact as
// repository@digest, which is how the deploy path's render names it. It returns
// "" without a release, which leaves the manifest's own image. A release with
// provider provisioned is refused, since nothing there would read it.
func capnicoReleaseImage(inv *sitespec.SiteSpec, releasePath, arch string) (string, error) {
	if releasePath == "" {
		return "", nil
	}
	// The flag's default is empty, not the host's architecture, so the generated
	// command reference does not change with the machine that generates it.
	if arch == "" {
		arch = "linux/" + runtime.GOARCH
	}
	if !inv.IsNico() {
		return "", errors.New("--release only applies with infra.provider nico: it replaces the bootstrap CAPNICo image, and no other provider installs one")
	}
	release, err := nvkrrelease.Load(releasePath)
	if err != nil {
		return "", err
	}
	artifact, err := release.Select(controllerdeploy.CAPNICoArtifact, arch)
	if err != nil {
		return "", fmt.Errorf("--release %s: %w", releasePath, err)
	}
	return artifact.Repository() + "@" + artifact.Digest, nil
}

// waitForCWEHostEndpoint verifies the same host path that the create request
// uses. The in-cluster readiness check can pass before Kind's host-port and
// NodePort path is ready to carry traffic.
func waitForCWEHostEndpoint(ctx context.Context, baseURL string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{Timeout: 5 * time.Second}
	readyURL := strings.TrimRight(baseURL, "/") + "/readyz"
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, readyURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
				return nil
			}
			lastErr = fmt.Errorf("unexpected HTTP status %s", resp.Status)
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not become ready within %s: %w", readyURL, timeout, lastErr)
		case <-time.After(time.Second):
		}
	}
}

// requireContainerRuntime reports a missing container runtime as the missing
// prerequisite it is. Without this the first failure is kind's, several steps
// in and phrased as a cluster-listing problem, because kind execs the runtime
// itself rather than this process doing it.
func requireContainerRuntime(lookPath func(string) (string, error)) error {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if _, err := lookPath(bootstrapengine.RuntimeCLI); err != nil {
		return fmt.Errorf("%s is required to bootstrap a site but was not found on PATH: %w\n"+
			"the bootstrap cluster runs on nerdctl/containerd; Docker is not supported",
			bootstrapengine.RuntimeCLI, err)
	}
	return nil
}

// sharedCNIConfDir is the directory requireIsolatedRuntimeCNI inspects. A
// variable so tests do not read the machine running them.
var sharedCNIConfDir = bootstrapengine.SharedCNIConfDir

// requireIsolatedRuntimeCNI fails when the container runtime has already written
// network configs into dir, the node containerd's CNI directory.
//
// The bootstrap runs on the node that becomes the site. A runtime config left in
// dir is the only CNI config there between kubeadm init and the CNI plugin
// writing its own, so the first pods, CoreDNS among them, are sandboxed on the
// runtime's bridge and keep an address no pod on the cluster network can reach.
// The bootstrap now writes new configs elsewhere, but cannot move these.
func requireIsolatedRuntimeCNI(dir string) error {
	found, err := bootstrapengine.NerdctlCNIConfigs(dir)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("%s already holds %s network config(s), and this node's own containerd reads that directory for pod networking:\n  %s\n"+
		"Left there, they are the only CNI config on the node until the cluster's CNI plugin writes its own, so the first pods "+
		"(CoreDNS, node-local-dns) get addresses on the bootstrap bridge and cluster DNS never works.\n"+
		"List the bridge each file names with `grep -h '\"bridge\":' %s/nerdctl-*.conflist` before removing the files, "+
		"then delete each of those bridges with `ip link delete <bridge>` and re-run. "+
		"To keep %s out of that directory for good, set `cni_netconfpath = %q` in /etc/nerdctl/nerdctl.toml",
		dir, bootstrapengine.RuntimeCLI, strings.Join(found, "\n  "), dir,
		bootstrapengine.RuntimeCLI, bootstrapengine.IsolatedCNIConfDir)
}

// ensureImageAvailable makes sure image is present in containerd's image
// store before handing it to Engine.LoadImage. A reference with a registry
// host is pulled when it is not cached. One without must already be there;
// requireLocalImages checks that before the cluster is created.
func ensureImageAvailable(
	ctx context.Context,
	engine *bootstrapengine.Engine,
	cluster *bootstrapengine.Cluster,
	image string,
	logger *slog.Logger,
) error {
	if out, err := engine.Runner.Run(ctx, nil, bootstrapengine.RuntimeCLI, "image", "inspect", image); err != nil {
		// An absent binary is not a cache miss, and pulling cannot fix it.
		// requireContainerRuntime catches this before any of it runs; this is
		// defence in depth for a runtime that leaves PATH mid-bootstrap, and
		// keeps the message the same either way.
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("%s is required to bootstrap a site but was not found on PATH: %w\n"+
				"the bootstrap cluster runs on nerdctl/containerd; Docker is not supported",
				bootstrapengine.RuntimeCLI, err)
		}
		if !hasRegistryHost(image) {
			return missingLocalImageError(image, err, out)
		}
		logger.Info("image not cached locally; pulling", "image", image, "runtime", bootstrapengine.RuntimeCLI)
		if out, err := engine.Runner.Run(ctx, nil, bootstrapengine.RuntimeCLI, "pull", image); err != nil {
			return fmt.Errorf("%s pull %s: %w\n%s\n"+
				"if this is an authentication failure, log in to the image's registry "+
				"(e.g. `%s login nvcr.io`) and re-run", bootstrapengine.RuntimeCLI, image, err, out, bootstrapengine.RuntimeCLI)
		}
	}
	return engine.LoadImage(ctx, cluster.Name, image)
}

// hasRegistryHost reports whether an image reference names a registry: its
// first path component holds a dot or a colon, is localhost, or has an upper
// case letter (repository names are lower case, so the reference parser reads
// it as a domain). A reference without one is a local build the runtime
// cannot pull.
func hasRegistryHost(image string) bool {
	first, _, found := strings.Cut(image, "/")
	if !found {
		return false
	}
	return first == "localhost" || strings.ContainsAny(first, ".:") || strings.ToLower(first) != first
}

// requireLocalImages checks that every image without a registry host is in
// the local image store. Such an image cannot be pulled later, so this runs
// before the cluster is created.
func requireLocalImages(ctx context.Context, engine *bootstrapengine.Engine, images ...string) error {
	for _, image := range images {
		if hasRegistryHost(image) {
			continue
		}
		if out, err := engine.Runner.Run(ctx, nil, bootstrapengine.RuntimeCLI, "image", "inspect", image); err != nil {
			return missingLocalImageError(image, err, out)
		}
	}
	return nil
}

// missingLocalImageError names the build command and the flags and variables
// that replace a local build, and keeps the inspect failure. The profiles image
// is not one `make -C cli images` builds, and only its own variable sets it.
func missingLocalImageError(image string, err error, out string) error {
	if image == os.Getenv(controllerdeploy.ProfilesImageEnv) {
		return fmt.Errorf("the profiles image %s is not in the local image store (%w\n%s) and names no registry to pull it from; "+
			"build it and tag it with something other than latest, or name an image the runtime can pull in %s "+
			"(for Docker Hub, write docker.io/<org>/<name>)",
			image, err, strings.TrimSpace(out), controllerdeploy.ProfilesImageEnv)
	}
	return fmt.Errorf("image %s is not in the local image store (%w\n%s) and names no registry to pull it from; "+
		"build it with `make -C cli images`, or name an image the runtime can pull with "+
		"--nvke-controller-image / --provisioned-provider-image or NVKECTL_NVKE_CONTROLLER_IMAGE / NVKECTL_CAPI_PROVISIONED_IMAGE "+
		"(for Docker Hub, write docker.io/<org>/<name>)",
		image, err, strings.TrimSpace(out))
}

// dumpSiteSpec prints the loaded, defaulted site spec as YAML so the operator
// can see exactly what was parsed.
func dumpSiteSpec(out io.Writer, inv *sitespec.SiteSpec) error {
	y, err := yaml.Marshal(inv)
	if err != nil {
		return fmt.Errorf("marshal site spec: %w", err)
	}
	fmt.Fprintln(out, "Loaded site spec:")
	fmt.Fprint(out, string(y))
	return nil
}
