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
	inventory         string
	dryRun            bool
	registryAPIKey    string
	provider          string
	keepBootstrapKind bool

	// bootstrapRunner overrides the CommandRunner the bootstrap engine uses.
	// Nil in production (bootstrapengine.New defaults to the real exec-backed
	// runner); tests set it to a fake so this command can be exercised
	// without a real kind binary or container runtime.
	bootstrapRunner bootstrapengine.CommandRunner
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
	cmd.Flags().StringVar(&opts.provider, "bootstrap-provider", "",
		"Container runtime front end for the ephemeral bootstrap Kind cluster: "+
			string(bootstrapengine.ProviderNerdctl)+" (default) or "+string(bootstrapengine.ProviderDocker)+
			" (rollback path, see NKX-14787). Overrides NKECTL_BOOTSTRAP_PROVIDER.")
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

	// Step 3+ (subsequent tickets): deploy CWE onto the bootstrap cluster and
	// dispatch the init/join/untaint workflows. Stubbed for now so the
	// command is runnable end-to-end while the rest of the bootstrap core is
	// ported.
	return errors.New("bootstrap dispatch is not yet implemented; re-run with --dry-run to validate the inventory")
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
