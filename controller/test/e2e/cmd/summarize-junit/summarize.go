// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2/reporters"
)

// Spec states as Ginkgo's JUnit reporter writes them into the status
// attribute, which is types.SpecState.String(). Named here rather than
// matched inline so that a state this tool does not know about is visibly
// absent from the switch rather than silently treated as a pass.
const (
	statusPassed      = "passed"
	statusSkipped     = "skipped"
	statusPending     = "pending"
	statusFailed      = "failed"
	statusTimedout    = "timedout"
	statusAborted     = "aborted"
	statusPanicked    = "panicked"
	statusInterrupted = "interrupted"

	// A Gomega failure message renders everything it asserted on, so a struct
	// comparison runs to kilobytes. A job summary is capped at 1 MiB per step
	// and the diagnostic value is in the opening clause; the rest is in the log.
	messageLimit = 300

	// Ginkgo writes a bare "skipped" into the skipped element for a spec the
	// label filter never selected, and "skipped - <reason>" for one that was
	// selected, started, and called Skip. The two say opposite things about a
	// lane: the first is it running the specs it was asked to, the second is
	// coverage it was expected to have and silently does not.
	skipReasonPrefix = "skipped - "
)

// outcome is one test case reduced to what the summary shows. Deliberately
// not the whole JUnitTestCase: SystemErr holds CapturedGinkgoWriterOutput,
// which on the hardware lanes is the output of commands run against real
// machines, and nothing here should be able to put that on a summary page.
type outcome struct {
	Name     string
	Status   string
	Duration time.Duration
	Message  string
}

// summary is one JUnit file reduced to the numbers and names worth showing.
type summary struct {
	// TotalSpecs and the counts below exclude Ginkgo's suite-level nodes. Its
	// own documentation on JUnitTestSuites.Tests warns that the attribute
	// "includes any suite nodes such as BeforeSuite", which is why a run
	// reporting "Ran 7 of 280 Specs" writes 281 test cases. Counting the
	// attribute would put this summary permanently out of step with every
	// other report of the same run.
	TotalSpecs  int
	Executed    int
	Passed      int
	Failed      int
	Errored     int
	Unknown     int
	Pending     int
	SuiteTime   time.Duration
	LabelFilter string

	// Deselected counts specs the label filter never selected; SkippedAtRuntime
	// counts specs it did select and that then skipped themselves. Ginkgo
	// reports both as skipped, but only the second is a gap in this lane.
	Deselected       int
	SkippedAtRuntime int

	// Ran holds the executed specs in suite order; NotPassed holds everything
	// that did not pass, specs and suite nodes alike; RuntimeSkips holds the
	// SkippedAtRuntime specs with the reason each one gave.
	Ran          []outcome
	NotPassed    []outcome
	RuntimeSkips []outcome

	// SuiteNodeFailures counts BeforeSuite/AfterSuite-class nodes that did not
	// pass. Tracked apart from the spec counts because it does not mean the
	// same thing: setup or teardown broke, so the spec results may not mean
	// what they appear to, and on a hardware lane a failed teardown is a
	// leaked machine.
	SuiteNodeFailures int
}

// leafNodeType returns the bracketed node type a Ginkgo test case name starts
// with. Names are "[NodeType] FullText", and for suite-level nodes the full
// text is empty.
func leafNodeType(name string) string {
	if !strings.HasPrefix(name, "[") {
		return ""
	}
	end := strings.Index(name, "]")
	if end < 0 {
		return ""
	}
	return name[1:end]
}

// isSuiteLevelNode reports whether a test case is one of Ginkgo's suite-level
// nodes rather than a spec. The set is types.NodeTypesForSuiteLevelNodes,
// spelled as types.nodeTypeToString renders each one.
func isSuiteLevelNode(name string) bool {
	switch leafNodeType(name) {
	case "BeforeSuite",
		"SynchronizedBeforeSuite",
		"AfterSuite",
		"SynchronizedAfterSuite",
		"ReportBeforeSuite",
		"ReportAfterSuite",
		"DeferCleanup (Suite)":
		return true
	default:
		return false
	}
}

// failureMessage pulls the message attribute off whichever of the two failure
// elements Ginkgo wrote. It never reads their bodies, nor SystemOut/SystemErr.
func failureMessage(testCase reporters.JUnitTestCase) string {
	if testCase.Failure != nil {
		return testCase.Failure.Message
	}
	if testCase.Error != nil {
		return testCase.Error.Message
	}
	return ""
}

// skipReason returns why a spec skipped itself at runtime, or the empty
// string if the label filter deselected it instead.
func skipReason(testCase reporters.JUnitTestCase) string {
	if testCase.Skipped == nil {
		return ""
	}
	if !strings.HasPrefix(testCase.Skipped.Message, skipReasonPrefix) {
		return ""
	}
	return strings.TrimPrefix(testCase.Skipped.Message, skipReasonPrefix)
}

func seconds(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}

// summarize reduces every suite in one JUnit file to a single summary.
func summarize(suites reporters.JUnitTestSuites) summary {
	var result summary

	for _, suite := range suites.TestSuites {
		result.SuiteTime += seconds(suite.Time)
		if filter := suite.Properties.WithName("LabelFilter"); filter != "" {
			result.LabelFilter = filter
		}

		for _, testCase := range suite.TestCases {
			result.add(testCase)
		}
	}

	return result
}

func (s *summary) add(testCase reporters.JUnitTestCase) {
	item := outcome{
		Name:     testCase.Name,
		Status:   testCase.Status,
		Duration: seconds(testCase.Time),
		Message:  failureMessage(testCase),
	}

	if isSuiteLevelNode(testCase.Name) {
		// Kept out of the spec tally but never dropped: a BeforeSuite that
		// fails takes the whole run with it, and that is the single most
		// useful line a summary can carry.
		if item.Status != statusPassed && item.Status != statusSkipped {
			s.SuiteNodeFailures++
			s.NotPassed = append(s.NotPassed, item)
		}
		return
	}

	s.TotalSpecs++

	// Ginkgo writes pending specs as skipped too, counting them as disabled
	// rather than skipped. Neither executed, so neither joins the tallies
	// below; a runtime skip is kept with its reason so the summary can say
	// which coverage this lane did not reach.
	switch item.Status {
	case statusSkipped:
		if reason := skipReason(testCase); reason != "" {
			item.Message = reason
			s.SkippedAtRuntime++
			s.RuntimeSkips = append(s.RuntimeSkips, item)
			return
		}
		s.Deselected++
		return
	case statusPending:
		s.Pending++
		return
	}

	s.Executed++
	s.Ran = append(s.Ran, item)

	switch item.Status {
	case statusPassed:
		s.Passed++
	case statusFailed, statusTimedout, statusAborted:
		s.Failed++
		s.NotPassed = append(s.NotPassed, item)
	case statusPanicked, statusInterrupted:
		s.Errored++
		s.NotPassed = append(s.NotPassed, item)
	default:
		// A state this tool was not written for. Counted and shown rather
		// than folded into passed, which is the one way a summary can lie.
		s.Unknown++
		s.NotPassed = append(s.NotPassed, item)
	}
}

func formatDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		// Milliseconds rather than a rounded "0.0s". A suite whose setup
		// rejected its own configuration finishes in single-digit
		// milliseconds, and that is the most useful thing the line can say.
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
}

// fenceSafe neutralizes the only sequence that can break out of a fenced
// block. Spec names reach one verbatim, and they are written by humans.
func fenceSafe(s string) string {
	return strings.ReplaceAll(s, "```", "'''")
}

func clip(s string, limit int) string {
	// Collapsed first: a message spanning lines would otherwise break the
	// single-line shape of the block it goes into.
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	return s[:limit] + " [...]"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (s summary) row(item outcome) string {
	return fmt.Sprintf("%-11s %8s  %s", item.Status, formatDuration(item.Duration), fenceSafe(item.Name))
}

// markdown renders the summary as the markdown a job summary displays.
func (s summary) markdown(title string, withText bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, "### %s\n\n", title)

	if s.TotalSpecs == 0 && s.SuiteNodeFailures == 0 {
		fmt.Fprintf(&b, "No specs in the report. The suite wrote a JUnit file with no test cases in it.\n")
		return b.String()
	}

	s.writeHeadline(&b)
	s.writeNotRun(&b)
	s.writeNotPassed(&b, withText)
	s.writeRan(&b)

	return b.String()
}

// writeHeadline leads with what ran and never with what was filtered out. A
// label-filtered lane skips most of the suite by design -- the forge lane runs
// 7 of 280 -- and leading with the skip count reads as breakage on a run that
// is perfectly healthy.
func (s summary) writeHeadline(b *strings.Builder) {
	fmt.Fprintf(b, "**%d passed**", s.Passed)
	if s.Failed > 0 {
		fmt.Fprintf(b, ", **%d failed**", s.Failed)
	}
	if s.Errored > 0 {
		fmt.Fprintf(b, ", **%d errored**", s.Errored)
	}
	if s.Unknown > 0 {
		fmt.Fprintf(b, ", **%d in an unrecognized state**", s.Unknown)
	}
	fmt.Fprintf(b, " of %d spec%s executed", s.Executed, plural(s.Executed))
	if s.SuiteTime > 0 {
		fmt.Fprintf(b, " in %s", formatDuration(s.SuiteTime))
	}
	fmt.Fprintf(b, ".\n")

	if s.SuiteNodeFailures > 0 {
		fmt.Fprintf(b, "\n**%d suite-level node did not pass.** A BeforeSuite or AfterSuite failure is not a spec"+
			" failure: setup or teardown broke, so the spec results may not mean what they appear to.\n",
			s.SuiteNodeFailures)
	}
}

// writeNotRun accounts for the specs that did not execute, keeping the two
// reasons apart. Specs the label filter deselected belong to the other lanes
// sharing this suite and say nothing about this one. A spec this lane selected
// and that then skipped itself is coverage the lane was expected to have, and
// folding it in with the deselected ones is how a summary comes to claim a
// check was made that never ran.
func (s summary) writeNotRun(b *strings.Builder) {
	if deselected := s.Deselected + s.Pending; deselected > 0 {
		fmt.Fprintf(b, "\n%d of %d specs in the suite did not run", deselected, s.TotalSpecs)
		if s.LabelFilter != "" {
			fmt.Fprintf(b, ", having not been selected by the label filter `%s`", s.LabelFilter)
		}
		if s.Pending > 0 {
			fmt.Fprintf(b, " (%d of them pending)", s.Pending)
		}
		fmt.Fprintf(b, ".\n")
	}

	if len(s.RuntimeSkips) == 0 {
		return
	}

	// Always shown, never behind -with-text: these reasons are written by the
	// specs themselves rather than rendered by Gomega, and a reason nobody
	// reads is the whole defect this block exists to fix.
	fmt.Fprintf(b, "\n**%d selected spec%s skipped at runtime.** This lane did not cover:\n\n```\n",
		s.SkippedAtRuntime, plural(s.SkippedAtRuntime))
	for _, item := range s.RuntimeSkips {
		fmt.Fprintf(b, "%s\n%13s%s\n", fenceSafe(item.Name), "", clip(fenceSafe(item.Message), messageLimit))
	}
	fmt.Fprintf(b, "```\n")
}

func (s summary) writeNotPassed(b *strings.Builder, withText bool) {
	if len(s.NotPassed) == 0 {
		return
	}

	fmt.Fprintf(b, "\nDid not pass:\n\n```\n")
	for _, item := range s.NotPassed {
		fmt.Fprintf(b, "%s\n", s.row(item))
		if withText && item.Message != "" {
			fmt.Fprintf(b, "%13s%s\n", "", clip(fenceSafe(item.Message), messageLimit))
		}
	}
	fmt.Fprintf(b, "```\n")
}

// writeRan is suppressed when nothing passed, because then every executed spec
// is already listed under "Did not pass" and this would only repeat it. That
// is the shape of a run that fell over early.
func (s summary) writeRan(b *strings.Builder) {
	if s.Passed == 0 || len(s.Ran) == 0 {
		return
	}

	fmt.Fprintf(b, "\n<details><summary>All %d executed spec%s</summary>\n\n```\n", len(s.Ran), plural(len(s.Ran)))
	for _, item := range s.Ran {
		fmt.Fprintf(b, "%s\n", s.row(item))
	}
	fmt.Fprintf(b, "```\n\n</details>\n")
}
