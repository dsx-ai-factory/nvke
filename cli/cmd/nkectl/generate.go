// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nke/cli/internal/oidckubeconfig"
	"github.com/dsx-ai-factory/nke/cli/internal/util"
)

// newGenerateCmd is the "generate" command group.
func newGenerateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate NKE configuration files",
	}
	cmd.AddCommand(newGenerateSiteSpecCmd())
	cmd.AddCommand(newGenerateOIDCKubeconfigCmd())
	return cmd
}

// generateSiteSpecOptions holds the flag values for "generate sitespec".
type generateSiteSpecOptions struct {
	nodes string
	sets  []string
}

func newGenerateSiteSpecCmd() *cobra.Command {
	opts := &generateSiteSpecOptions{}

	cmd := &cobra.Command{
		Use:   "sitespec <cluster-name>",
		Short: "Generate a site spec file for a tier-0 cluster",
		Long: `Write a complete site spec for "nkectl bootstrap site" to standard output.

The hosts come from a CSV so a real site can be described without repeating a
flag per machine. The output needs no editing: every field is validated before
anything is written, so a file this command produces is a file bootstrap site
accepts.

The CSV needs a header naming its columns, in any order. name and ip are
required; tpm_ek_thumbprint is optional per row and enables TPM attestation for
that host.

  name,ip,tpm_ek_thumbprint
  cp-01,10.10.50.2,0000000000000000000000000000000000000000000000000000000000000000
  cp-02,10.10.50.3,
  cp-03,10.10.50.4,

Anything the CSV does not cover is set with --set, using the same field paths
the file itself uses. Run with an unknown path to list them.

The generated file depends only on the arguments given: unlike bootstrap site,
this command ignores the inventory environment variables, so a site spec never
carries a value nobody typed.`,
		Example: `  # Generate a site spec and bootstrap from it
  nkectl generate sitespec my-tier0-site --nodes hosts.csv \
    --set cluster.controlPlaneVIP=10.10.50.10 > sitespec.yaml
  nkectl bootstrap site -f sitespec.yaml

  # Override fields the CSV does not carry
  nkectl generate sitespec my-tier0-site --nodes hosts.csv \
    --set cluster.controlPlaneVIP=10.10.50.10 \
    --set cluster.podCidr=10.244.0.0/16 > sitespec.yaml`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGenerateSiteSpec(cmd, args[0], opts)
		},
	}

	cmd.Flags().StringVar(&opts.nodes, "nodes", "",
		"Path to the node CSV, or '-' to read standard input (required)")
	_ = cmd.MarkFlagRequired("nodes")
	cmd.Flags().StringArrayVar(&opts.sets, "set", nil,
		"Set a field, as path=value. Repeatable. Accepts: "+strings.Join(sitespec.SettablePaths(), ", "))

	return cmd
}

func runGenerateSiteSpec(cmd *cobra.Command, clusterName string, opts *generateSiteSpecOptions) error {
	nodes, err := readNodesCSV(cmd, opts.nodes)
	if err != nil {
		return err
	}

	spec, err := sitespec.Build(clusterName, nodes, opts.sets)
	if err != nil {
		return err
	}

	rendered, err := spec.Render()
	if err != nil {
		return err
	}

	_, err = cmd.OutOrStdout().Write(rendered)
	return err
}

// readNodesCSV reads the node list from path, or from standard input when path
// is "-", so the CSV can be piped from whatever already knows the hosts.
func readNodesCSV(cmd *cobra.Command, path string) ([]sitespec.Node, error) {
	if path == "-" {
		return sitespec.ParseNodesCSV("standard input", cmd.InOrStdin())
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open node CSV: %w", err)
	}
	defer func() { _ = file.Close() }()

	return sitespec.ParseNodesCSV(path, file)
}

type generateOIDCKubeconfigOptions struct {
	kubeconfig  string
	issuerURL   string
	clientID    string
	extraScopes []string
	outputFile  string
}

func newGenerateOIDCKubeconfigCmd() *cobra.Command {
	opts := &generateOIDCKubeconfigOptions{}

	cmd := &cobra.Command{
		Use:   "oidc-kubeconfig",
		Short: "Generate an oidc-login kubeconfig from an input kubeconfig",
		Long: `Write an oidc-login kubeconfig from an input kubeconfig.

The cluster server and certificate authority come from the input file's current
context. The input user is not copied. The generated user is an exec plugin:

  kubectl oidc-login get-token --oidc-issuer-url ... --oidc-client-id ...

Nothing is sent to the cluster. After OIDC is enabled on the API server, the
file can be passed to config set-site --kubeconfig.

By default the kubeconfig is printed to stdout. Use --output-file to write it
to a file instead.`,
		Example: `  nkectl generate oidc-kubeconfig \
    --kubeconfig ./cluster.kubeconfig \
    --oidc-issuer-url https://idp.example/realms/nke \
    --oidc-client-id nke-cli \
    --output-file ./oidc.kubeconfig

  nkectl config set-site --name my-tier0-site \
    --url https://controller.my-tier0-site.example.com \
    --kubeconfig ./oidc.kubeconfig`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGenerateOIDCKubeconfig(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "Path to the input kubeconfig (required)")
	cmd.Flags().StringVar(&opts.issuerURL, "oidc-issuer-url", "", "OIDC issuer URL (required)")
	cmd.Flags().StringVar(&opts.clientID, "oidc-client-id", "", "OIDC client ID (required)")
	cmd.Flags().StringArrayVar(&opts.extraScopes, "oidc-extra-scope", nil, "Extra OIDC scope. Repeatable.")
	cmd.Flags().StringVar(&opts.outputFile, "output-file", "", "Write kubeconfig to file instead of stdout")
	_ = cmd.MarkFlagRequired("kubeconfig")
	_ = cmd.MarkFlagRequired("oidc-issuer-url")
	_ = cmd.MarkFlagRequired("oidc-client-id")

	return cmd
}

func runGenerateOIDCKubeconfig(cmd *cobra.Command, opts *generateOIDCKubeconfigOptions) error {
	data, err := oidckubeconfig.Generate(opts.kubeconfig, oidckubeconfig.Options{
		IssuerURL:   opts.issuerURL,
		ClientID:    opts.clientID,
		ExtraScopes: opts.extraScopes,
	})
	if err != nil {
		return err
	}

	if opts.outputFile != "" {
		if err := util.WritePrivateFile(opts.outputFile, data); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "kubeconfig written to %s\n", opts.outputFile)
		return nil
	}

	_, err = cmd.OutOrStdout().Write(data)
	return err
}
