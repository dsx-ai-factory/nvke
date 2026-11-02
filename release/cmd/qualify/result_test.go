// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

const resultTestPlan = `{
  "subject": "nvke",
  "version": "v1.0.0",
  "scope": "full",
  "combinations": [
    {
      "id": "cli-v1.0.0",
      "components": [{"name": "cli", "version": "v1.0.0"}],
      "dependencies": [{"name": "cni-ovn", "version": "1.4.0"}],
      "streams": [
        {
          "name": "amd64-1.34",
          "capabilities": {"arch": "amd64", "gpu": "h100", "kube": "1.34"},
          "dependencies": [{"name": "kube", "version": "1.34.2"}]
        }
      ]
    }
  ]
}`

// writeTestPlan writes resultTestPlan to <dir>/plan.json and returns its path.
func writeTestPlan(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(resultTestPlan), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeGinkgoReport(t *testing.T, succeeded bool) string {
	t.Helper()
	state := types.SpecStateFailed
	if succeeded {
		state = types.SpecStatePassed
	}
	start := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	report := types.Report{
		SuiteDescription: "NVKE E2E Suite",
		SuiteSucceeded:   succeeded,
		StartTime:        start,
		EndTime:          start.Add(5 * time.Minute),
		PreRunStats:      types.PreRunStats{SpecsThatWillRun: 1},
		SpecReports:      types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: state}},
	}
	b, err := json.Marshal([]types.Report{report})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runResultCmd(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	cmd := resultCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return buf.String(), err
}

func TestResultCmdSucceeds(t *testing.T) {
	plan := writeTestPlan(t)
	report := writeGinkgoReport(t, true)

	stdout, err := runResultCmd(t,
		"--plan", plan,
		"--report", report,
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
		"--output", "-",
	)
	if err != nil {
		t.Fatal(err)
	}

	var result qualification.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
	}
	if result.Spec.Subject != "nvke" {
		t.Errorf("spec = %+v, want subject=nvke", result.Spec)
	}
	stream := result.Status.Combination.Stream
	if stream.Result != "passed" {
		t.Errorf("result = %q, want passed", stream.Result)
	}
	if stream.Suite != "NVKE E2E Suite" {
		t.Errorf("suite = %q, want the Ginkgo report's own SuiteDescription", stream.Suite)
	}
	if stream.Capabilities["gpu"] != "h100" || stream.Capabilities["kube"] != "1.34" {
		t.Errorf("capabilities = %v, want gpu=h100 kube=1.34 stamped from plan.json", stream.Capabilities)
	}
	if len(stream.Dependencies) != 1 || stream.Dependencies[0].Name != "kube" || stream.Dependencies[0].Version != "1.34.2" {
		t.Errorf("dependencies = %+v, want the stream's own kube 1.34.2 stamped from plan.json", stream.Dependencies)
	}
}

// Every real plan holds exactly one combination, so --combination may be
// omitted -- the one case worth typing the generated id out for is a plan
// that genuinely holds more than one.
func TestResultCmdDefaultsToSoleCombination(t *testing.T) {
	plan := writeTestPlan(t)
	report := writeGinkgoReport(t, true)

	stdout, err := runResultCmd(t,
		"--plan", plan,
		"--report", report,
		"--stream", "amd64-1.34",
		"--output", "-",
	)
	if err != nil {
		t.Fatal(err)
	}

	var result qualification.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
	}
	if result.Status.Combination.ID != "cli-v1.0.0" {
		t.Errorf("combination = %q, want the plan's sole combination cli-v1.0.0", result.Status.Combination.ID)
	}
}

func TestResultCmdRequiresCombinationWhenPlanHasMultiple(t *testing.T) {
	twoCombos := strings.Replace(resultTestPlan, `"combinations": [`, `"combinations": [
    {
      "id": "cli-v1.0.1",
      "components": [{"name": "cli", "version": "v1.0.1"}],
      "streams": [{"name": "amd64-1.34", "capabilities": {"arch": "amd64", "gpu": "h100", "kube": "1.34"}}]
    },`, 1)
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(plan, []byte(twoCombos), 0o644); err != nil {
		t.Fatal(err)
	}
	report := writeGinkgoReport(t, true)

	_, err := runResultCmd(t,
		"--plan", plan,
		"--report", report,
		"--stream", "amd64-1.34",
	)
	if err == nil || !strings.Contains(err.Error(), "--combination is required") {
		t.Errorf("err = %v, want it to say --combination is required", err)
	}
}

func TestResultCmdReportsFailOnFailedSuite(t *testing.T) {
	plan := writeTestPlan(t)
	report := writeGinkgoReport(t, false)

	stdout, err := runResultCmd(t,
		"--plan", plan,
		"--report", report,
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
		"--output", "-",
	)
	if err != nil {
		t.Fatal(err)
	}

	var result qualification.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	stream := result.Status.Combination.Stream
	if stream.Result != "failed" {
		t.Errorf("result = %q, want failed", stream.Result)
	}
	if strings.Contains(stream.Summary, "infrastructure failure") {
		t.Errorf("summary = %q, a failed suite is not an infrastructure failure", stream.Summary)
	}
}

// A run that never produced a report (Forge provisioning failure, a wedged
// runner) still needs a visible red entry, not a command error that leaves
// nothing published.
func TestResultCmdReportsInfrastructureFailureOnMissingReport(t *testing.T) {
	plan := writeTestPlan(t)

	stdout, err := runResultCmd(t,
		"--plan", plan,
		"--report", filepath.Join(t.TempDir(), "does-not-exist.json"),
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
		"--output", "-",
	)
	if err != nil {
		t.Fatalf("RunE returned an error, want a written errored result: %v", err)
	}

	var result qualification.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	stream := result.Status.Combination.Stream
	if stream.Result != "errored" {
		t.Errorf("result = %q, want errored", stream.Result)
	}
	if !strings.Contains(stream.Summary, "infrastructure failure") {
		t.Errorf("summary = %q, want it to say infrastructure failure", stream.Summary)
	}
	if stream.Suite != "" {
		t.Errorf("suite = %q, want empty -- Ginkgo never ran far enough to say which suite this was", stream.Suite)
	}
	// Capabilities still come from the plan even when the report is missing --
	// the plan load does not depend on the report.
	if stream.Capabilities["gpu"] != "h100" {
		t.Errorf("capabilities = %v, want gpu stamped from plan.json even on an infra failure", stream.Capabilities)
	}
}

// A report that exists but is unreadable (malformed JSON, a truncated or
// corrupted artifact) is not the same thing as the suite never having run --
// it is a bug in how the report was written, so it is a command error, not a
// quiet FAIL result that would bury it.
func TestResultCmdErrorsOnMalformedReport(t *testing.T) {
	plan := writeTestPlan(t)
	badReport := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(badReport, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runResultCmd(t,
		"--plan", plan,
		"--report", badReport,
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
	)
	if err == nil || !strings.Contains(err.Error(), "--report") {
		t.Errorf("err = %v, want it to name --report", err)
	}
}

// Ginkgo always encodes the report as a JSON array with one entry; an empty
// array is as unreadable as malformed JSON, not an infrastructure failure.
func TestResultCmdErrorsOnEmptyReportArray(t *testing.T) {
	plan := writeTestPlan(t)
	emptyReport := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(emptyReport, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runResultCmd(t,
		"--plan", plan,
		"--report", emptyReport,
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
	)
	if err == nil || !strings.Contains(err.Error(), "--report") {
		t.Errorf("err = %v, want it to name --report", err)
	}
}

// When --started-at is passed, the infra-failure path's StartedAt is the
// job's own start time, not the moment `qualify result` happened to run --
// there is no Ginkgo report to read it from otherwise.
func TestResultCmdInfrastructureFailureUsesStartedAtFlag(t *testing.T) {
	plan := writeTestPlan(t)
	jobStart := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

	stdout, err := runResultCmd(t,
		"--plan", plan,
		"--report", filepath.Join(t.TempDir(), "does-not-exist.json"),
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
		"--started-at", jobStart.Format(time.RFC3339),
		"--output", "-",
	)
	if err != nil {
		t.Fatalf("RunE returned an error, want a written FAIL result: %v", err)
	}

	var result qualification.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Spec.StartedAt.Equal(jobStart) {
		t.Errorf("startedAt = %v, want the --started-at value %v", result.Spec.StartedAt, jobStart)
	}
	if !result.Spec.FinishedAt.After(jobStart) {
		t.Errorf("finishedAt = %v, want it after startedAt (still qualify result's own run time)", result.Spec.FinishedAt)
	}
}

// A suite interrupted or aborted before Ginkgo wrote EndTime (a wedged
// runner killed mid-suite, not the "report missing entirely" infra-failure
// path) must not let a zero EndTime overwrite FinishedAt with 0001-01-01 --
// that would make buildPrefix reject the publish and bury exactly the run
// that most needs to show up red.
func TestResultCmdFallsBackWhenGinkgoEndTimeIsZero(t *testing.T) {
	plan := writeTestPlan(t)
	report := types.Report{
		SuiteDescription: "NVKE E2E Suite",
		SuiteSucceeded:   false,
		StartTime:        time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC),
		EndTime:          time.Time{},
		PreRunStats:      types.PreRunStats{SpecsThatWillRun: 1},
		SpecReports:      types.SpecReports{{LeafNodeType: types.NodeTypeIt, State: types.SpecStateFailed}},
	}
	b, err := json.Marshal([]types.Report{report})
	if err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(reportPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, err := runResultCmd(t,
		"--plan", plan,
		"--report", reportPath,
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
		"--output", "-",
	)
	if err != nil {
		t.Fatalf("RunE returned an error, want a written result: %v", err)
	}

	var result qualification.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Spec.FinishedAt.IsZero() {
		t.Error("finishedAt is zero, want it to keep the default (qualify result's own run time) instead of Ginkgo's zero EndTime")
	}
	if !result.Spec.StartedAt.Equal(report.StartTime) {
		t.Errorf("startedAt = %v, want Ginkgo's own non-zero StartTime %v", result.Spec.StartedAt, report.StartTime)
	}
}

func TestResultCmdRejectsMalformedStartedAt(t *testing.T) {
	plan := writeTestPlan(t)

	_, err := runResultCmd(t,
		"--plan", plan,
		"--report", filepath.Join(t.TempDir(), "does-not-exist.json"),
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
		"--started-at", "not-a-timestamp",
	)
	if err == nil || !strings.Contains(err.Error(), "--started-at") {
		t.Errorf("err = %v, want it to name --started-at", err)
	}
}

func TestResultCmdRejectsUnknownCombination(t *testing.T) {
	plan := writeTestPlan(t)
	report := writeGinkgoReport(t, true)

	_, err := runResultCmd(t,
		"--plan", plan,
		"--report", report,
		"--combination", "does-not-exist",
		"--stream", "amd64-1.34",
	)
	if err == nil {
		t.Fatal("got nil, want an error for an unknown combination")
	}
}

func TestResultCmdRejectsUnknownStream(t *testing.T) {
	plan := writeTestPlan(t)
	report := writeGinkgoReport(t, true)

	_, err := runResultCmd(t,
		"--plan", plan,
		"--report", report,
		"--combination", "cli-v1.0.0",
		"--stream", "does-not-exist",
	)
	if err == nil {
		t.Fatal("got nil, want an error for an unknown stream")
	}
}

func TestResultCmdRejectsMissingPlan(t *testing.T) {
	report := writeGinkgoReport(t, true)

	_, err := runResultCmd(t,
		"--plan", filepath.Join(t.TempDir(), "does-not-exist.json"),
		"--report", report,
		"--combination", "cli-v1.0.0",
		"--stream", "amd64-1.34",
	)
	if err == nil || !strings.Contains(err.Error(), "--plan") {
		t.Errorf("err = %v, want it to name --plan", err)
	}
}

// A suite running under Ginkgo's own focus/skip filtering -- the normal case
// for a per-stream subset of a shared suite binary -- must read as a
// fully-passing run restricted to the specs the filter selected, not a
// mostly-failing run sized to the whole suite binary: SpecReports carries an
// entry for every It in the binary, including the ones the filter excluded
// (recorded skipped, not absent), so the denominator has to come from
// PreRunStats.SpecsThatWillRun, not a count of It leaf nodes. SpecReports
// also carries a BeforeSuite/AfterSuite entry, which must not inflate the
// passed count either.
func TestSummarizeGinkgoReportCountsAgainstSpecsThatWillRun(t *testing.T) {
	report := types.Report{
		PreRunStats: types.PreRunStats{SpecsThatWillRun: 2},
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeBeforeSuite, State: types.SpecStatePassed},
			{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed},
			{LeafNodeType: types.NodeTypeIt, State: types.SpecStatePassed},
			{LeafNodeType: types.NodeTypeIt, State: types.SpecStateSkipped},
			{LeafNodeType: types.NodeTypeAfterSuite, State: types.SpecStatePassed},
		},
	}
	got := summarizeGinkgoReport(report)
	if got != "2/2 passed" {
		t.Errorf("summarizeGinkgoReport = %q, want %q (2 passed Its against the 2 specs the filter selected, not the whole suite binary)", got, "2/2 passed")
	}
}

// A re-run keeps the run's ID, and the bare run URL opens on the latest
// attempt, so a result has to link the attempt that produced it or it can
// land on another attempt's green jobs. Outside a run there is nothing to link.
func TestRunURLFromEnvLinksTheAttempt(t *testing.T) {
	cases := []struct {
		name    string
		runID   string
		attempt string
		want    string
	}{
		{name: "a re-run links its own attempt", runID: "7", attempt: "2", want: "https://github.com/o/r/actions/runs/7/attempts/2"},
		{name: "a first attempt is linked as one", runID: "7", attempt: "1", want: "https://github.com/o/r/actions/runs/7/attempts/1"},
		{name: "no attempt in the environment links the run", runID: "7", attempt: "", want: "https://github.com/o/r/actions/runs/7"},
		{name: "outside a run there is no link", runID: "", attempt: "1", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_SERVER_URL", "https://github.com")
			t.Setenv("GITHUB_REPOSITORY", "o/r")
			t.Setenv("GITHUB_RUN_ID", tc.runID)
			t.Setenv("GITHUB_RUN_ATTEMPT", tc.attempt)
			if got := runURLFromEnv(); got != tc.want {
				t.Errorf("runURLFromEnv() = %q, want %q", got, tc.want)
			}
		})
	}
}
