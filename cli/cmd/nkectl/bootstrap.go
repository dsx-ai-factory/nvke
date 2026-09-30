// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/cwedeploy"
	// Aliased: this file has a local variable named engine.
	bootstrapengine "github.com/dsx-ai-factory/nke/cli/internal/bootstrap/engine"
	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/etcdkey"
	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/sitecluster"
	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/spireentrydump"
	"github.com/dsx-ai-factory/nke/cli/internal/clients"
	"github.com/dsx-ai-factory/nke/cli/internal/config"
)

const (
	// siteSubcommand is the name of the "bootstrap site" subcommand.
	siteSubcommand = "site"

	// registryAPIKeyEnv is the environment variable consulted for the registry
	// credential when it is not passed on the command line. Keeping the secret
	// out of the site spec file lets the file be committed and shared safely.
	registryAPIKeyEnv = "REGISTRY_API_KEY" //nolint:gosec // G101: this is an env var name, not a credential
)

// newBootstrapCmd is the "bootstrap" command group. Site bootstrap is the only
// subcommand today; the group leaves room for further bootstrap targets.
func newBootstrapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap NKE clusters from a declarative site spec",
	}
	cmd.AddCommand(newBootstrapSiteCmd())
	return cmd
}

// bootstrapSiteOptions holds the flag values for "bootstrap site".
type bootstrapSiteOptions struct {
	siteSpec       string
	dryRun         bool
	registryAPIKey string
	// diagnosticsDir is where a failed bootstrap writes what the cluster had
	// to say for itself, before it is torn down.
	diagnosticsDir    string
	keepBootstrapKind bool

	// disableMTLS stands the site up without SPIFFE, for bring-up debugging.
	disableMTLS bool

	// clusterTimeout bounds the wait for CWE to finish provisioning the
	// cluster, from the create call to every node having joined.
	clusterTimeout time.Duration

	// kubeconfigOut is where the finished cluster's admin kubeconfig is
	// written. Empty defaults to <clusterName>.kubeconfig in the working
	// directory.
	kubeconfigOut string

	// kubeconfigTimeout bounds the wait for tenant-access to publish the
	// finished cluster's kubeconfig Secret. Zero uses cwedeploy's default.
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

	// cweRunner overrides the CommandRunner cwedeploy uses (helm/kubectl).
	// Nil in production; tests set it to a fake for the same reason as
	// bootstrapRunner above.
	cweRunner cwedeploy.CommandRunner

	// dumpSpireEntries overrides the SPIRE bootstrap-resource export. Nil uses
	// a Kubernetes client built from the bootstrap kubeconfig; tests replace it
	// to avoid contacting a real cluster.
	dumpSpireEntries func(context.Context, string, string) (int, error)

	// lookPath resolves the container runtime binary. Nil uses exec.LookPath;
	// tests substitute it so they do not require a real nerdctl installation,
	// the same seam bootstrapRunner and cweRunner provide for the commands.
	lookPath func(string) (string, error)

	// dialContext is how the pre-create probe reaches each site spec host.
	// Nil uses a net.Dialer; tests substitute it so no real host is dialled.
	dialContext func(ctx context.Context, network, address string) (net.Conn, error)
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
		Short: "Bootstrap a new NKE site controller from a site spec file",
		Long: `Bootstrap a vanilla Kubernetes cluster from a set of pre-provisioned nodes
described by the site spec file passed with -f/--sitespec.

The site spec file is loaded and validated first. On any invalid or missing
field the command fails immediately with a clear error, before any workflow is
dispatched or any node is touched.

See the bundled examples/sitespec.yaml and docs/sitespec.md for the
site spec file format.`,
		Example: `  # Validate and bootstrap from a site spec file
  nkectl bootstrap site -f sitespec.yaml

  # Validate only — load, check, and print the site spec without dispatching
  nkectl bootstrap site -f sitespec.yaml --dry-run

  # Supply the registry credential without putting it in the file
  REGISTRY_API_KEY=... nkectl bootstrap site -f sitespec.yaml`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBootstrapSite(cmd.OutOrStdout(), opts)
		},
	}

	cmd.Flags().StringVarP(&opts.siteSpec, "sitespec", "f", "",
		"Path to the site spec YAML file (required)")
	_ = cmd.MarkFlagRequired("sitespec")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false,
		"Load and validate the site spec and print it, but do not dispatch any workflow")
	cmd.Flags().StringVar(&opts.registryAPIKey, "registry-api-key", "",
		"Container registry credential (overrides registry.apiKey and the "+registryAPIKeyEnv+" env var)")
	cmd.Flags().StringVar(&opts.diagnosticsDir, "diagnostics-dir",
		envOrDefault("NKECTL_BOOTSTRAP_DIAGNOSTICS_DIR", "cluster-diagnostics"),
		"Directory a failed bootstrap writes cluster diagnostics to before tearing the cluster down. "+
			"Overrides NKECTL_BOOTSTRAP_DIAGNOSTICS_DIR.")
	cmd.Flags().BoolVar(&opts.disableMTLS, "disable-mtls", false,
		"Stand the site up without SPIFFE: no SPIRE, and CWE dials node agents in plaintext. "+
			"The agents must be running with --disable-mtls too. Development only.")
	cmd.Flags().BoolVar(&opts.keepBootstrapKind, "keep-bootstrap-cluster", false,
		"Leave the ephemeral bootstrap Kind cluster running after the command exits, for debugging")
	cmd.Flags().DurationVar(&opts.clusterTimeout, "timeout",
		envDurationOrDefault("NKECTL_BOOTSTRAP_CLUSTER_TIMEOUT", 30*time.Minute),
		"How long to wait for the cluster to finish provisioning, which includes "+
			"every control plane node joining. Overrides NKECTL_BOOTSTRAP_CLUSTER_TIMEOUT.")
	cmd.Flags().StringVar(&opts.kubeconfigOut, "kubeconfig-out", "",
		"Where to write the finished cluster's admin kubeconfig "+
			"(default: <clusterName>.kubeconfig in the working directory)")
	cmd.Flags().StringVar(&opts.encryptionKeyOut, "encryption-key-out", "",
		"Where to write the finished cluster's etcd encryption key, as a Secret manifest "+
			"(default: <clusterName>-etcd-encryption-key.json in the working directory)")

	return cmd
}

func runBootstrapSite(out io.Writer, opts *bootstrapSiteOptions) (err error) {
	logger := slog.Default().With("component", "bootstrap-site")

	// Step 1: load and validate the sitespec. Fail fast on any problem.
	inv, err := sitespec.Load(opts.siteSpec)
	if err != nil {
		return err
	}
	logger.Info("loaded site spec", "path", opts.siteSpec, "cluster", inv.ClusterName)

	// An explicit --registry-api-key flag wins over the value the loader
	// resolved (the site spec file or the REGISTRY_API_KEY env var). The
	// credential is never logged or printed.
	if opts.registryAPIKey != "" {
		inv.Registry.APIKey = opts.registryAPIKey
	}

	if err := dumpSiteSpec(out, inv); err != nil {
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

	// Step 2: stand up the ephemeral bootstrap Kind cluster CWE will run on.
	// It is normally torn down on exit. After site-cluster creation starts, a
	// later failure retains it so the operator does not lose the controllers and
	// credentials needed for recovery.
	ctx := context.Background()
	cfg := bootstrapengine.DefaultConfig()
	cweCfg, err := cweDeployConfig(inv, cfg.ClusterName, opts.disableMTLS)
	if err != nil {
		return err
	}

	// The CWE API has to be reachable from this host: this command deploys it
	// and then calls it. A host port mapping rather than the node container's
	// own address, because that address is not routable from the host on
	// every platform this runs on.
	cfg.ExtraPortMappings = append(cfg.ExtraPortMappings, bootstrapengine.PortMapping{
		ContainerPort: cweCfg.CWEPort,
		HostPort:      cweCfg.CWEPort,
	})
	if len(cweCfg.TPMEKFingerprints) > 0 {
		cfg.ExtraPortMappings = append(cfg.ExtraPortMappings, bootstrapengine.PortMapping{
			ContainerPort: cwedeploy.SPIREServerNodePort,
			HostPort:      cwedeploy.SPIREServerNodePort,
		})
	}

	engine := bootstrapengine.New(opts.bootstrapRunner, logger)

	// Last checks before anything is built. Each refuses a state a fresh
	// bootstrap cannot run over, names it, and says what to do; each costs a
	// message here and a Kind cluster plus half an hour anywhere later. None of
	// them changes anything: what an earlier run left behind is left for the
	// operator to deal with, and for a later resume to pick up.
	//
	// A bootstrap cluster still running is the strongest sign of an earlier
	// attempt, and may hold that attempt's encryption key.
	if err := requireNoBootstrapCluster(ctx, engine, cfg.ClusterName); err != nil {
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
	if err := requireNoControlPlane(ctx, inv.Cluster.Nodes, opts.dialContext); err != nil {
		return err
	}
	// The two images this repository builds are side-loaded, not pulled. A
	// build that was never made fails here, before the cluster exists.
	if err := requireLocalImages(ctx, engine, cweCfg.CWEImage, cweCfg.ProvisionedProviderImage); err != nil {
		return err
	}

	logger.Info("creating ephemeral bootstrap kind cluster", "cluster", cfg.ClusterName)
	cluster, err := engine.Create(ctx, cfg)
	if err != nil {
		return fmt.Errorf("standing up bootstrap kind cluster: %w", err)
	}
	siteClusterCreated := false
	defer func() {
		// Collect diagnostics before anything is torn down: this cluster is
		// removed on every exit path, so this is the only moment the evidence
		// still exists. Only on failure — a successful bootstrap has nothing
		// to explain.
		if err != nil {
			dir := opts.diagnosticsDir
			logger.Info("bootstrap failed; collecting cluster diagnostics", "dir", dir)
			cwedeploy.New(opts.cweRunner, logger).
				DumpDiagnostics(ctx, cluster.KubeconfigPath, dir)
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
		if deleteErr := engine.Delete(ctx, cluster.Name); deleteErr != nil {
			logger.Error("failed to tear down bootstrap kind cluster",
				"cluster", cluster.Name, "err", deleteErr)
		}
	}()

	// Step 3: deploy Postgres, Temporal, SPIRE, Cluster API, and CWE onto the bootstrap
	// cluster and wait for CWE to report healthy on /readyz before going any
	// further — there is nothing to dispatch a workflow to otherwise.
	if err := deployAndWaitForCWE(ctx, engine, cluster, cweCfg, opts.cweRunner, logger); err != nil {
		return err
	}

	// Step 4: create the cluster. CWE owns everything from here — it drives
	// kubeadm on each host through nke-agent and reconciles the Cluster API
	// objects that the provisioned infrastructure provider binds to the
	// site spec's hosts.
	siteClusterCreated, err = createSiteCluster(ctx, cluster, inv, opts, cweCfg.CWEPort, logger)
	if err != nil && siteClusterCreated {
		// The advice below ends with the command that deletes the bootstrap
		// cluster. When the key was not saved, that cluster holds the only copy
		// the operator does not have, and the error says so instead.
		var capture *keyCaptureError
		if errors.As(err, &capture) {
			return fmt.Errorf("%w (bootstrap cluster %q kept; its kubeconfig is %s)",
				err, cluster.Name, cluster.KubeconfigPath)
		}
		// The env prefix is not optional: kind resolves a cluster through the
		// provider that created it, so without it the command reports the
		// cluster as missing rather than deleting it.
		deleteCommand := fmt.Sprintf(
			"KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name %s", cluster.Name,
		)
		return fmt.Errorf("%w; site cluster creation has started, so bootstrap cluster %q was retained for recovery "+
			"(kubeconfig: %s); delete it after recovery with `%s`",
			err, cluster.Name, cluster.KubeconfigPath, deleteCommand)
	}
	if err == nil && !cweCfg.DisableMTLS {
		count, dumpErr := dumpSpireBootstrapEntries(
			ctx, opts, cluster.KubeconfigPath, filepath.Dir(siteKubeconfigPath(inv.ClusterName, opts)),
		)
		if dumpErr != nil {
			return fmt.Errorf("writing SPIRE bootstrap resources: %w", dumpErr)
		}
		logger.Info("wrote SPIRE bootstrap resources", "clusterStaticEntries", count)
	}
	return err
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

// createSiteCluster posts the cluster to CWE, waits for it to come up, and
// writes out its kubeconfig. The boolean reports whether CWE accepted the
// create request, after which callers must preserve the management cluster on
// failure so the in-progress site cluster remains recoverable.
//
// The CWE client is built here rather than being passed in because its address
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
		return false, fmt.Errorf("building the CWE client for %s: %w", cweURL, err)
	}
	created, err := sitecluster.Create(ctx, client, inv, logger)
	if err != nil {
		return false, err
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
		ctx, client, created.ID, len(inv.Cluster.Nodes), time.Until(deadline), logger,
	); err != nil {
		return true, err
	}
	logger.Info("all control plane nodes joined", "cluster", created.Name, "nodes", len(inv.Cluster.Nodes))

	// The kubeconfig first, so a failure in the key capture below still leaves
	// the operator holding the admin credential of the cluster that was built.
	kubeconfigPath, err := writeTenantKubeconfig(ctx, bootstrapCluster, created.Name, opts, logger)
	if err != nil {
		return true, err
	}

	// Every control-plane node holds the key now, and this is the last moment
	// the copy CWE keeps is reachable: it lives in the bootstrap cluster, which
	// goes when the command does.
	keyPath := ""
	if inv.Cluster.EtcdEncryptionEnabled() {
		keyPath = etcdKeyPath(opts, inv.ClusterName)
		if err := saveEtcdEncryptionKey(ctx, bootstrapCluster, created.ID, inv.ClusterName,
			keyPath, kubeconfigPath, opts, logger); err != nil {
			return true, err
		}
	}

	fmt.Fprintf(os.Stdout, "\nCluster %q is ready. Talk to it with:\n  kubectl --kubeconfig %s get nodes\n",
		created.Name, kubeconfigPath)
	if keyPath != "" {
		fmt.Fprintf(os.Stdout, "Its etcd encryption key is at %s. Move that file off this host "+
			"and keep it with the cluster's other credentials.\n", keyPath)
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

// saveEtcdEncryptionKey copies the key Secret CWE holds for the cluster out of
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
			recover:        cwedeploy.EtcdEncryptionKeyCommand(bootstrapCluster.KubeconfigPath, clusterID, path),
			err:            err,
		}
	}

	readCtx, cancel := context.WithTimeout(ctx, etcdKeyReadTimeout)
	defer cancel()
	data, err := cwedeploy.New(opts.cweRunner, logger).
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

// writeTenantKubeconfig saves the finished cluster's admin kubeconfig beside
// the operator and returns its path.
//
// 0o600: it is an admin credential for the cluster that was just created. A
// plain write, unlike the etcd key's: a kubeconfig can be issued again from
// the cluster, so losing or overwriting this file is recoverable.
func writeTenantKubeconfig(
	ctx context.Context,
	bootstrapCluster *bootstrapengine.Cluster,
	clusterName string,
	opts *bootstrapSiteOptions,
	logger *slog.Logger,
) (string, error) {
	path := siteKubeconfigPath(clusterName, opts)

	kubeconfig, err := cwedeploy.New(opts.cweRunner, logger).
		TenantKubeconfig(ctx, bootstrapCluster.KubeconfigPath, clusterName, opts.kubeconfigTimeout)
	if err != nil {
		return "", fmt.Errorf("reading the kubeconfig of cluster %q: %w", clusterName, err)
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

// requireNoBootstrapCluster refuses to start while a bootstrap cluster from an
// earlier run is still running.
//
// A leftover is kept on purpose — by --keep-bootstrap-cluster, or by a run that
// failed after building its site cluster. In the second case it may hold the
// only copy of that cluster's etcd encryption key that is not on a node, and
// the natural response to "already exists" is the delete that loses it. The
// message leads with saving the key. Nothing is deleted or reused here.
func requireNoBootstrapCluster(ctx context.Context, engine *bootstrapengine.Engine, name string) error {
	exists, err := engine.Exists(ctx, name)
	if err != nil {
		return fmt.Errorf("checking for a bootstrap cluster from an earlier run: %w", err)
	}
	if !exists {
		return nil
	}
	return fmt.Errorf("bootstrap cluster %q from an earlier run is still running. If that run built an encrypted "+
		"site cluster and did not save its key, the key is in this cluster: get its kubeconfig with "+
		"`KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get kubeconfig --name %s`, save any etcd-encryption-key-* Secret "+
		"in namespace %s, then delete it with `KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name %s` and re-run",
		name, name, cwedeploy.Namespace, name)
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

// envOrDefault reads an environment variable, falling back to a default.
func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// cweDeployConfig assembles everything the CWE deploy needs into one value,
// from the site spec and the environment, before any of it is used.
//
// It is separate from deployAndWaitForCWE so that the two questions stay apart:
// this one is "what did the operator ask for", and everything it can get wrong
// is answered here, where the answer costs a message rather than a Kind cluster
// that then has to be torn down.
//
// clusterName is the ephemeral cluster's name, which is settled before the
// cluster is created; SPIRE is told it, and it seeds the default trust domain.
func cweDeployConfig(inv *sitespec.SiteSpec, clusterName string, disableMTLS bool) (cwedeploy.Config, error) {
	cfg := cwedeploy.DefaultConfig()

	// Both images default to the local build's tag from the bootstrap
	// manifest, so an empty value means the manifest lacks the entry.
	if cfg.CWEImage == "" {
		return cfg, errors.New("NKECTL_CWE_IMAGE is not set and the bootstrap manifest carries no default CWE image")
	}
	if cfg.ProvisionedProviderImage == "" {
		return cfg, errors.New("NKECTL_CAPI_PROVISIONED_IMAGE is not set and the bootstrap manifest carries no default provisioned-provider image")
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

	// The CA SPIRE chains to, straight from the site spec, which is where an
	// operator declares what this bootstrap is entitled to trust.
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

	return cfg, nil
}

// deployAndWaitForCWE installs Postgres, Temporal, SPIRE, Cluster API, and CWE onto cluster
// and polls CWE's /readyz until it reports healthy or a bounded timeout
// elapses. What to install is decided by cweDeployConfig; this only carries it
// out.
//
// The CWE and provisioned-provider images are loaded into the kind cluster
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
	cweCfg cwedeploy.Config,
	cweRunner cwedeploy.CommandRunner,
	logger *slog.Logger,
) error {
	for _, image := range []string{cweCfg.CWEImage, cweCfg.ProvisionedProviderImage} {
		if err := ensureImageAvailable(ctx, engine, cluster, image, logger); err != nil {
			return fmt.Errorf("loading %s into bootstrap kind cluster: %w", image, err)
		}
	}

	cwe := cwedeploy.New(cweRunner, logger)
	logger.Info("deploying cwe onto bootstrap kind cluster", "cluster", cluster.Name, "image", cweCfg.CWEImage)
	if err := cwe.Deploy(ctx, cluster.KubeconfigPath, cweCfg); err != nil {
		return fmt.Errorf("deploying cwe: %w", err)
	}

	logger.Info("waiting for cwe to report healthy", "timeout", cweCfg.ReadyTimeout)
	if err := cwe.WaitReady(ctx, cluster.KubeconfigPath, cweCfg.ReadyTimeout); err != nil {
		return fmt.Errorf("waiting for cwe to become healthy: %w", err)
	}
	cweURL := fmt.Sprintf("http://127.0.0.1:%d", cweCfg.CWEPort)
	if err := waitForCWEHostEndpoint(ctx, cweURL, cweCfg.ReadyTimeout); err != nil {
		return fmt.Errorf("waiting for the published cwe api: %w", err)
	}
	logger.Info("cwe deployment healthy")
	return nil
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

// missingLocalImageError names the build command and the two variables that
// replace a local build, and keeps the inspect failure.
func missingLocalImageError(image string, err error, out string) error {
	return fmt.Errorf("image %s is not in the local image store (%w\n%s) and names no registry to pull it from; "+
		"build it with `make -C cli images`, or set NKECTL_CWE_IMAGE / NKECTL_CAPI_PROVISIONED_IMAGE "+
		"to an image the runtime can pull (for Docker Hub, write docker.io/<org>/<name>)",
		image, err, strings.TrimSpace(out))
}

// dumpSiteSpec prints the loaded, defaulted site spec as YAML so the operator
// can see exactly what was parsed. The registry credential is redacted.
func dumpSiteSpec(out io.Writer, inv *sitespec.SiteSpec) error {
	redacted := *inv
	if redacted.Registry.APIKey != "" {
		redacted.Registry.APIKey = "***redacted***"
	}
	y, err := yaml.Marshal(&redacted)
	if err != nil {
		return fmt.Errorf("marshal site spec: %w", err)
	}
	fmt.Fprintln(out, "Loaded site spec:")
	fmt.Fprint(out, string(y))
	return nil
}
