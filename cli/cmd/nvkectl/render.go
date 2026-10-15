// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/nvkrrelease"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath"
)

func newRenderCmd() *cobra.Command {
	// Internal: deploy/nvke-deploy.sh runs it with --sitespec and --release.
	cmd := &cobra.Command{
		Use:    "render",
		Short:  "Write a resolved copy of an input, ready to apply",
		Hidden: true,
	}
	cmd.AddCommand(newRenderDeployPathCmd())
	return cmd
}

type renderDeployPathOptions struct {
	deployPath string
	siteSpec   string
	release    string
	out        string
	arch       string
	runDir     string
	plainHTTP  []string
	ephemeral  bool
}

func newRenderDeployPathCmd() *cobra.Command {
	opts := &renderDeployPathOptions{}
	cmd := &cobra.Command{
		Use:   "deploy-path --deploy-path DIR (--sitespec FILE | --release FILE | both) --out DIR",
		Short: "Resolve a deploy path for a site and against an NVKR release, and write the result",
		Long: `Write a copy of a deploy path under --out, resolved for a site, against an
NVKR release, or both. The committed deploy path is never edited.

With --sitespec, the spec's controllerStack section fills in what the stack
needs from the site: architecture, NICo API and token, pull secret, tenant
login, the database password file, and optionally a mirror, image overrides
and an nktp values file. A missing database password file is created with a
generated password. Items get a site-values.yaml; template items become
manifests. SPIRE gets the bootstrap CA by file reference: helm reads it at
install time. The stack joins the bootstrap's trust domain: spire.trustDomain,
set once before the bootstrap, else the default both commands share, with a
warning. Paths under controllerStack resolve from the spec's directory;
spire.caCertFile and spire.caKeyFile from the working directory, as
"nvkectl bootstrap site" reads them.

--release takes the nvkr.dev/release-description/v1 JSON that "nvkr release
export --reference <release>" writes. The export describes the release and
verifies nothing: run "nvkr release verify" on the same reference first.
Each item's release.yaml names its artifacts: chart.artifact (default: the
item name), chart.imageArtifact, chart.profilesArtifact, or a manifest
item's artifact. Charts are pulled by digest into charts/<item>/; images go
into release-images.yaml or a kustomization.yaml; images.txt lists them. A
chart the release lacks stays as committed. The release decides the version
of every chart it carries, with a warning when release.yaml pins another;
the pin decides only for a chart the release lacks. --arch selects
artifacts (default: the site spec's
controllerStack.arch, else this machine's). --nvkr-run-dir reads a
completed NVKR Run or a directory of OCI layouts instead of a registry.

--out holds the site's secrets: it is created 0700 and is for one install.
An --out under a shared temp directory is refused; use a directory only you
can read, for example ~/.local/state/nvke/renders/<site>. The command ends
by naming the files under --out that hold a credential; delete --out after
the install. --out gets a .gitignore that ignores everything, so a
repository does not pick it up by accident. Committed chart paths are
written absolute, so --out can live anywhere. Problems are reported all at
once and nothing is left on failure. Then install it:
deploy/nvke-deploy.sh install --deploy-path OUT.`,
		Example: `  nvkectl render deploy-path --deploy-path deploy/nico --sitespec sitespec.yaml --release release.json --out ~/.local/state/nvke/renders/my-site
  nvkectl render deploy-path --deploy-path deploy/nico --sitespec sitespec.yaml --release release.json --nvkr-run-dir .nvkr/runs/demo-01 --out ~/.local/state/nvke/renders/my-site`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRenderDeployPath(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.deployPath, "deploy-path", "", "The committed deploy path (required)")
	cmd.Flags().StringVarP(&opts.siteSpec, "sitespec", "f", "", "The site spec whose controllerStack section describes the site")
	cmd.Flags().StringVar(&opts.release, "release", "", "The NVKR release description, JSON")
	cmd.Flags().StringVar(&opts.out, "out", "", "Directory to write; must not exist and must not be under a shared temp directory (required)")
	cmd.Flags().StringVar(&opts.arch, "arch", "linux/"+runtime.GOARCH, "Architecture to select release artifacts for; default: the site spec's controllerStack.arch, else this machine's")
	cmd.Flags().StringVar(&opts.runDir, "nvkr-run-dir", "", "A completed NVKR Run or a directory of OCI layouts to read artifacts from instead of a registry")
	cmd.Flags().StringSliceVar(&opts.plainHTTP, "plain-http-registries", nil, "Registry hosts served over plain HTTP; nothing is probed")
	cmd.Flags().BoolVar(&opts.ephemeral, "ephemeral", false, "The caller removes --out when it is done; the summary does not ask the operator to")
	for _, flag := range []string{"deploy-path", "out"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func runRenderDeployPath(cmd *cobra.Command, opts *renderDeployPathOptions) error {
	if opts.siteSpec == "" && opts.release == "" {
		return errors.New("pass --sitespec, --release or both")
	}
	if opts.runDir != "" && opts.release == "" {
		return errors.New("--nvkr-run-dir holds a release's artifacts; pass --release")
	}
	ro := deploypath.RenderOptions{DeployPath: opts.deployPath, Out: opts.out, Log: cmd.ErrOrStderr(), AllowTempOut: os.Getenv("NVKECTL_RENDER_ALLOW_TEMP_OUT") == "1"}
	var inputs []string
	if opts.siteSpec != "" {
		spec, err := sitespec.Load(opts.siteSpec)
		if err != nil {
			return err
		}
		ro.Site = spec
		inputs = append(inputs, spec.ClusterName)
	}
	if opts.release != "" {
		release, err := nvkrrelease.Load(opts.release)
		if err != nil {
			return err
		}
		if ro.Site != nil && ro.Site.ControllerStack != nil && ro.Site.ControllerStack.Arch != "" {
			siteArch := "linux/" + ro.Site.ControllerStack.Arch
			if !cmd.Flags().Changed("arch") {
				opts.arch = siteArch
			} else if opts.arch != siteArch {
				return fmt.Errorf("--arch %s, but the site spec's controllerStack.arch is %s", opts.arch, ro.Site.ControllerStack.Arch)
			}
		}
		ro.Release, ro.ReleasePath, ro.Arch, ro.RunDir = release, opts.release, opts.arch, opts.runDir
		ro.Registry = deploypath.Registries(opts.plainHTTP)
		inputs = append(inputs, "release "+release.Reference)
	}
	result, err := deploypath.Render(cmd.Context(), ro)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "rendered %s for %s into %s\n", opts.deployPath, strings.Join(inputs, " and "), opts.out)
	if len(result.Images) > 0 {
		var refs []string
		for _, img := range result.Images {
			refs = append(refs, img.Reference)
		}
		fmt.Fprintf(out, "images: %s\n", strings.Join(refs, ", "))
	}
	if opts.ephemeral {
		fmt.Fprintf(out, "%s holds this site's credentials: %s; it is removed when the install ends.\n", opts.out, strings.Join(result.Credentials, ", "))
	} else {
		fmt.Fprintf(out, "%s holds this site's credentials: %s. Delete it after the install.\n", opts.out, strings.Join(result.Credentials, ", "))
	}
	return nil
}
