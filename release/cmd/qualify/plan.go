// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nke/release/internal/qualification"
	"github.com/dsx-ai-factory/nke/release/internal/resolve"
)

func planCmd() *cobra.Command {
	var (
		subject    string
		version    string
		overrides  string
		scope      string
		output     string
		root       string
		policyAt   string
		registryAt string
	)

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Resolve what to qualify for one subject",
		Long: `Resolve what to qualify for one subject.

Reads the subject's qualification spec, the release registry and the release
policy, resolves every component and dependency to a version, and expands the
scope's selector over the stream list. The result is one file naming every
combination and every stream.

An absent --version means the subject's highest version.

--overrides replaces what a component or dependency would have resolved
to, as comma-separated name=value pairs, the format the e2e suite's -e2e.versions takes.
The names it replaces are listed in the plan under overrides. An image or a
bootloader such as os-image-amd64 is a dependency, and its override is a URL.

An image or a bootloader URL is asked for with HEAD and must answer 200. The
bucket is private, so a missing artifact and a network that is not allowed both
answer 403, and the check can confirm that something exists but not that it does
not, so plan needs a network that can reach it.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if root == "" {
				var err error
				if root, err = repoRoot(); err != nil {
					return err
				}
			}
			if policyAt == "" {
				policyAt = filepath.Join(root, "release", "policy.yaml")
			}
			if registryAt == "" {
				registryAt = filepath.Join(root, "release", "registry.yaml")
			}

			p, err := qualification.LoadPolicy(policyAt)
			if err != nil {
				return err
			}
			reg, err := qualification.LoadRegistry(registryAt)
			if err != nil {
				return err
			}
			resolvers, err := buildResolvers(reg, root)
			if err != nil {
				return err
			}
			chosen, err := qualification.ParseOverrides(overrides)
			if err != nil {
				return fmt.Errorf("--overrides: %w", err)
			}

			planner := &qualification.Planner{
				Policy:    p,
				Registry:  reg,
				Resolvers: resolvers,
				Root:      root,
				Overrides: chosen,
			}

			result, err := planner.Plan(subject, version, scope)
			if err != nil {
				return err
			}
			return write(cmd, output, result)
		},
	}

	f := cmd.Flags()
	f.StringVar(&subject, "subject", "", "subject to qualify (required)")
	f.StringVar(&version, "version", "", "version of the subject to qualify, empty means its highest version")
	f.StringVar(&overrides, "overrides", "", "values to use in place of what resolves, as name=value pairs separated by commas")
	// full is the default because it is the scope that is never wrong. A
	// narrower default would silently cover less of the matrix than the
	// person running it believed.
	f.StringVar(&scope, "scope", "full", "how much of the matrix to cover")
	f.StringVar(&output, "output", "-", "where to write the plan, - is stdout")
	f.StringVar(&root, "root", "", "repository root (default: the enclosing git checkout)")
	f.StringVar(&policyAt, "policy", "", "release policy file (default: <root>/release/policy.yaml)")
	f.StringVar(&registryAt, "registry", "", "release registry file (default: <root>/release/registry.yaml)")

	_ = cmd.MarkFlagRequired("subject")
	return cmd
}

// buildResolvers makes one resolver for each registry component. This is the one
// place that names a strategy, which is what lets the planner be tested with
// fakes.
func buildResolvers(reg *qualification.Registry, root string) (map[string]resolve.Resolver, error) {
	resolvers := make(map[string]resolve.Resolver, len(reg.Spec.Components))
	for _, c := range reg.Spec.Components {
		r, err := resolve.New(c.Resolve.Strategy, reg.StrategyConfig(c), root)
		if err != nil {
			return nil, fmt.Errorf("component %s: %w", c.Name, err)
		}
		resolvers[c.Name] = r
	}
	return resolvers, nil
}

// write emits the plan as indented JSON, to stdout or to a file.
func write(cmd *cobra.Command, path string, result *qualification.Plan) error {
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the plan: %w", err)
	}
	b = append(b, '\n')

	if path == "-" {
		_, err = cmd.OutOrStdout().Write(b)
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// repoRoot finds the checkout the command was run from, so that neither
// --root nor --policy has to be passed by hand.
func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("finding the repository root: %w (pass --root)", err)
	}
	return strings.TrimSpace(string(out)), nil
}
