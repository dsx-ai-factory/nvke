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
		root        string
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

If --report is missing or unreadable (the suite never ran -- a Forge
provisioning failure, a wedged runner), this still writes a Result: FAIL,
with a summary saying so, rather than erroring. A run that fails this early
still needs a red entry in TestGrid, not silence. On that path, StartedAt
comes from --started-at if the workflow passed one (the job's own start
time), since there is no Ginkgo report to read it from; FinishedAt is still
` + "`qualify result`" + `'s own run time, which is accurate -- that is genuinely
when the failure was discovered.

Type comes from the subject's own qualification.yaml (spec.type).`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if root == "" {
				var err error
				if root, err = repoRoot(); err != nil {
					return err
				}
			}

			p, err := qualification.LoadPlan(plan)
			if err != nil {
				return fmt.Errorf("--plan: %w", err)
			}
			combo, strm, err := qualification.CombinationStream(p, combination, stream)
			if err != nil {
				return err
			}

			subject, err := qualification.LoadSubject(qualification.SubjectPath(root, p.Subject))
			if err != nil {
				return err
			}
			if subject.Spec.Type == "" {
				return fmt.Errorf("%s: spec.type is empty, so this result would not say what kind of thing it qualified", qualification.SubjectPath(root, p.Subject))
			}

			infraFailureStart := time.Now().UTC()
			if startedAt != "" {
				t, err := time.Parse(time.RFC3339, startedAt)
				if err != nil {
					return fmt.Errorf("--started-at: %w", err)
				}
				infraFailureStart = t.UTC()
			}

			resultState, summary, runStartedAt, runFinishedAt := qualification.ResultFail, "", infraFailureStart, time.Now().UTC()
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
				// FAIL result that would bury it.
				return fmt.Errorf("--report: %w", err)
			case ginkgoReport.SuiteSucceeded:
				resultState = qualification.ResultPass
				fallthrough
			default:
				summary = summarizeGinkgoReport(ginkgoReport)
				runStartedAt, runFinishedAt = ginkgoReport.StartTime, ginkgoReport.EndTime
			}

			result := qualification.NewResult(
				qualification.ResultSpec{
					Subject:      p.Subject,
					Type:         subject.Spec.Type,
					Version:      p.Version,
					Dependencies: combo.Dependencies,
					StartedAt:    runStartedAt,
					FinishedAt:   runFinishedAt,
					Identity:     identityFromEnv(),
				},
				qualification.ResultCombination{
					ID:         combo.ID,
					Components: combo.Components,
					Streams: []qualification.ResultStream{{
						Name:         strm.Name,
						Scope:        p.Scope,
						Result:       resultState,
						Summary:      summary,
						RunURL:       runURLFromEnv(),
						Capabilities: strm.Capabilities,
						Dependencies: strm.Dependencies,
					}},
				},
			)
			return write(cmd, output, result)
		},
	}

	f := cmd.Flags()
	f.StringVar(&plan, "plan", "", "path to the plan.json this leg ran from (required)")
	f.StringVar(&report, "report", "", "path to the Ginkgo JSON report from this leg's run (required)")
	f.StringVar(&combination, "combination", "", "the combination id this leg ran (required)")
	f.StringVar(&stream, "stream", "", "the stream name this leg ran (required)")
	f.StringVar(&output, "output", "-", "where to write the result, - is stdout")
	f.StringVar(&root, "root", "", "repository root (default: the enclosing git checkout)")
	f.StringVar(&startedAt, "started-at", "", "RFC3339 time the job started, used as StartedAt only when --report is missing or unreadable (default: now, which is a near-zero-duration guess)")

	for _, name := range []string{"plan", "report", "combination", "stream"} {
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
// report's own spec states, counting only It leaf nodes -- report.SpecReports
// also carries an entry for BeforeSuite/AfterSuite/cleanup nodes, which would
// otherwise inflate the total, and a stream that runs under Ginkgo's own
// focus/skip filtering (common for per-stream subsets of a shared suite
// binary) would read as a mostly-failing run instead of a fully-passing one
// restricted to a handful of specs.
func summarizeGinkgoReport(report types.Report) string {
	its := report.SpecReports.WithLeafNodeType(types.NodeTypeIt)
	total := len(its)
	passed := its.CountWithState(types.SpecStatePassed)
	return fmt.Sprintf("%d/%d passed", passed, total)
}

// runURLFromEnv builds the GitHub Actions run URL from the environment.
// Empty outside a GitHub Actions run.
func runURLFromEnv() string {
	server := os.Getenv("GITHUB_SERVER_URL")
	repo := os.Getenv("GITHUB_REPOSITORY")
	runID := os.Getenv("GITHUB_RUN_ID")
	if server == "" || repo == "" || runID == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/actions/runs/%s", server, repo, runID)
}

// identityFromEnv reads the run's identity from the GitHub Actions
// environment. Empty outside a GitHub Actions run, which is fine for local
// testing -- an identity of all-empty strings is still valid JSON.
func identityFromEnv() qualification.ResultIdentity {
	return qualification.ResultIdentity{
		Repository: os.Getenv("GITHUB_REPOSITORY"),
		RunID:      os.Getenv("GITHUB_RUN_ID"),
		RunAttempt: os.Getenv("GITHUB_RUN_ATTEMPT"),
		SHA:        os.Getenv("GITHUB_SHA"),
		Workflow:   os.Getenv("GITHUB_WORKFLOW"),
		Actor:      os.Getenv("GITHUB_ACTOR"),
	}
}
