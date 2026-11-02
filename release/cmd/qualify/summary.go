// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/onsi/ginkgo/v2/reporters"
	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

func summaryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Render a plan, or the streams' JUnit reports, as Markdown",
		Long: `Render a plan, or the streams' JUnit reports, as Markdown.

Each subcommand writes GitHub-flavoured Markdown to stdout, for a workflow to
append to $GITHUB_STEP_SUMMARY, so what a run qualified and how each stream went
can be read on the run page without downloading an artifact.

  plan     what the plan resolved, with versions and locations in separate columns
  results  what every stream ran, and why anything that did not pass failed`,
		SilenceUsage: true,
	}
	cmd.AddCommand(summaryPlanCmd())
	cmd.AddCommand(summaryResultsCmd())
	return cmd
}

func summaryPlanCmd() *cobra.Command {
	var (
		planAt string
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Render a plan as Markdown",
		Long: `Render a plan as Markdown.

One table for the run as a whole, then for each combination its components and
dependencies, and for each of its streams a line of its capabilities and a
table of its own dependencies.

A version and a location are separate columns. An entry can have either or
both, and one column showing whichever was set could not say which it showed.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan, err := readPlan(planAt)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), planSummary(plan, dryRun))
			return err
		},
	}

	f := cmd.Flags()
	f.StringVar(&planAt, "plan", "plan.json", "the plan to render, - is stdin")
	f.BoolVar(&dryRun, "dry-run", false, "say that the run is a dry run, which takes no machine")
	return cmd
}

func summaryResultsCmd() *cobra.Command {
	var (
		planAt  string
		reports []string
	)

	cmd := &cobra.Command{
		Use:   "results",
		Short: "Render every stream's JUnit report as Markdown",
		Long: `Render every stream's JUnit report as Markdown.

One table with a row for each stream of the plan, then a section for each:
whether its suite passed, what ran and how each went, and for anything that did
not pass, the failure Ginkgo recorded and where it happened.

--report names a stream and the JUnit report it wrote, as stream=path, once for
each stream. A stream of the plan with no --report, or whose report does not
exist, is shown as having none: its job failed or was cancelled before the
suite started, or the suite was killed before it finished. Its log says which.

A spec skipped with no reason is counted rather than listed. That is what Ginkgo
records for a spec the label filter did not select, and a stream selects a few
specs of a suite of hundreds, so listing them would bury the ones that ran. A
spec skipped for a reason -- Skip(), or an earlier spec in an ordered container
failing -- is listed with its reason, and specs skipped for the same reason share
a row.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan, err := readPlan(planAt)
			if err != nil {
				return err
			}
			streams := planStreams(plan)

			found := make(map[string]*reporters.JUnitTestSuites, len(reports))
			seen := make([]string, 0, len(reports))
			for _, r := range reports {
				stream, path, ok := strings.Cut(r, "=")
				switch {
				case !ok || stream == "" || path == "":
					return fmt.Errorf("--report %q is not stream=path", r)
				case !contains(streams, stream):
					return fmt.Errorf("--report %q names a stream the plan does not have", r)
				case contains(seen, stream):
					return fmt.Errorf("--report names %s more than once", stream)
				}
				seen = append(seen, stream)

				suites, err := loadJUnit(path)
				switch {
				case errors.Is(err, os.ErrNotExist):
					continue
				case err != nil:
					return fmt.Errorf("--report %s: %w", stream, err)
				}
				found[stream] = suites
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), resultsSummary(streams, found))
			return err
		},
	}

	f := cmd.Flags()
	f.StringVar(&planAt, "plan", "plan.json", "the plan the run qualified, - is stdin")
	f.StringArrayVar(&reports, "report", nil, "a stream and the JUnit report it wrote, as stream=path, repeated for each stream")
	return cmd
}

// planSummary renders a plan as Markdown.
func planSummary(plan *qualification.Plan, dryRun bool) string {
	overridden := func(name string) string {
		if contains(plan.Overrides, name) {
			return "yes"
		}
		return ""
	}

	streams := 0
	for _, c := range plan.Combinations {
		streams += len(c.Streams)
	}
	overrides := "none"
	if len(plan.Overrides) > 0 {
		overrides = codes(plan.Overrides)
	}
	mode := "run"
	if dryRun {
		mode = "dry run, no machine is taken"
	}

	var b strings.Builder
	b.WriteString("### Qualification plan\n\n| | |\n|---|---|\n")
	row(&b, "nvke", code(plan.Version))
	row(&b, "scope", code(plan.Scope))
	row(&b, "overrides", overrides)
	row(&b, "mode", mode)
	row(&b, "combinations", strconv.Itoa(len(plan.Combinations)))
	row(&b, "streams", strconv.Itoa(streams))

	for _, c := range plan.Combinations {
		fmt.Fprintf(&b, "\n#### Combination %s\n", code(c.ID))
		entries(&b, "Component", c.Components, overridden)
		entries(&b, "Dependency", c.Dependencies, overridden)
		for _, s := range c.Streams {
			fmt.Fprintf(&b, "\n##### Stream %s\n", code(s.Name))
			capabilities(&b, s.Capabilities)
			entries(&b, "Dependency", s.Dependencies, overridden)
		}
	}
	return b.String()
}

// streamCapabilities are the capabilities a stream's section shows, in order,
// with the label each is shown under.
var streamCapabilities = []struct{ key, label string }{
	{"arch", "arch"},
	{"gpu", "GPU"},
	{"ubuntu", "Ubuntu"},
	{"kube", "Kubernetes"},
}

// capabilities writes a stream's capabilities as one line, leaving out any the
// stream does not declare, or nothing for none. The line is not in a table, so
// the values are code spans as they are, without code's escaped pipes.
func capabilities(b *strings.Builder, caps map[string]string) {
	var parts []string
	for _, c := range streamCapabilities {
		if v := caps[c.key]; v != "" {
			parts = append(parts, c.label+" `"+v+"`")
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(b, "\n%s\n", strings.Join(parts, " · "))
	}
}

// entries writes one table of resolved entries, or nothing for none rather than
// a header over no rows.
func entries(b *strings.Builder, kind string, versions []qualification.ComponentVersion, overridden func(string) string) {
	if len(versions) == 0 {
		return
	}
	fmt.Fprintf(b, "\n| %s | Version | URL | Overridden |\n|---|---|---|---|\n", kind)
	for _, v := range versions {
		row(b, text(v.Name), code(v.Version), code(location(v.URL)), overridden(v.Name))
	}
}

// location is a URL as the plan summary shows it: an HTTP(S) URL without its
// scheme and host, any other URL whole.
func location(url string) string {
	for _, scheme := range []string{"https://", "http://"} {
		if rest, ok := strings.CutPrefix(url, scheme); ok {
			if _, path, ok := strings.Cut(rest, "/"); ok {
				return path
			}
		}
	}
	return url
}

// loadJUnit reads a report written by -ginkgo.junit-report.
func loadJUnit(path string) (*reporters.JUnitTestSuites, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading JUnit report: %w", err)
	}
	var suites reporters.JUnitTestSuites
	if err := xml.Unmarshal(b, &suites); err != nil {
		return nil, fmt.Errorf("parsing JUnit report %s: %w", path, err)
	}
	return &suites, nil
}

// specStates is the order a summary counts Ginkgo's spec states in: passed,
// then each way a spec can fail, then the ways it can not run.
var specStates = []string{"passed", "failed", "timedout", "panicked", "interrupted", "aborted", "skipped", "pending"}

// failureLimit bounds the text shown for one failure. GitHub rejects a step
// summary over 1 MiB, and a failure message can carry a whole object dump.
const failureLimit = 4 << 10

// failedStates are the spec states that count as a failure.
var failedStates = []string{"failed", "timedout", "panicked", "interrupted", "aborted"}

// planStreams is every stream the plan names, once each, in the plan's order.
func planStreams(plan *qualification.Plan) []string {
	var names []string
	for _, c := range plan.Combinations {
		for _, s := range c.Streams {
			if !contains(names, s.Name) {
				names = append(names, s.Name)
			}
		}
	}
	return names
}

// outcome is what one stream's JUnit report says about its run.
type outcome struct {
	succeeded bool
	reasons   []string
	filters   []string
	ran       []reporters.JUnitTestCase
	notRun    int
	seconds   float64
}

func outcomeOf(suites *reporters.JUnitTestSuites) outcome {
	o := outcome{succeeded: len(suites.TestSuites) > 0}
	for _, s := range suites.TestSuites {
		// Ginkgo records the outcome itself, and it says more than the counts
		// do: a suite fails with no spec failing on -ginkgo.fail-on-empty, or
		// when it is interrupted between specs. A report without the property
		// is judged by its counts.
		switch s.Properties.WithName("SuiteSucceeded") {
		case "true":
		case "false":
			o.succeeded = false
		default:
			if s.Failures+s.Errors > 0 {
				o.succeeded = false
			}
		}
		if r := s.Properties.WithName("SpecialSuiteFailureReason"); r != "" {
			o.reasons = append(o.reasons, r)
		}
		if f := s.Properties.WithName("LabelFilter"); f != "" && !contains(o.filters, f) {
			o.filters = append(o.filters, f)
		}
		o.seconds += s.Time
		for _, tc := range s.TestCases {
			// Bare "skipped" is a spec that never started: not selected, or
			// not reached after an interrupt. Ginkgo appends the reason to
			// every other skip.
			if tc.Status == "skipped" && tc.Skipped != nil && tc.Skipped.Message == "skipped" {
				o.notRun++
				continue
			}
			o.ran = append(o.ran, tc)
		}
	}
	if suites.Time > 0 {
		o.seconds = suites.Time
	}
	return o
}

// count is how many of the test cases that ran ended in one of states.
func (o outcome) count(states ...string) int {
	n := 0
	for _, tc := range o.ran {
		if contains(states, tc.Status) {
			n++
		}
	}
	return n
}

// verdict is the outcome as a Result cell: in bold, with Ginkgo's reason, when
// it failed.
func (o outcome) verdict() string {
	switch {
	case o.succeeded:
		return "passed"
	case len(o.reasons) > 0:
		return "**failed: " + text(strings.Join(o.reasons, "; ")) + "**"
	default:
		return "**failed**"
	}
}

// resultsSummary renders every stream's JUnit report as Markdown: a row for each
// stream of the plan, then a section for each. A stream missing from reports
// wrote none.
func resultsSummary(streams []string, reports map[string]*reporters.JUnitTestSuites) string {
	var b strings.Builder
	b.WriteString("### Test results\n\n")
	if len(streams) == 0 {
		b.WriteString("The plan names no streams.\n")
		return b.String()
	}

	outcomes := make(map[string]outcome, len(reports))
	passed := 0
	for _, s := range streams {
		if suites, ok := reports[s]; ok {
			outcomes[s] = outcomeOf(suites)
			if outcomes[s].succeeded {
				passed++
			}
		}
	}
	noun := "streams"
	if len(streams) == 1 {
		noun = "stream"
	}
	fmt.Fprintf(&b, "**%d of %d %s passed.**\n\n", passed, len(streams), noun)

	b.WriteString("| Stream | Result | Ran | Passed | Failed | Took |\n|---|---|---|---|---|---|\n")
	for _, s := range streams {
		o, ok := outcomes[s]
		if !ok {
			row(&b, text(s), "**no report**", "", "", "", "")
			continue
		}
		row(&b, text(s), o.verdict(), strconv.Itoa(len(o.ran)), strconv.Itoa(o.count("passed")),
			strconv.Itoa(o.count(failedStates...)), duration(o.seconds))
	}

	for _, s := range streams {
		fmt.Fprintf(&b, "\n#### Stream %s\n\n", code(s))
		o, ok := outcomes[s]
		if !ok {
			b.WriteString("**No report.** Ginkgo wrote no JUnit report: the job failed or was cancelled before the suite" +
				" started, or the suite was killed before it finished. The job log says which.\n")
			continue
		}
		streamDetails(&b, o)
	}
	return b.String()
}

// specRow is one row of a stream's spec table.
type specRow struct {
	spec    string
	result  string
	seconds float64
}

// specRows is a row for each spec that ran, except that specs skipped for the
// same reason share one, where the first of them stood. A spec that fails in an
// ordered container skips every spec after it for one reason, and a row for
// each would bury the failure under them.
func specRows(cases []reporters.JUnitTestCase) []specRow {
	skips := make(map[string]int)
	for _, tc := range cases {
		if tc.Status == "skipped" {
			skips[resultCell(tc)]++
		}
	}

	var rows []specRow
	grouped := make(map[string]int)
	for _, tc := range cases {
		result := resultCell(tc)
		if tc.Status != "skipped" || skips[result] < 2 {
			rows = append(rows, specRow{spec: text(tc.Name), result: result, seconds: tc.Time})
			continue
		}
		if i, ok := grouped[result]; ok {
			rows[i].seconds += tc.Time
			continue
		}
		grouped[result] = len(rows)
		rows = append(rows, specRow{spec: fmt.Sprintf("%d specs", skips[result]), result: result, seconds: tc.Time})
	}
	return rows
}

// streamDetails writes one stream's section: what happened in a sentence, a row
// for each spec that ran, and the failure recorded for each that did not pass.
func streamDetails(b *strings.Builder, o outcome) {
	switch {
	case o.succeeded:
		b.WriteString("**Passed.**")
	case len(o.reasons) > 0:
		fmt.Fprintf(b, "**Failed: %s.**", text(strings.Join(o.reasons, "; ")))
	default:
		b.WriteString("**Failed.**")
	}
	fmt.Fprintf(b, " %d ran", len(o.ran))
	if counts := stateCounts(o.ran); counts != "" {
		b.WriteString(": " + counts)
	}
	b.WriteString(".")
	if o.notRun > 0 {
		why := "not selected"
		if len(o.filters) > 0 {
			why += " by the label filter " + codes(o.filters)
		}
		if !o.succeeded {
			why += " or not reached"
		}
		fmt.Fprintf(b, " %d more did not run, %s.", o.notRun, why)
	}
	fmt.Fprintf(b, " Took %s.\n", duration(o.seconds))

	if len(o.ran) > 0 {
		b.WriteString("\n| Spec | Result | Duration |\n|---|---|---|\n")
		for _, r := range specRows(o.ran) {
			row(b, r.spec, r.result, duration(r.seconds))
		}
	}

	failed := false
	for _, tc := range o.ran {
		body, kind := failure(tc)
		if kind == "" {
			continue
		}
		if !failed {
			b.WriteString("\n##### Failures\n")
			failed = true
		}
		body = truncate(strings.TrimRight(body, " \n"), failureLimit)
		f := fence(body)
		fmt.Fprintf(b, "\n<details>\n<summary>%s (%s)</summary>\n\n%stext\n%s\n%s\n\n</details>\n",
			html.EscapeString(tc.Name), html.EscapeString(kind), f, body, f)
	}
}

// stateCounts says how many test cases ended in each state, in specStates
// order, with any state Ginkgo adds later sorted after them.
func stateCounts(cases []reporters.JUnitTestCase) string {
	counts := map[string]int{}
	for _, tc := range cases {
		counts[tc.Status]++
	}
	var unknown []string
	for state := range counts {
		if !contains(specStates, state) {
			unknown = append(unknown, state)
		}
	}
	sort.Strings(unknown)

	var parts []string
	for _, state := range append(append([]string{}, specStates...), unknown...) {
		if n := counts[state]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, text(state)))
		}
	}
	return strings.Join(parts, ", ")
}

// resultCell is a test case's Result cell: its state, in bold when it did not
// pass or skip, and with the reason when it was skipped for one.
func resultCell(tc reporters.JUnitTestCase) string {
	switch {
	case tc.Status == "passed", tc.Status == "pending":
		return text(tc.Status)
	case tc.Skipped != nil:
		return text(strings.Replace(tc.Skipped.Message, " - ", ": ", 1))
	default:
		return "**" + text(tc.Status) + "**"
	}
}

// failure is what Ginkgo recorded for a test case that failed, and the kind of
// failure. Ginkgo's description is the message and the location it failed at,
// so it is preferred, and the message stands in when a report has none.
func failure(tc reporters.JUnitTestCase) (body, kind string) {
	switch {
	case tc.Failure != nil:
		body, kind = tc.Failure.Description, tc.Failure.Type
		if body == "" {
			body = tc.Failure.Message
		}
	case tc.Error != nil:
		body, kind = tc.Error.Description, tc.Error.Type
		if body == "" {
			body = tc.Error.Message
		}
	default:
		return "", ""
	}
	if kind == "" {
		kind = tc.Status
	}
	return body, kind
}

// duration renders seconds the way a reader scans a table: to the second, and
// to the millisecond under a second.
func duration(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	if d >= time.Second {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Millisecond).String()
}

// truncate cuts s to at most limit bytes, on a rune boundary, and says that it
// did.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n... truncated; the full text is in the JUnit report"
}

// fence is a code fence longer than any run of backticks in s, so a failure
// that quotes Markdown cannot close the block it is shown in.
func fence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return strings.Repeat("`", max(3, longest+1))
}

// row writes one table row from cells that are already Markdown.
func row(b *strings.Builder, cells ...string) {
	b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
}

// text makes s safe in a table cell as plain text: a pipe would end the cell,
// and a name in angle brackets would be read as HTML.
func text(s string) string {
	return strings.ReplaceAll(html.EscapeString(s), "|", `\|`)
}

// code puts s in a code span safe in a table cell, and leaves an empty value an
// empty cell rather than an empty span.
func code(s string) string {
	if s == "" {
		return ""
	}
	return "`" + strings.ReplaceAll(s, "|", `\|`) + "`"
}

// codes is each of values in a code span, separated by commas.
func codes(values []string) string {
	spans := make([]string, len(values))
	for i, v := range values {
		spans[i] = code(v)
	}
	return strings.Join(spans, ", ")
}
