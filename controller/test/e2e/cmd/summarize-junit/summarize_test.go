// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/reporters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadFixture(t *testing.T, name string) reporters.JUnitTestSuites {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	var suites reporters.JUnitTestSuites
	require.NoError(t, xml.Unmarshal(raw, &suites))

	return suites
}

// The property the whole tool exists for. Ginkgo counts its suite-level nodes
// in the tests attribute but not in "Ran N of M", so a summary that trusts the
// attribute is permanently out of step with every other report of the run.
func TestSuiteLevelNodesAreNotSpecs(t *testing.T) {
	t.Parallel()

	suites := loadFixture(t, "label-filtered.xml")
	result := summarize(suites)

	assert.Equal(t, 8, suites.Tests, "the fixture's attribute counts [AfterSuite] as a test")
	assert.Equal(t, 7, result.TotalSpecs, "but it is not a spec, so the summary must not count it")
	assert.Equal(t, 3, result.Executed)
	assert.Equal(t, 3, result.Passed)
	assert.Equal(t, 2, result.Deselected)
	assert.Equal(t, 1, result.SkippedAtRuntime)
	assert.Equal(t, 1, result.Pending)
	assert.Equal(t, 0, result.SuiteNodeFailures, "a passing suite node is not reported at all")
	assert.Equal(t, result.TotalSpecs, result.Executed+result.Deselected+result.SkippedAtRuntime+result.Pending,
		"every spec is either executed or not run")
}

// Ginkgo reports both as skipped, and a summary that cannot tell them apart
// reports a check as filtered out when the lane selected it and it declined
// to run -- which reads as coverage that exists.
func TestRuntimeSkipsAreNotMistakenForDeselectedSpecs(t *testing.T) {
	t.Parallel()

	result := summarize(loadFixture(t, "label-filtered.xml"))

	require.Len(t, result.RuntimeSkips, 1)
	assert.Contains(t, result.RuntimeSkips[0].Name, "maps NKE's identities")
	assert.Equal(t, "OIDC is disabled in the CWE deployment (NKE_OIDC_AUTH_ENABLED != true)",
		result.RuntimeSkips[0].Message, "the reason is kept, with Ginkgo's prefix stripped")

	// The reason is the point, so it is not held back behind -with-text the
	// way a Gomega failure message is.
	rendered := result.markdown("T", false)
	assert.Contains(t, rendered, "**1 selected spec skipped at runtime.**")
	assert.Contains(t, rendered, "OIDC is disabled in the CWE deployment")
	assert.NotContains(t, rendered, "skipped - ", "Ginkgo's prefix is an artifact of the format")

	// Deselected specs are counted apart from it rather than absorbing it.
	assert.Contains(t, rendered, "3 of 7 specs in the suite did not run")
}

func TestSkipReason(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		testCase reporters.JUnitTestCase
		want     string
	}{
		"deselected by the filter": {
			testCase: reporters.JUnitTestCase{Skipped: &reporters.JUnitSkipped{Message: "skipped"}},
			want:     "",
		},
		"skipped itself": {
			testCase: reporters.JUnitTestCase{Skipped: &reporters.JUnitSkipped{Message: "skipped - no GPU shape here"}},
			want:     "no GPU shape here",
		},
		// Ginkgo writes pending specs through the same element.
		"pending": {
			testCase: reporters.JUnitTestCase{Skipped: &reporters.JUnitSkipped{Message: "pending"}},
			want:     "",
		},
		"no skipped element": {testCase: reporters.JUnitTestCase{}, want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, testCase.want, skipReason(testCase.testCase))
		})
	}
}

func TestIsSuiteLevelNode(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		specName string
		want     bool
	}{
		"before suite":              {specName: "[BeforeSuite]", want: true},
		"after suite":               {specName: "[AfterSuite]", want: true},
		"synchronized before suite": {specName: "[SynchronizedBeforeSuite]", want: true},
		"synchronized after suite":  {specName: "[SynchronizedAfterSuite]", want: true},
		"report before suite":       {specName: "[ReportBeforeSuite]", want: true},
		"report after suite":        {specName: "[ReportAfterSuite]", want: true},
		"suite-scoped cleanup":      {specName: "[DeferCleanup (Suite)]", want: true},
		"a spec":                    {specName: "[It] does a thing [label]", want: false},
		// Each-scoped cleanup belongs to a spec, so it is not suite level.
		"each-scoped cleanup": {specName: "[DeferCleanup (Each)] something", want: false},
		"no node type":        {specName: "bare name", want: false},
		"unterminated":        {specName: "[It unterminated", want: false},
		"empty":               {specName: "", want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, testCase.want, isSuiteLevelNode(testCase.specName))
		})
	}
}

func TestSetupFailureIsReportedApartFromSpecs(t *testing.T) {
	t.Parallel()

	result := summarize(loadFixture(t, "setup-failed.xml"))

	assert.Equal(t, 1, result.SuiteNodeFailures, "the failed BeforeSuite is surfaced")
	assert.Equal(t, 0, result.Failed, "but it is not counted as a failed spec")
	assert.Equal(t, 1, result.Errored, "the panicking spec is an error, not a failure")
	assert.Equal(t, 1, result.Passed)
	assert.Equal(t, 2, result.Executed)
	assert.Equal(t, 3, result.TotalSpecs)

	require.Len(t, result.NotPassed, 2)
	assert.Equal(t, "[BeforeSuite]", result.NotPassed[0].Name)
	assert.Equal(t, "no declared site offers a arm64 CPU shape and a l40 GPU shape", result.NotPassed[0].Message)
}

func TestLabelFilterIsRead(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "site-controller", summarize(loadFixture(t, "label-filtered.xml")).LabelFilter)

	// An unfiltered lane writes the property empty, and the summary must then
	// say nothing about filtering rather than claim an empty filter.
	empty := summarize(reporters.JUnitTestSuites{
		TestSuites: []reporters.JUnitTestSuite{{
			Properties: reporters.JUnitProperties{
				Properties: []reporters.JUnitProperty{{Name: "LabelFilter", Value: ""}},
			},
			TestCases: []reporters.JUnitTestCase{
				{Name: "[It] one", Status: statusPassed},
				{Name: "[It] two", Status: statusSkipped},
			},
		}},
	})
	assert.Empty(t, empty.LabelFilter)
	assert.NotContains(t, empty.markdown("T", false), "label filter")
}

func TestStatusClassification(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		status      string
		wantPassed  int
		wantFailed  int
		wantErrored int
		wantUnknown int
	}{
		"passed":      {status: statusPassed, wantPassed: 1},
		"failed":      {status: statusFailed, wantFailed: 1},
		"timed out":   {status: statusTimedout, wantFailed: 1},
		"aborted":     {status: statusAborted, wantFailed: 1},
		"panicked":    {status: statusPanicked, wantErrored: 1},
		"interrupted": {status: statusInterrupted, wantErrored: 1},
		// A state added by a future Ginkgo must never be silently counted as
		// a pass, which is the one way this summary could lie.
		"unrecognized": {status: "something-new", wantUnknown: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			result := summarize(reporters.JUnitTestSuites{
				TestSuites: []reporters.JUnitTestSuite{{
					TestCases: []reporters.JUnitTestCase{{Name: "[It] a spec", Status: testCase.status}},
				}},
			})

			assert.Equal(t, testCase.wantPassed, result.Passed)
			assert.Equal(t, testCase.wantFailed, result.Failed)
			assert.Equal(t, testCase.wantErrored, result.Errored)
			assert.Equal(t, testCase.wantUnknown, result.Unknown)
			assert.Equal(t, 1, result.Executed)
		})
	}
}

// The bodies of these elements are CapturedGinkgoWriterOutput, which on the
// hardware lanes is the output of commands run against real machines.
func TestCapturedOutputNeverReachesTheSummary(t *testing.T) {
	t.Parallel()

	suites := loadFixture(t, "setup-failed.xml")
	require.Contains(t, suites.TestSuites[0].TestCases[0].SystemErr, "must not be read",
		"the fixture has to carry captured output for this test to mean anything")

	for _, withText := range []bool{false, true} {
		rendered := summarize(suites).markdown("T", withText)
		assert.NotContains(t, rendered, "must not be read")
		assert.NotContains(t, rendered, "the full description")
	}
}

func TestFailureTextIsOptIn(t *testing.T) {
	t.Parallel()

	suites := loadFixture(t, "setup-failed.xml")
	message := "no declared site offers a arm64 CPU shape"

	assert.NotContains(t, summarize(suites).markdown("T", false), message)
	assert.Contains(t, summarize(suites).markdown("T", true), message)
}

func TestHeadlineLeadsWithWhatRan(t *testing.T) {
	t.Parallel()

	rendered := summarize(loadFixture(t, "label-filtered.xml")).markdown("Forge site controller", false)
	lines := strings.Split(strings.TrimSpace(rendered), "\n")

	require.GreaterOrEqual(t, len(lines), 3)
	assert.Equal(t, "### Forge site controller", lines[0])
	// The count that matters is what executed. A label-filtered lane skips
	// most of the suite by design, and leading with that reads as breakage.
	assert.Equal(t, "**3 passed** of 3 specs executed in 31m03s.", lines[2])
	assert.Contains(t, rendered, "3 of 7 specs in the suite did not run")
	assert.Contains(t, rendered, "label filter `site-controller`")
	assert.Contains(t, rendered, "(1 of them pending)")
}

func TestExecutedListIsSuppressedWhenNothingPassed(t *testing.T) {
	t.Parallel()

	onlyFailures := reporters.JUnitTestSuites{
		TestSuites: []reporters.JUnitTestSuite{{
			TestCases: []reporters.JUnitTestCase{{
				Name:    "[It] the only spec",
				Status:  statusFailed,
				Failure: &reporters.JUnitFailure{Message: "boom"},
			}},
		}},
	}

	rendered := summarize(onlyFailures).markdown("T", false)
	assert.Contains(t, rendered, "Did not pass:")
	assert.NotContains(t, rendered, "<details>", "it would only repeat the block above it")
}

func TestEmptyReport(t *testing.T) {
	t.Parallel()

	rendered := summarize(reporters.JUnitTestSuites{}).markdown("T", false)
	assert.Contains(t, rendered, "No specs in the report")
}

// A spec name reaches a fenced block verbatim, and encoding/xml hands back the
// decoded text, so a name written with a fence in it would end the block early.
func TestFenceInASpecNameCannotBreakTheBlock(t *testing.T) {
	t.Parallel()

	suites := loadFixture(t, "setup-failed.xml")
	decoded := suites.TestSuites[0].TestCases[3].Name
	require.Contains(t, decoded, "```", "the fixture has to carry a fence for this test to mean anything")
	require.Contains(t, decoded, `& <markup> and "quotes"`, "encoding/xml decodes the entities for us")

	rendered := summarize(suites).markdown("T", false)
	assert.NotContains(t, rendered, "``` fences")
	assert.Contains(t, rendered, "''' fences")
	// Three fences open and close the two blocks; a fourth would mean one of
	// them was terminated early by the spec name.
	assert.Equal(t, 4, strings.Count(rendered, "```"))
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		in   time.Duration
		want string
	}{
		"sub second": {in: 500 * time.Millisecond, want: "500ms"},
		// The suite that rejected its own configuration. "0.0s" read as a bug.
		"instant failure": {in: 11368487 * time.Nanosecond, want: "11ms"},
		"one second":      {in: time.Second, want: "1.0s"},
		"seconds":         {in: 6962 * time.Millisecond, want: "7.0s"},
		"minutes":         {in: 640 * time.Second, want: "10m40s"},
		"hours":           {in: 3526 * time.Second, want: "58m46s"},
		"over hour":       {in: 7260 * time.Second, want: "2h01m"},
		"zero":            {in: 0, want: "0ms"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, testCase.want, formatDuration(testCase.in))
		})
	}
}

func TestClip(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "short", clip("short", 300))
	assert.Equal(t, "lines are collapsed", clip("lines\nare\tcollapsed", 300))
	assert.Equal(t, "abcde [...]", clip("abcdefghij", 5))
}

func TestLegName(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		path string
		want string
	}{
		"sequential leg": {path: "/state/junit-cp-controller.xml", want: "cp-controller"},
		"other leg":      {path: "/state/junit-nodepool.xml", want: "nodepool"},
		"single leg":     {path: "/state/junit.xml", want: "junit"},
		"unconventional": {path: "/state/site-e2e.junit.xml", want: "site-e2e.junit"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, testCase.want, legName(testCase.path))
		})
	}
}
