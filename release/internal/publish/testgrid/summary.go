// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testgrid

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/onsi/ginkgo/v2/reporters"
)

// testGridMessageLimit is the length below which TestGrid keeps a failure
// whole. Its updater joins a test case's message attribute and body with a
// newline, and turns anything of 140 bytes or more into the first 70 bytes,
// "...", and the last 70 (metadata/junit Result.Message, called with
// `const max = 140` in pkg/updater). Gomega's framing fills the first 70 and
// Ginkgo's "In [It] at: file:line @ time" line the last 70, so the cause in
// between is the part that is lost.
const testGridMessageLimit = 140

var (
	// gomegaPreamble is the line Gomega prints ahead of an unexpected error.
	gomegaPreamble = regexp.MustCompile(`^(Unexpected error:|Expected success, but got an error:)$`)
	// gomegaTypeLine is a line holding only Gomega's tag for the value that
	// follows it, e.g. "<*fmt.wrapError | 0xc000123456>:".
	gomegaTypeLine = regexp.MustCompile(`^<[^<>]+>:?$`)
	// gomegaPointerTag is the same tag when it shares a line with the value.
	gomegaPointerTag = regexp.MustCompile(`<[^<>]*\|\s*0x[0-9a-f]+>:\s*`)
	// ginkgoStateLabel is the label a Ginkgo failure description opens with.
	ginkgoStateLabel = regexp.MustCompile(`^\[(FAILED|PANICKED|TIMEDOUT|INTERRUPTED|ABORTED)\]\s*`)
	// ginkgoLocation is Ginkgo's "In [BeforeAll] at: /path/file_test.go:227 @ <time>" line.
	ginkgoLocation = regexp.MustCompile(`In \[([^\]]+)\] at: (\S+):(\d+)`)
	uuidPattern    = regexp.MustCompile(`\b([0-9a-f]{8})-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	whitespaceRun  = regexp.MustCompile(`\s+`)
)

// summarizeFailures rewrites each failed, errored, panicked or timed-out test
// case in a Ginkgo JUnit report so that what TestGrid keeps is the failure's
// cause: the message becomes a one-line summary shorter than
// testGridMessageLimit, and the body is emptied, since TestGrid would join it
// back on and cut the summary again. The full failure text is not lost: Ginkgo
// already writes it into the case's system-err as part of the timeline, and it
// is copied there when it is missing.
//
// A report with nothing to rewrite comes back unchanged, byte for byte.
func summarizeFailures(junit []byte) ([]byte, error) {
	var report reporters.JUnitTestSuites
	if err := xml.Unmarshal(junit, &report); err != nil {
		return nil, fmt.Errorf("parsing JUnit report: %w", err)
	}
	changed := false
	for i := range report.TestSuites {
		cases := report.TestSuites[i].TestCases
		for j := range cases {
			tc := &cases[j]
			switch {
			case tc.Failure != nil:
				changed = summarizeCase(tc, tc.Failure.Type, &tc.Failure.Message, &tc.Failure.Description) || changed
			case tc.Error != nil:
				changed = summarizeCase(tc, tc.Error.Type, &tc.Error.Message, &tc.Error.Description) || changed
			}
		}
	}
	if !changed {
		return junit, nil
	}
	// Encoded the way Ginkgo's own JUnit reporter writes it.
	var out bytes.Buffer
	out.WriteString(xml.Header)
	encoder := xml.NewEncoder(&out)
	encoder.Indent("  ", "    ")
	if err := encoder.Encode(report); err != nil {
		return nil, fmt.Errorf("encoding JUnit report: %w", err)
	}
	return out.Bytes(), nil
}

// summarizeCase rewrites one failing case in place and reports whether it
// changed.
func summarizeCase(tc *reporters.JUnitTestCase, kind string, message, description *string) bool {
	summary := failureSummary(kind, *message, *description)
	if summary == *message && *description == "" {
		return false
	}
	if full := strings.TrimSpace(*description); full != "" && !strings.Contains(tc.SystemErr, full) {
		tc.SystemErr = full + "\n" + tc.SystemErr
	}
	*message, *description = summary, ""
	return true
}

// failureSummary is one line, shorter than testGridMessageLimit, that leads
// with what went wrong. Gomega's framing is dropped; the assertion's own
// description and the error's text are kept; a state other than a plain
// failure is labelled the way Ginkgo labels it ([PANICKED], [TIMEDOUT]); and
// the Ginkgo node and file:line are appended when they fit, the node alone
// when only it fits. A cause too long to fit is cut at the end, never the
// start.
func failureSummary(kind, message, description string) string {
	cause := failureCause(message)
	if cause == "" {
		cause = failureCause(description)
	}
	label := ""
	if kind != "" && kind != "failed" {
		label = "[" + strings.ToUpper(kind) + "] "
	}
	tails := []string{""}
	if m := ginkgoLocation.FindStringSubmatch(description); m != nil {
		tails = []string{fmt.Sprintf(" [%s %s:%s]", m[1], path.Base(m[2]), m[3]), " [" + m[1] + "]", ""}
	}
	for _, tail := range tails {
		if s := strings.TrimSpace(label + cause + tail); len(s) < testGridMessageLimit {
			return s
		}
	}
	const cut = "..."
	return truncateToBytes(label+cause, testGridMessageLimit-1-len(cut)) + cut
}

// failureCause is the text of a Gomega failure message (or, failing that, of
// a Ginkgo failure description) on one line, with the framing removed: no
// "Unexpected error:" preamble, no type tag or field dump around the error,
// no Ginkgo state label or location line, and UUIDs cut to their first block.
func failureCause(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") {
			// Gomega prints an error's fields after its text; the text is the cause.
			break
		}
		if line == "" || gomegaPreamble.MatchString(line) || gomegaTypeLine.MatchString(line) || ginkgoLocation.MatchString(line) {
			continue
		}
		line = ginkgoStateLabel.ReplaceAllString(line, "")
		kept = append(kept, gomegaPointerTag.ReplaceAllString(line, ""))
	}
	cause := uuidPattern.ReplaceAllString(strings.Join(kept, " "), "$1...")
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(cause, " "))
}

// truncateToBytes cuts s to at most n bytes without splitting a UTF-8
// sequence.
func truncateToBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
