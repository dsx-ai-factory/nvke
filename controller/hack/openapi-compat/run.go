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
	"time"
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
	noticeDays := flags.Int("deprecation-days", deprecationNoticeDays, "Days of notice an operation owes consumers between being deprecated and being removed")
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

	got, err := gatherReport(ctx, *reportPath, *reportOut, reportRequest{
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

	// The deprecation rules read the two documents rather than the diff, so
	// they run whether or not the engine found anything: a deprecation
	// announced in this change still has to be a real one.
	life, err := evaluateLifecycle(got.specs.baseline, got.specs.revision, time.Now().UTC(), *noticeDays)
	if err != nil {
		return err
	}
	kept, discharged := applyLifecycle(got.report.Changes, life)
	kept = append(kept, life.Findings...)

	result := evaluate(kept, rules)
	writeSummary(stdout, got.baseline, *spec, got.report, result, discharged, *noticeDays)

	switch {
	case len(result.Unwaived) > 0 && len(result.Unused) > 0:
		return fmt.Errorf("%d unwaived breaking change(s), and %d waiver(s) that match nothing", len(result.Unwaived), len(result.Unused))
	case len(result.Unwaived) > 0:
		return fmt.Errorf("%d unwaived breaking change(s) against %s", len(result.Unwaived), got.baseline)
	case len(result.Unused) > 0:
		return fmt.Errorf("%d waiver(s) in %s match nothing; remove them", len(result.Unused), *waivers)
	}
	return nil
}

// gathered is everything a verdict is reached from: what the engine said, the
// two documents it said it about, and the ref the baseline came from.
type gathered struct {
	report   *report
	specs    specPair
	baseline string
}

// gatherReport resolves both specifications and obtains the engine's report
// over them, reading one from disk instead when --report supplies it.
func gatherReport(ctx context.Context, reportPath, reportOut string, req reportRequest) (gathered, error) {
	// Both specifications are resolved either way. --report replaces the
	// engine, not the documents: the deprecation rules read them directly, and
	// a mode that skipped them would be a mode in which a removal looked
	// sanctioned because nothing had checked.
	specs, resolved, err := req.resolve(ctx)
	if err != nil {
		return gathered{}, err
	}
	out := gathered{specs: specs, baseline: resolved}

	if reportPath != "" {
		raw, err := os.ReadFile(reportPath)
		if err != nil {
			return gathered{}, fmt.Errorf("reading report %s: %w", reportPath, err)
		}
		if out.report, err = parseReport(raw); err != nil {
			return gathered{}, err
		}
		return out, nil
	}

	raw, err := req.produce(ctx, specs)
	if err != nil {
		return gathered{}, err
	}
	if reportOut != "" {
		if err := os.MkdirAll(filepath.Dir(reportOut), 0o755); err != nil {
			return gathered{}, fmt.Errorf("creating the report directory: %w", err)
		}
		if err := os.WriteFile(reportOut, raw, 0o600); err != nil {
			return gathered{}, fmt.Errorf("writing report %s: %w", reportOut, err)
		}
	}
	if out.report, err = parseReport(raw); err != nil {
		return gathered{}, err
	}
	return out, nil
}

// writeSummary prints what changed and what the policy made of it. It is always
// printed, including on success: the additive total is the number a reviewer
// wants beside "no breaking changes", and a silent pass teaches nobody what the
// gate actually compared.
func writeSummary(w io.Writer, baseline, spec string, rep *report, result policyResult, discharged []change, noticeDays int) {
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

	if len(discharged) > 0 {
		fmt.Fprintf(w, "\n%d removal(s) needed no waiver: deprecated, with an %s date that has passed.\n", len(discharged), sunsetExtension)
		for _, c := range discharged {
			fmt.Fprintf(w, "  %s\n", c.describe())
		}
	}

	// Printed only when a removal is among the failures, because it is advice
	// about removals. On a newly-required property it would be a non sequitur,
	// and a gate that answers a question nobody asked gets skimmed.
	if anyRemoval(result.Unwaived) {
		fmt.Fprintf(w, "\nA removal does not have to be waived. Mark the operation `deprecated: true`\n")
		fmt.Fprintf(w, "with an %s date at least %d days out, ship that, and remove it once the date\n", sunsetExtension, noticeDays)
		fmt.Fprintf(w, "has passed; the gate then lets it through on its own.\n")
	}

	if kinds := breakingByKind(result.Unwaived); len(kinds) > 0 {
		fmt.Fprintf(w, "\nunwaived breaking changes by kind:\n")
		for _, k := range kinds {
			fmt.Fprintf(w, "  %-28s %d\n", k.kind, k.count)
		}
	}
}

// anyRemoval reports whether a removal is among these changes. The two kinds
// are the shapes the engine reports a removed path item and a removed operation
// in; see applyLifecycle, which discharges exactly the same two.
func anyRemoval(changes []change) bool {
	for _, c := range changes {
		if c.ChangeText == "object_removed" || c.ChangeText == "property_removed" {
			return true
		}
	}
	return false
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
