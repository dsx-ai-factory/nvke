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

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
	"github.com/dsx-ai-factory/nvke/release/internal/resolve"
)

func planCmd() *cobra.Command {
	var (
		subject     string
		version     string
		overrides   string
		scope       string
		output      string
		root        string
		policyAt    string
		registryAt  string
		resolution  string
		checkExists bool
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

--resolution chooses how the components are resolved, and names the strategy that
will answer. "gitTag" takes each from its own release tags, which is how a release
is qualified. "commitDescribe" describes each at the commit the subject's version
points at, which is how a beta is qualified: a beta tag carries no component tags,
so the tags would name the newest release rather than what was built there.

The commit is derived from the version being qualified rather than given. Either
way, the components that are not built here, such as the Kubernetes and OVN
packages an image carries, are read from that commit's files, and an override
still names a tag.

--check-exists asks for each location this resolves and requires it to be
published. It is off by default, so planning reaches nothing but git, and the
plan is checked as a whole afterwards instead. With it on, note that the bucket
is private, so a missing artifact and a network that is not allowed both answer
403, and the check can confirm that something exists but not that it does not.`,
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

			if resolution != resolve.StrategyGitTag && resolution != resolve.StrategyCommitDescribe {
				return fmt.Errorf("--resolution %q is neither %q nor %q", resolution, resolve.StrategyGitTag, resolve.StrategyCommitDescribe)
			}

			p, err := qualification.LoadPolicy(policyAt)
			if err != nil {
				return err
			}
			reg, err := qualification.LoadRegistry(registryAt)
			if err != nil {
				return err
			}
			resolvers, err := buildResolvers(reg, root, resolve.Options{CheckExists: checkExists})
			if err != nil {
				return err
			}
			chosen, err := qualification.ParseOverrides(overrides)
			if err != nil {
				return fmt.Errorf("--overrides: %w", err)
			}

			planner := &qualification.Planner{
				Policy:          p,
				Registry:        reg,
				Resolvers:       resolvers,
				Root:            root,
				Overrides:       chosen,
				ResolveAtCommit: resolution == resolve.StrategyCommitDescribe,
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
	// The values are the strategies the plan records under each component, so
	// what a run asked for and what answered are the same word.
	f.StringVar(&resolution, "resolution", resolve.StrategyGitTag,
		`how to resolve the components, "`+resolve.StrategyGitTag+`" or "`+resolve.StrategyCommitDescribe+`"`)
	f.BoolVar(&checkExists, "check-exists", false, "require every location this resolves to be published already")

	_ = cmd.MarkFlagRequired("subject")
	_ = cmd.RegisterFlagCompletionFunc("resolution", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{resolve.StrategyGitTag, resolve.StrategyCommitDescribe}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

// buildResolvers makes one resolver for each registry component. This is the one
// place that names a strategy, which is what lets the planner be tested with
// fakes.
func buildResolvers(reg *qualification.Registry, root string, opts resolve.Options) (map[string]resolve.Resolver, error) {
	resolvers := make(map[string]resolve.Resolver, len(reg.Spec.Components))
	for _, c := range reg.Spec.Components {
		r, err := resolve.New(c.Resolve.Strategy, reg.StrategyConfig(c), root, opts)
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
