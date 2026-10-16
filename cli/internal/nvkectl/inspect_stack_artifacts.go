// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath"
	"github.com/dsx-ai-factory/nvke/cli/internal/stackartifacts"
)

type inspectStackArtifactsOptions struct {
	artifacts  bool
	kind       string
	meta       bool
	name       string
	arch       string
	bindings   bool
	deployPath string
}

func newInspectStackArtifactsCmd() *cobra.Command {
	opts := &inspectStackArtifactsOptions{}
	cmd := &cobra.Command{
		Use:   "stack-artifacts FILE (--artifacts [--kind KIND] | --meta | --select NAME --arch OS/ARCH | --bindings --deploy-path DIR --arch OS/ARCH)",
		Short: "Print one view of a stack artifacts file",
		Long: `Read a nvke.nvidia.com/stack-artifacts/v1 file the way the render does and
print one view of it:

  --artifacts    one line per artifact in document order, tab separated:
                 name, kind, reference, digest, platforms (comma separated),
                 chart version, layout (absolute, or empty). --kind keeps
                 one kind.
  --meta         STACK_ARTIFACTS (the file's absolute path), SOURCE_REFERENCE,
                 QUALIFICATION, ARTIFACTS (count), LAYOUTS (count),
                 LAYOUTS_MISSING (the artifacts whose layout is missing or
                 does not list their digest, space separated) and
                 PULL_HOSTS (the registry hosts of the artifacts without a
                 layout, space separated), one per line, each single-quoted
                 for eval.
  --select NAME --arch OS/ARCH
                 the artifact of that name for that architecture, as name,
                 reference, digest, layout, tab separated. A native artifact
                 names its platform; a multi-arch image lists its platforms
                 in its image index; per-architecture artifacts may be
                 published as NAME and NAME-arm64. An artifact naming
                 platforms is taken only for one of them. No artifact of
                 the name prints nothing and exits 0; one published only for
                 other architectures, or without a reference, is an error.
  --bindings --deploy-path DIR --arch OS/ARCH
                 one line per artifact the deploy path binds, in item order,
                 tab separated: item, role (chart, image, profiles-image),
                 artifact name, the artifact chosen ("-" when none), state
                 (bound, missing, committed, other-arch:<platforms>,
                 invalid:<reason>); then one line
                 "unused<TAB><name>" per artifact no item binds. The render
                 makes the same selections.

Nothing here reads a registry. The layouts are read only for --meta.`,
		Example: `  nvkectl inspect stack-artifacts stack-artifacts.json --artifacts --kind helm-oci
  nvkectl inspect stack-artifacts stack-artifacts.json --select cwe --arch linux/arm64
  nvkectl inspect stack-artifacts stack-artifacts.json --bindings --deploy-path deploy/nico --arch linux/arm64`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspectStackArtifacts(cmd, args[0], opts)
		},
	}
	cmd.Flags().BoolVar(&opts.artifacts, "artifacts", false, "Print one tab-separated line per artifact")
	cmd.Flags().StringVar(&opts.kind, "kind", "", "With --artifacts: print only artifacts of this kind (oci, helm-oci, ...)")
	cmd.Flags().BoolVar(&opts.meta, "meta", false, "Print the file's facts as shell assignments")
	cmd.Flags().StringVar(&opts.name, "select", "", "Print the artifact of this name for --arch")
	cmd.Flags().StringVar(&opts.arch, "arch", "", "With --select or --bindings: the architecture, linux/amd64 or linux/arm64")
	cmd.Flags().BoolVar(&opts.bindings, "bindings", false, "Print what --deploy-path binds, resolved for --arch")
	cmd.Flags().StringVar(&opts.deployPath, "deploy-path", "", "With --bindings: the deploy path whose items bind the artifacts")
	return cmd
}

func runInspectStackArtifacts(cmd *cobra.Command, path string, opts *inspectStackArtifactsOptions) error {
	modes := 0
	for _, on := range []bool{opts.artifacts, opts.meta, opts.name != "", opts.bindings} {
		if on {
			modes++
		}
	}
	if modes != 1 {
		return errors.New("pass exactly one of --artifacts, --meta, --select NAME or --bindings")
	}
	if opts.kind != "" && !opts.artifacts {
		return errors.New("--kind goes with --artifacts")
	}
	if opts.name != "" && opts.arch == "" {
		return errors.New("--select NAME and --arch go together")
	}
	if opts.bindings && (opts.arch == "" || opts.deployPath == "") {
		return errors.New("--bindings needs --deploy-path DIR and --arch")
	}
	if opts.arch != "" && opts.name == "" && !opts.bindings {
		return errors.New("--arch goes with --select or --bindings")
	}
	if opts.deployPath != "" && !opts.bindings {
		return errors.New("--deploy-path goes with --bindings")
	}
	r, err := stackartifacts.Load(path)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	switch {
	case opts.artifacts:
		for _, a := range r.OfKind(opts.kind) {
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Kind, a.Reference, a.Digest, strings.Join(a.Platforms(), ","), a.ChartVersion, a.Layout)
		}
	case opts.meta:
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		layouts := 0
		hosts := map[string]bool{}
		for _, a := range r.Artifacts {
			if a.Layout != "" {
				layouts++
			} else if host := a.Host(); host != "" {
				hosts[host] = true
			}
		}
		var missing []string
		for _, err := range r.CheckLayouts() {
			// "artifact <name>: ..." as CheckLayouts words it.
			name := strings.TrimPrefix(err.Error(), "artifact ")
			missing = append(missing, name[:strings.Index(name, ":")])
		}
		var pullHosts []string
		for host := range hosts {
			pullHosts = append(pullHosts, host)
		}
		sort.Strings(pullHosts)
		var reference, qualification string
		if r.Source != nil {
			reference, qualification = r.Source.Reference, r.Source.DeclaredQualification
		}
		for _, kv := range [][2]string{
			{"STACK_ARTIFACTS", abs},
			{"SOURCE_REFERENCE", reference},
			{"QUALIFICATION", qualification},
			{"ARTIFACTS", strconv.Itoa(len(r.Artifacts))},
			{"LAYOUTS", strconv.Itoa(layouts)},
			{"LAYOUTS_MISSING", strings.Join(missing, " ")},
			{"PULL_HOSTS", strings.Join(pullHosts, " ")},
		} {
			fmt.Fprintf(out, "%s=%s\n", kv[0], shellQuote(kv[1]))
		}
	case opts.name != "":
		a, err := r.Select(opts.name, opts.arch)
		if errors.Is(err, stackartifacts.ErrNotProvided) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", a.Name, a.Reference, a.Digest, a.Layout)
	case opts.bindings:
		if _, err := r.Select("", opts.arch); err != nil && strings.Contains(err.Error(), "is not one of") {
			return err
		}
		bindings, unused, err := deploypath.Bindings(opts.deployPath, r, opts.arch)
		if err != nil {
			return err
		}
		for _, b := range bindings {
			// "-" keeps the field: a shell read collapses adjacent tabs.
			resolved := b.Resolved
			if resolved == "" {
				resolved = "-"
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", b.Item, b.Role, b.Artifact, resolved, b.State)
		}
		for _, name := range unused {
			fmt.Fprintf(out, "unused\t%s\n", name)
		}
	}
	return nil
}
