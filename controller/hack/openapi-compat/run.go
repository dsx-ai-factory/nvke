// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// defaultSpec is the working-tree specification, relative to the controller
	// module, which is where the Makefile target runs from.
	defaultSpec = "api/openapi.yaml"
	// defaultSpecInRepo is the same file addressed from the repository root,
	// which is how `git show <ref>:<path>` must name it.
	defaultSpecInRepo = "controller/api/openapi.yaml"
	// defaultWaivers holds the accepted breaking changes. See README.md.
	defaultWaivers = "api/openapi-compat-waivers.yaml"
	// defaultTagGlob matches the tags controller-api-tag.yml publishes. The
	// digit classes keep release-candidate and other suffixed tags out: a
	// baseline has to be a stable, consumable version.
	defaultTagGlob = "controller/api/v[0-9]*.[0-9]*.[0-9]*"
	// defaultDiffer is the change engine. It is invoked with `go run`, pinned
	// by --differ-version, so nothing has to be installed and the classification
	// cannot drift with whatever is on PATH.
	defaultDiffer = "github.com/pb33f/openapi-changes"
)

func run(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("openapi-compat", flag.ContinueOnError)
	spec := flags.String("spec", defaultSpec, "Working-tree specification to check")
	specInRepo := flags.String("spec-in-repo", defaultSpecInRepo, "Path of that specification from the repository root, used to read the baseline out of a tag")
	baseline := flags.String("baseline", "", "Git ref to compare against; empty selects the newest tag matching --tag-glob")
	tagGlob := flags.String("tag-glob", defaultTagGlob, "Tag pattern the baseline is selected from")
	waivers := flags.String("waivers", defaultWaivers, "YAML file of accepted breaking changes")
	differ := flags.String("differ", defaultDiffer, "Go module path of the change engine")
	differVersion := flags.String("differ-version", "", "Version of --differ to run; required unless --report is given")
	reportPath := flags.String("report", "", "Read an existing engine report instead of running the engine")
	reportOut := flags.String("report-out", "", "Also write the engine report here, for a CI artifact")
	flags.Usage = func() {
		out := flags.Output()
		if _, err := fmt.Fprint(out, `Usage: openapi-compat --differ-version <version>

Compares the working-tree OpenAPI specification against the newest published
controller/api version tag and fails on any breaking change that is not waived
in `+defaultWaivers+`.

Run it through the Makefile, which supplies the pinned engine version:

    make -C controller api-compat-check

Fails, rather than passing, when the baseline cannot be resolved (a shallow
clone with no tags), when either specification cannot be parsed, or when the
engine writes a report this tool does not recognize.

`); err != nil {
			return
		}
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	rules, err := loadWaivers(*waivers)
	if err != nil {
		return err
	}

	rep, resolved, err := gatherReport(ctx, *reportPath, *reportOut, reportRequest{
		spec:          *spec,
		specInRepo:    *specInRepo,
		baseline:      *baseline,
		tagGlob:       *tagGlob,
		differ:        *differ,
		differVersion: *differVersion,
	})
	if err != nil {
		return err
	}

	result := evaluate(rep.Changes, rules)
	writeSummary(stdout, resolved, *spec, rep, result)

	switch {
	case len(result.Unwaived) > 0 && len(result.Unused) > 0:
		return fmt.Errorf("%d unwaived breaking change(s), and %d waiver(s) that match nothing", len(result.Unwaived), len(result.Unused))
	case len(result.Unwaived) > 0:
		return fmt.Errorf("%d unwaived breaking change(s) against %s", len(result.Unwaived), resolved)
	case len(result.Unused) > 0:
		return fmt.Errorf("%d waiver(s) in %s match nothing; remove them", len(result.Unused), *waivers)
	}
	return nil
}

// gatherReport returns the engine's report, and the baseline it was produced
// against. With --report the report is read from disk and the baseline is
// reported as the literal "(report supplied)", because this tool did not
// resolve one.
func gatherReport(ctx context.Context, reportPath, reportOut string, req reportRequest) (*report, string, error) {
	if reportPath != "" {
		raw, err := os.ReadFile(reportPath)
		if err != nil {
			return nil, "", fmt.Errorf("reading report %s: %w", reportPath, err)
		}
		rep, err := parseReport(raw)
		if err != nil {
			return nil, "", err
		}
		return rep, "(report supplied)", nil
	}

	raw, resolved, err := req.produce(ctx)
	if err != nil {
		return nil, "", err
	}
	if reportOut != "" {
		if err := os.MkdirAll(filepath.Dir(reportOut), 0o755); err != nil {
			return nil, "", fmt.Errorf("creating the report directory: %w", err)
		}
		if err := os.WriteFile(reportOut, raw, 0o600); err != nil {
			return nil, "", fmt.Errorf("writing report %s: %w", reportOut, err)
		}
	}
	rep, err := parseReport(raw)
	if err != nil {
		return nil, "", err
	}
	return rep, resolved, nil
}

// writeSummary prints what changed and what the policy made of it. It is always
// printed, including on success: the additive total is the number a reviewer
// wants beside "no breaking changes", and a silent pass teaches nobody what the
// gate actually compared.
func writeSummary(w io.Writer, baseline, spec string, rep *report, result policyResult) {
	fmt.Fprintf(w, "baseline: %s\n", baseline)
	fmt.Fprintf(w, "spec:     %s\n", spec)
	if rep.NoChanges {
		// Still fall through: a waiver can be stale even when nothing changed,
		// and that is precisely the run on which it should be noticed.
		fmt.Fprintf(w, "\nThe specification is byte-for-byte compatible with the baseline: no changes.\n")
	} else {
		breaking := 0
		for _, c := range rep.Changes {
			if c.Breaking {
				breaking++
			}
		}
		fmt.Fprintf(w, "changes:  %d (%d additive, %d breaking)\n", len(rep.Changes), len(rep.Changes)-breaking, breaking)
	}

	if len(result.Unwaived) > 0 {
		fmt.Fprintf(w, "\n%d breaking change(s) are not waived:\n\n", len(result.Unwaived))
		for _, c := range result.Unwaived {
			fmt.Fprintf(w, "  %s\n", c.describe())
			fmt.Fprintf(w, "    waive with: %s\n", strings.Join(c.waiverStub(), "\n                "))
		}
	}
	if len(result.Waived) > 0 {
		fmt.Fprintf(w, "\n%d breaking change(s) waived in the waiver file:\n", len(result.Waived))
		for _, c := range result.Waived {
			fmt.Fprintf(w, "  %s\n", c.describe())
		}
	}
	if len(result.Unused) > 0 {
		fmt.Fprintf(w, "\n%d waiver(s) match nothing and must be removed:\n", len(result.Unused))
		for _, waiver := range result.Unused {
			fmt.Fprintf(w, "  %s | %s | %s (owner %s)\n", waiver.Change, waiver.Path, waiver.Property, waiver.Owner)
		}
	}

	if kinds := breakingByKind(result.Unwaived); len(kinds) > 0 {
		fmt.Fprintf(w, "\nunwaived breaking changes by kind:\n")
		for _, k := range kinds {
			fmt.Fprintf(w, "  %-28s %d\n", k.kind, k.count)
		}
	}
}

type kindCount struct {
	kind  string
	count int
}

func breakingByKind(changes []change) []kindCount {
	counts := map[string]int{}
	for _, c := range changes {
		counts[c.ChangeText]++
	}
	out := make([]kindCount, 0, len(counts))
	for kind, n := range counts {
		out = append(out, kindCount{kind: kind, count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].kind < out[j].kind
	})
	return out
}
