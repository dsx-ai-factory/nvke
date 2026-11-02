// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/onsi/ginkgo/v2/types"
	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

func resultCmd() *cobra.Command {
	var (
		plan        string
		report      string
		combination string
		stream      string
		output      string
		startedAt   string
	)

	cmd := &cobra.Command{
		Use:   "result",
		Short: "Record what one stream of one combination qualified",
		Long: `Record what one stream of one combination qualified.

Writes a QualificationResult: the combination's components and this stream's
outcome, for ` + "`qualify publish`" + ` to read alongside the JUnit report from the
same run.

Reads --combination and --stream from plan.json (components, dependencies),
and --report (Ginkgo's JSON report) for the outcome -- pass/fail and a summary
come from the Ginkgo report, not a second re-derivation from JUnit.

--combination may be omitted when the plan holds exactly one, which every
real plan does today -- its id is a generated string long enough that typing
it out is never worth it when there is nothing to disambiguate.

If --report is missing (the suite never ran -- a Forge provisioning failure,
a wedged runner), this still writes a Result: errored, with a summary saying
so, rather than erroring out itself -- the record is kept even though
` + "`qualify publish`" + ` will not ship it to TestGrid, since Ginkgo never ran far
enough to say which suite this was. On that path, StartedAt comes from
--started-at if the workflow passed one (the job's own start time), since
there is no Ginkgo report to read it from; FinishedAt is still
` + "`qualify result`" + `'s own run time, which is accurate -- that is genuinely
when the failure was discovered.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := qualification.LoadPlan(plan)
			if err != nil {
				return fmt.Errorf("--plan: %w", err)
			}
			if combination == "" {
				if len(p.Combinations) != 1 {
					return fmt.Errorf("%s: plan has %d combinations, --combination is required", plan, len(p.Combinations))
				}
				combination = p.Combinations[0].ID
			}
			combo, strm, err := qualification.CombinationStream(p, combination, stream)
			if err != nil {
				return err
			}

			infraFailureStart := time.Now().UTC()
			if startedAt != "" {
				t, err := time.Parse(time.RFC3339, startedAt)
				if err != nil {
					return fmt.Errorf("--started-at: %w", err)
				}
				infraFailureStart = t.UTC()
			}

			resultState, summary, suite, runStartedAt, runFinishedAt := qualification.ResultErrored, "", "", infraFailureStart, time.Now().UTC()
			ginkgoReport, err := loadGinkgoReport(report)
			switch {
			case errors.Is(err, os.ErrNotExist):
				// The suite never ran -- a Forge provisioning failure, a
				// wedged runner. Genuinely nothing to report beyond that.
				summary = fmt.Sprintf("infrastructure failure: %s", err)
			case err != nil:
				// The report exists but is unreadable -- a bug in how it was
				// written, or a corrupted artifact. Distinct from the suite
				// itself failing, so this is a command error, not a quiet
				// errored result that would bury it.
				return fmt.Errorf("--report: %w", err)
			default:
				resultState = qualification.ResultFailed
				if ginkgoReport.SuiteSucceeded {
					resultState = qualification.ResultPassed
				}
				summary = summarizeGinkgoReport(ginkgoReport)
				// A suite interrupted or aborted before it wrote StartTime/
				// EndTime (which this run most needs to show up red for)
				// leaves one or both zero -- keep the infra-failure defaults
				// rather than overwrite them with 0001-01-01, which would
				// make buildPrefix's zero-FinishedAt guard reject the
				// publish and bury the run entirely.
				if !ginkgoReport.StartTime.IsZero() {
					runStartedAt = ginkgoReport.StartTime
				}
				if !ginkgoReport.EndTime.IsZero() {
					runFinishedAt = ginkgoReport.EndTime
				}
				suite = ginkgoReport.SuiteDescription
			}

			result := qualification.NewResult(
				qualification.ResultSpec{
					Subject:      p.Subject,
					Version:      p.Version,
					Dependencies: combo.Dependencies,
					StartedAt:    runStartedAt,
					FinishedAt:   runFinishedAt,
					Identity:     identityFromEnv(),
				},
				qualification.ResultCombination{
					ID:         combo.ID,
					Components: combo.Components,
					Stream: qualification.ResultStream{
						Name:         strm.Name,
						Scope:        p.Scope,
						Result:       resultState,
						Summary:      summary,
						Suite:        suite,
						RunURL:       runURLFromEnv(),
						Capabilities: strm.Capabilities,
						Dependencies: strm.Dependencies,
					},
				},
			)
			return write(cmd, output, result)
		},
	}

	f := cmd.Flags()
	f.StringVar(&plan, "plan", "", "path to the plan.json this leg ran from (required)")
	f.StringVar(&report, "report", "", "path to the Ginkgo JSON report from this leg's run (required)")
	f.StringVar(&combination, "combination", "", "the combination id this leg ran (required if the plan has more than one)")
	f.StringVar(&stream, "stream", "", "the stream name this leg ran (required)")
	f.StringVar(&output, "output", "-", "where to write the result, - is stdout")
	f.StringVar(&startedAt, "started-at", "", "RFC3339 time the job started, used as StartedAt only when --report is missing (default: now, which is a near-zero-duration guess)")

	for _, name := range []string{"plan", "report", "stream"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

// loadGinkgoReport reads a report written by -ginkgo.json-report. Ginkgo
// always encodes it as a JSON array (one entry per suite run in the process);
// this test binary runs a single suite, so the first entry is the report.
func loadGinkgoReport(path string) (types.Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return types.Report{}, fmt.Errorf("reading Ginkgo report: %w", err)
	}
	var reports []types.Report
	if err := json.Unmarshal(b, &reports); err != nil {
		return types.Report{}, fmt.Errorf("parsing Ginkgo report: %w", err)
	}
	if len(reports) == 0 {
		return types.Report{}, fmt.Errorf("%s: no reports found", path)
	}
	return reports[0], nil
}

// summarizeGinkgoReport renders a short pass-count summary from a Ginkgo
// report's own spec states. The denominator is PreRunStats.SpecsThatWillRun,
// not a count of It leaf nodes: SpecReports carries an entry for every It in
// the suite binary, including ones a stream's own -ginkgo.label-filter
// excluded (recorded as skipped, not absent), so counting It nodes directly
// would read a fully-passing filtered run as mostly failing instead of
// correctly sized to the handful of specs the filter actually selected.
func summarizeGinkgoReport(report types.Report) string {
	its := report.SpecReports.WithLeafNodeType(types.NodeTypeIt)
	total := report.PreRunStats.SpecsThatWillRun
	passed := its.CountWithState(types.SpecStatePassed)
	return fmt.Sprintf("%d/%d passed", passed, total)
}

// runURLFromEnv builds the URL of this attempt of the GitHub Actions run from
// the environment. Empty outside a GitHub Actions run.
//
// The attempt is part of it because a re-run keeps the run's ID, and the bare
// run URL opens on whichever attempt ran last. A result from attempt 1 linked
// that way lands on attempt 2's jobs, which can all be green beside a red
// result that none of them produced. Without GITHUB_RUN_ATTEMPT it is the bare
// run URL.
func runURLFromEnv() string {
	server := os.Getenv("GITHUB_SERVER_URL")
	repo := os.Getenv("GITHUB_REPOSITORY")
	runID := os.Getenv("GITHUB_RUN_ID")
	if server == "" || repo == "" || runID == "" {
		return ""
	}
	url := fmt.Sprintf("%s/%s/actions/runs/%s", server, repo, runID)
	if attempt := os.Getenv("GITHUB_RUN_ATTEMPT"); attempt != "" {
		url += "/attempts/" + attempt
	}
	return url
}

// identityFromEnv reads the run's identity from the GitHub Actions
// environment. Empty outside a GitHub Actions run, which is fine for local
// testing -- an identity of all-empty strings is still valid JSON.
func identityFromEnv() qualification.RunIdentity {
	return qualification.RunIdentity{
		Repository: os.Getenv("GITHUB_REPOSITORY"),
		RunID:      os.Getenv("GITHUB_RUN_ID"),
		RunAttempt: os.Getenv("GITHUB_RUN_ATTEMPT"),
		SHA:        os.Getenv("GITHUB_SHA"),
		Workflow:   os.Getenv("GITHUB_WORKFLOW"),
		Actor:      os.Getenv("GITHUB_ACTOR"),
	}
}
