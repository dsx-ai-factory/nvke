// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/artifact"
	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

func verifyCmd() *cobra.Command {
	var (
		planAt string
		wait   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check that everything a plan names is published",
		Long: `Check that everything a plan names is published.

Reads a plan and asks for each location it records: a manifest from the registry
for an oci:// one, and a HEAD for an https:// one. An entry that names no
location, such as a Kubernetes version read from a lock file, has nothing to ask
for and is skipped.

A run that resolved a release expects its artifacts to be there already, so the
default is to ask once and fail. --wait is for a run that resolved a commit the
publishers may still be building, where a missing artifact means not yet rather
than never.

This runs before any hardware is taken, so an artifact that was never published
fails here rather than an hour into a run.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan, err := readPlan(planAt)
			if err != nil {
				return err
			}

			locations := locations(plan)
			if len(locations) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "the plan names no locations to check")
				return nil
			}

			checker := artifact.Checker{Auth: artifact.EnvAuth(registryCredentials...)}
			return checker.WaitFor(cmd.Context(), cmd.OutOrStdout(), locations, wait, artifact.DefaultPoll)
		},
	}

	f := cmd.Flags()
	f.StringVar(&planAt, "plan", "plan.json", "the plan to check")
	f.DurationVar(&wait, "wait", 0, "how long to keep asking for an artifact that is not published yet")
	return cmd
}

// registryCredentials are the registries a run pulls from and how it
// authenticates to each, matching what the qualification workflow logs in with.
// Both hold private repositories, so neither answers anonymously.
//
// A registry not listed here, and one listed whose variables are unset, is asked
// anonymously and reports its own 401 if it will not answer.
var registryCredentials = []artifact.Credential{
	// $oauthtoken is NGC's literal username, not a variable reference.
	{Host: "nvcr.io", Username: "$oauthtoken", PasswordEnv: "NGC_API_KEY"},
	{Host: "ghcr.io", UsernameEnv: "GITHUB_ACTOR", PasswordEnv: "GITHUB_TOKEN"},
}

// readPlan loads a plan, from a file or from stdin when the name is "-".
func readPlan(path string) (*qualification.Plan, error) {
	file := os.Stdin
	if path != "-" {
		var err error
		if file, err = os.Open(path); err != nil {
			return nil, fmt.Errorf("reading the plan: %w", err)
		}
		defer file.Close()
	}

	var plan qualification.Plan
	if err := json.NewDecoder(file).Decode(&plan); err != nil {
		return nil, fmt.Errorf("reading the plan: %w", err)
	}
	return &plan, nil
}

// locations are every location the plan names, each with the names that wanted
// it, sorted so two runs of the same plan report in the same order.
//
// One location can be named by more than one entry, because a combination and
// its streams can resolve to the same image, and asking twice for the same thing
// tells nobody anything new.
func locations(plan *qualification.Plan) []artifact.Location {
	wanted := map[string][]string{}
	add := func(versions []qualification.ComponentVersion) {
		for _, v := range versions {
			if v.URL == "" || !artifact.IsScheme(v.URL) {
				continue
			}
			if !contains(wanted[v.URL], v.Name) {
				wanted[v.URL] = append(wanted[v.URL], v.Name)
			}
		}
	}

	for _, c := range plan.Combinations {
		add(c.Components)
		add(c.Dependencies)
		for _, s := range c.Streams {
			add(s.Dependencies)
		}
	}

	out := make([]artifact.Location, 0, len(wanted))
	for url, names := range wanted {
		sort.Strings(names)
		out = append(out, artifact.Location{URL: url, Wanted: names})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
