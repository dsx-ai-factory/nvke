// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/nvkrrelease"
	"github.com/dsx-ai-factory/nvke/cli/internal/catalogtemplate"
)

// catalogTargets maps the --target names to the template's overlays.
var catalogTargets = map[string]string{"seed": "nvke-seed", "site": "nvke-site"}

type renderCatalogOptions struct {
	template string
	target   string
	release  string
	arch     string
	out      string
}

func newRenderCatalogCmd() *cobra.Command {
	opts := &renderCatalogOptions{}
	cmd := &cobra.Command{
		Use:   "catalog --catalog-template DIR --target seed|site [--release FILE] --out DIR",
		Short: "Render the AICR catalog template into a catalog directory",
		Long: `Render the AICR catalog template (aicr-catalog/ in the NVKE repository) into a
directory "aicr recipe --data" and "aicr bundle --data" take as is.

The template carries NVKE's default chart locations and versions. --release is an
NVKR release description (nvkr release export); every output it carries for a
component the template names replaces that component's chart location and version,
and the CAPNICo image is pinned to its digest for --arch. Without --release the
defaults stand, which works only for a target whose charts all have a default
location. The release is not verified: run "nvkr release verify" on the same
reference first.

--target seed is the reduced stack "nvkectl bootstrap site" stands up on its
ephemeral cluster; site is the site controller cluster.`,
		Example: `  nvkectl render catalog --catalog-template aicr-catalog --target site --release release.json --out /tmp/catalog`,
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return runRenderCatalog(cmd, opts) },
	}
	cmd.Flags().StringVar(&opts.template, "catalog-template", "", "The AICR catalog template directory")
	cmd.Flags().StringVar(&opts.target, "target", "", "The overlay to render for: seed or site")
	cmd.Flags().StringVar(&opts.release, "release", "", "NVKR release description whose outputs replace the template's defaults")
	cmd.Flags().StringVar(&opts.arch, "arch", "linux/"+runtime.GOARCH, "The nodes' architecture, linux/amd64 or linux/arm64; default: this machine's")
	cmd.Flags().StringVar(&opts.out, "out", "", "Directory to write the catalog to; must not exist or be empty")
	for _, f := range []string{"catalog-template", "target", "out"} {
		_ = cmd.MarkFlagRequired(f)
	}
	return cmd
}

func runRenderCatalog(cmd *cobra.Command, opts *renderCatalogOptions) error {
	overlay, ok := catalogTargets[opts.target]
	if !ok {
		return fmt.Errorf("--target %s is not one of seed, site", opts.target)
	}
	if !slices.Contains(nvkrrelease.Archs, opts.arch) {
		return fmt.Errorf("--arch %s is not one of %s", opts.arch, strings.Join(nvkrrelease.Archs, ", "))
	}
	if opts.release == "" && cmd.Flags().Changed("arch") {
		return errors.New("--arch selects the architecture of a --release artifact; pass --release too, or drop --arch")
	}
	o := catalogtemplate.Options{TemplateDir: opts.template, OutDir: opts.out, Target: overlay, Arch: opts.arch}
	if opts.release != "" {
		release, err := nvkrrelease.Load(opts.release)
		if err != nil {
			return err
		}
		o.Release = release
	}
	res, err := catalogtemplate.Render(o)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "rendered the %s catalog into %s\n", opts.target, opts.out)
	for _, line := range res.Replaced {
		fmt.Fprintf(out, "  from the release: %s\n", line)
	}
	return nil
}
