// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/nke/cli/internal/bootstrapengine"
	"github.com/NVIDIA/nke/cli/internal/cwedeploy"
	"github.com/NVIDIA/nke/cli/internal/inventory"
)

const (
	// siteSubcommand is the name of the "bootstrap site" subcommand.
	siteSubcommand = "site"

	// registryAPIKeyEnv is the environment variable consulted for the registry
	// credential when it is not passed on the command line. Keeping the secret
	// out of the inventory file lets the file be committed and shared safely.
	registryAPIKeyEnv = "REGISTRY_API_KEY" //nolint:gosec // G101: this is an env var name, not a credential
)

// newBootstrapCmd is the "bootstrap" command group. Site bootstrap is the only
// subcommand today; the group leaves room for further bootstrap targets.
func newBootstrapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap NKE clusters from a declarative inventory",
	}
	cmd.AddCommand(newBootstrapSiteCmd())
	return cmd
}

// bootstrapSiteOptions holds the flag values for "bootstrap site".
type bootstrapSiteOptions struct {
	inventory      string
	dryRun         bool
	registryAPIKey string
	// identityRegistryAPIKey is separate because the SPIRE chart's registry
	// account need not be the bootstrap image's; see Registry.IdentityAPIKey.
	identityRegistryAPIKey string
	provider               string
	keepBootstrapKind      bool

	// bootstrapRunner overrides the CommandRunner the bootstrap engine uses.
	// Nil in production (bootstrapengine.New defaults to the real exec-backed
	// runner); tests set it to a fake so this command can be exercised
	// without a real kind binary or container runtime.
	bootstrapRunner bootstrapengine.CommandRunner

	// cweRunner overrides the CommandRunner cwedeploy uses (helm/kubectl).
	// Nil in production; tests set it to a fake for the same reason as
	// bootstrapRunner above.
	cweRunner cwedeploy.CommandRunner
}

func newBootstrapSiteCmd() *cobra.Command {
	opts := &bootstrapSiteOptions{}

	cmd := &cobra.Command{
		Use:   siteSubcommand,
		Short: "Bootstrap a new NKE site controller from an inventory file",
		Long: `Bootstrap a vanilla Kubernetes cluster from a set of pre-provisioned nodes
described by the inventory file passed with -i/--inventory.

The inventory file is loaded and validated first. On any invalid or missing
field the command fails immediately with a clear error, before any workflow is
dispatched or any node is touched.

See the bundled examples/inventory.yaml and docs/inventory-spec.md for the
inventory file format.`,
		Example: `  # Validate and bootstrap from an inventory file
  nkectl bootstrap site -i inventory.yaml

  # Validate only — load, check, and print the inventory without dispatching
  nkectl bootstrap site -i inventory.yaml --dry-run

  # Supply the registry credential without putting it in the file
  REGISTRY_API_KEY=... nkectl bootstrap site -i inventory.yaml`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runBootstrapSite(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.inventory, "inventory", "i", "",
		"Path to the inventory YAML file (required)")
	_ = cmd.MarkFlagRequired("inventory")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false,
		"Load and validate the inventory and print it, but do not dispatch any workflow")
	cmd.Flags().StringVar(&opts.registryAPIKey, "registry-api-key", "",
		"Container registry credential (overrides registry.apiKey and the "+registryAPIKeyEnv+" env var)")
	cmd.Flags().StringVar(&opts.identityRegistryAPIKey, "identity-registry-api-key", "",
		"Credential for the registry holding the SPIRE chart and its server image "+
			"(overrides registry.identityApiKey and the IDENTITY_REGISTRY_API_KEY env var). "+
			"Defaults to the bootstrap registry credential.")
	cmd.Flags().StringVar(&opts.provider, "bootstrap-provider", "",
		"Container runtime front end for the ephemeral bootstrap Kind cluster: "+
			string(bootstrapengine.ProviderNerdctl)+" (default) or "+string(bootstrapengine.ProviderDocker)+
			" (rollback path). Overrides NKECTL_BOOTSTRAP_PROVIDER.")
	cmd.Flags().BoolVar(&opts.keepBootstrapKind, "keep-bootstrap-cluster", false,
		"Leave the ephemeral bootstrap Kind cluster running after the command exits, for debugging")

	return cmd
}

func runBootstrapSite(opts *bootstrapSiteOptions) error {
	logger := slog.Default().With("component", "bootstrap-site")

	// Step 1: load and validate the inventory. Fail fast on any problem.
	inv, err := inventory.Load(opts.inventory)
	if err != nil {
		return err
	}
	logger.Info("loaded inventory", "path", opts.inventory, "cluster", inv.ClusterName)

	// An explicit --registry-api-key flag wins over the value the loader
	// resolved (the inventory file or the REGISTRY_API_KEY env var). The
	// credential is never logged or printed.
	if opts.registryAPIKey != "" {
		inv.Registry.APIKey = opts.registryAPIKey
	}
	if opts.identityRegistryAPIKey != "" {
		inv.Registry.IdentityAPIKey = opts.identityRegistryAPIKey
	}

	if err := dumpInventory(inv); err != nil {
		return err
	}

	if opts.dryRun {
		fmt.Fprintln(os.Stdout, "\nDry run: inventory is valid. No workflow dispatched.")
		return nil
	}

	// Step 2: stand up the ephemeral bootstrap Kind cluster CWE will run on.
	// Torn down unconditionally on the way out, including on every error path
	// below — nothing here is meant to outlive this command invocation.
	ctx := context.Background()
	cfg := bootstrapengine.DefaultConfig()
	if opts.provider != "" {
		cfg.Provider = bootstrapengine.Provider(opts.provider)
	}

	engine := bootstrapengine.New(opts.bootstrapRunner, logger)
	logger.Info("creating ephemeral bootstrap kind cluster",
		"cluster", cfg.ClusterName, "provider", cfg.Provider)
	cluster, err := engine.Create(ctx, cfg)
	if err != nil {
		return fmt.Errorf("standing up bootstrap kind cluster: %w", err)
	}
	defer func() {
		if opts.keepBootstrapKind {
			logger.Info("--keep-bootstrap-cluster set: leaving the bootstrap kind cluster running",
				"cluster", cluster.Name, "kubeconfig", cluster.KubeconfigPath)
			return
		}
		logger.Info("tearing down bootstrap kind cluster", "cluster", cluster.Name)
		if err := engine.Delete(ctx, cluster.Name, cluster.Provider); err != nil {
			logger.Error("failed to tear down bootstrap kind cluster",
				"cluster", cluster.Name, "err", err)
		}
	}()

	// Step 3: deploy Postgres, Temporal and CWE onto the bootstrap cluster and
	// wait for CWE to report healthy on /readyz before going any further —
	// there is nothing to dispatch a workflow to otherwise.
	if err := deployAndWaitForCWE(ctx, engine, cluster, opts.cweRunner, logger); err != nil {
		return err
	}

	// Step 4+ (subsequent tickets): dispatch the init/join/untaint workflows
	// against the now-healthy CWE. Stubbed for now so the command is runnable
	// end-to-end while the rest of the bootstrap core is ported.
	return errors.New("bootstrap dispatch is not yet implemented; re-run with --dry-run to validate the inventory")
}

// deployAndWaitForCWE installs Postgres, Temporal and CWE onto cluster and
// polls CWE's /readyz until it reports healthy or a bounded timeout elapses.
//
// The CWE and profiles images are loaded into the kind cluster directly
// (bootstrapengine's existing Engine.LoadImage, via `kind load docker-image`)
// rather than pulled from inside the cluster: the cluster is ephemeral and
// has no image pull secret configured, and this way authenticating to
// whatever registry hosts them is the caller's problem, not nkectl's —
// `docker login` once on the host, same as any other pull, before running
// this command.
//
// CWE reads osProfiles/metadata.yaml during schema migration, so it
// crash-loops without a profiles tree.
func deployAndWaitForCWE(
	ctx context.Context,
	engine *bootstrapengine.Engine,
	cluster *bootstrapengine.Cluster,
	cweRunner cwedeploy.CommandRunner,
	logger *slog.Logger,
) error {
	cweCfg := cwedeploy.DefaultConfig()
	if cweCfg.CWEImage == "" {
		return errors.New("NKECTL_CWE_IMAGE is not set; it must name the CWE image to deploy " +
			"(e.g. nvcr.io/<org>/controller:edge — see .github/workflows/image-nvcr.yml)")
	}

	if cweCfg.ProfilesImage == "" {
		return errors.New("NKECTL_CWE_PROFILES_IMAGE is not set; it must name an image " +
			"holding the OS profiles tree at /profiles")
	}

	for _, image := range []string{cweCfg.CWEImage, cweCfg.ProfilesImage} {
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
	logger.Info("cwe deployment healthy")
	return nil
}

// ensureImageAvailable makes sure image is present in the local container
// runtime's image store before handing it to Engine.LoadImage, pulling it if
// it is not already cached from a previous run.
//
// The CLI follows cluster.Provider, because that is the store kind loads
// from. A nerdctl host need not have Docker at all.
func ensureImageAvailable(
	ctx context.Context,
	engine *bootstrapengine.Engine,
	cluster *bootstrapengine.Cluster,
	image string,
	logger *slog.Logger,
) error {
	cli := string(cluster.Provider)
	if _, err := engine.Runner.Run(ctx, nil, cli, "image", "inspect", image); err != nil {
		logger.Info("cwe image not cached locally; pulling", "image", image, "runtime", cli)
		if out, err := engine.Runner.Run(ctx, nil, cli, "pull", image); err != nil {
			return fmt.Errorf("%s pull %s: %w\n%s\n"+
				"if this is an authentication failure, log in to the image's registry "+
				"(e.g. `%s login nvcr.io`) and re-run", cli, image, err, out, cli)
		}
	}
	return engine.LoadImage(ctx, cluster.Name, cluster.Provider, image)
}

// dumpInventory prints the loaded, defaulted inventory as YAML so the operator
// can see exactly what was parsed. The registry credential is redacted.
func dumpInventory(inv *inventory.Inventory) error {
	redacted := *inv
	if redacted.Registry.APIKey != "" {
		redacted.Registry.APIKey = "***redacted***"
	}
	out, err := yaml.Marshal(&redacted)
	if err != nil {
		return fmt.Errorf("marshal inventory: %w", err)
	}
	fmt.Fprintln(os.Stdout, "Loaded inventory:")
	fmt.Fprint(os.Stdout, string(out))
	return nil
}
