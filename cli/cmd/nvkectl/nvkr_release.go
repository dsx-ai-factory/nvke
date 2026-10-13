// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/nvkrrelease"
)

// newInspectCmd groups internal diagnostics: hidden from the top-level help,
// text output, and no registry or cluster access. Nothing under deploy/ calls
// them; the render does the work. The verb-noun shape is the one every
// nvkectl command has.
func newInspectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "inspect",
		Short:  "Internal diagnostics: print one view of a file nvkectl reads",
		Hidden: true,
		Long: `Internal diagnostics. These subcommands read a file nvkectl consumes and
print one view of it as plain text. Nothing under deploy/ calls them, and
operators are not expected to; they exist to look at an input when a render
refuses it. Nothing here touches a registry or a cluster.`,
	}
	cmd.AddCommand(newInspectNVKRReleaseCmd())
	return cmd
}

type inspectNVKRReleaseOptions struct {
	artifacts bool
	kind      string
	meta      bool
	name      string
	arch      string
}

func newInspectNVKRReleaseCmd() *cobra.Command {
	opts := &inspectNVKRReleaseOptions{}
	cmd := &cobra.Command{
		Use:   "nvkr-release FILE (--artifacts [--kind KIND] | --meta | --select NAME --arch OS/ARCH)",
		Short: "Print one view of the release description nvkr release verify prints",
		Long: `Read the nvkr.dev/release/v4 document nvkr release verify prints, bare or
inside its verification result, and print one view of it:

  --artifacts    one line per artifact in document order, tab separated:
                 name, kind, reference, digest, arch. --kind keeps one kind.
  --meta         SUBJECT, LANE, SCOPE and CONTENT_SUBJECT, one per line,
                 each single-quoted for eval.
  --select NAME --arch OS/ARCH
                 the artifact of that name for that architecture, as
                 name, reference, digest, tab separated. NVKR publishes some
                 outputs per architecture (NAME and NAME-arm64) and some as
                 multi-arch indexes under one name; an artifact carrying
                 arch is taken only for that architecture. No artifact of
                 the name prints nothing and exits 0; one published only
                 for other architectures, or without a reference, is an
                 error.`,
		Example: `  nvkectl inspect nvkr-release release.json --artifacts --kind helm-oci
  nvkectl inspect nvkr-release release.json --select cwe --arch linux/arm64`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspectNVKRRelease(cmd, args[0], opts)
		},
	}
	cmd.Flags().BoolVar(&opts.artifacts, "artifacts", false, "Print one tab-separated line per artifact")
	cmd.Flags().StringVar(&opts.kind, "kind", "", "With --artifacts: print only artifacts of this kind (oci, helm-oci, executable, disk-image, ...)")
	cmd.Flags().BoolVar(&opts.meta, "meta", false, "Print the metadata as shell assignments")
	cmd.Flags().StringVar(&opts.name, "select", "", "Print the artifact of this name for --arch")
	cmd.Flags().StringVar(&opts.arch, "arch", "", "With --select: the architecture to select for, linux/amd64 or linux/arm64")
	return cmd
}

func runInspectNVKRRelease(cmd *cobra.Command, path string, opts *inspectNVKRReleaseOptions) error {
	modes := 0
	for _, on := range []bool{opts.artifacts, opts.meta, opts.name != ""} {
		if on {
			modes++
		}
	}
	if modes != 1 {
		return errors.New("pass exactly one of --artifacts, --meta or --select NAME")
	}
	if opts.kind != "" && !opts.artifacts {
		return errors.New("--kind goes with --artifacts")
	}
	if (opts.arch != "") != (opts.name != "") {
		return errors.New("--select NAME and --arch go together")
	}
	r, err := nvkrrelease.Load(path)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	switch {
	case opts.artifacts:
		for _, a := range r.OfKind(opts.kind) {
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", a.Name, a.Kind, a.Reference, a.Digest, a.Platform())
		}
	case opts.meta:
		for _, kv := range [][2]string{
			{"SUBJECT", r.Subject},
			{"LANE", r.Lane},
			{"SCOPE", r.Scope},
			{"CONTENT_SUBJECT", r.ContentSubject},
		} {
			fmt.Fprintf(out, "%s=%s\n", kv[0], shellQuote(kv[1]))
		}
	case opts.name != "":
		a, err := r.Select(opts.name, opts.arch)
		if errors.Is(err, nvkrrelease.ErrNotPublished) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		fmt.Fprintf(out, "%s\t%s\t%s\n", a.Name, a.Reference, a.Digest)
	}
	return nil
}

// shellQuote single-quotes a value for eval. Every value is quoted, even a
// safe one.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
