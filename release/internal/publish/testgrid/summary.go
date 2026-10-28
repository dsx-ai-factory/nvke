// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testgrid

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
	"unicode"
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

// failureCauseProperty names the test-case property that carries a failure's
// cause: the text of the error the failing assertion was about. The e2e suite
// records it as a Ginkgo report entry when an assertion fails
// (controller/test/e2e/failure_format_test.go), and Ginkgo's JUnit reporter
// writes each report entry as a property holding its JSON-encoded value.
const failureCauseProperty = "failure-cause"

var (
	// uuidPattern matches a UUID, which a summary cuts to its first block.
	uuidPattern = regexp.MustCompile(`\b([0-9a-f]{8})-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// terminalEscape matches a terminal control sequence such as a colour
	// code (an ECMA-48 CSI sequence), whether its ESC survived or was
	// replaced by U+FFFD where XML could not carry it.
	terminalEscape = regexp.MustCompile(`[\x1b\x{FFFD}]\[[0-?]*[ -/]*[@-~]`)
)

// summarizeFailures rewrites each failed, errored, panicked or timed-out test
// case in a Ginkgo JUnit report so that what TestGrid keeps is the failure's
// cause: the message becomes a one-line summary shorter than
// testGridMessageLimit, and the body is emptied, since TestGrid would join it
// back on and cut the summary again. The full failure text is not lost:
// Ginkgo already writes it into the case's system-err as part of the
// timeline, and it is copied there when it is missing.
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
	summary := failureSummary(kind, recordedCause(tc, *message+"\n"+*description), *message, *description)
	if summary == *message && *description == "" {
		return false
	}
	// Ginkgo's own timeline in system-err draws every failure, starting with
	// its message, though on a timeout or a failing cleanup not in the same
	// shape as the body. A system-err that carries the message therefore
	// carries the failure. Any other report gets the body and the message
	// copied in, so one with only a message, or a body that does not repeat
	// it, keeps the full text.
	logged := oneLine(tc.SystemErr)
	if key := oneLine(*message); key == "" || !strings.Contains(logged, key) {
		for _, original := range []string{*description, *message} {
			if full := strings.TrimSpace(original); full != "" && !strings.Contains(logged, oneLine(full)) {
				tc.SystemErr = full + "\n" + tc.SystemErr
				logged = oneLine(tc.SystemErr)
			}
		}
	}
	*message, *description = summary, ""
	return true
}

// recordedCause is the cause the e2e suite recorded for a case's failure, on
// one line, or "" when it recorded none. A spec that fails more than once can
// carry a cause for each failure, so the one used is the first whose text the
// reported failure contains. Nothing here depends on how Gomega or Ginkgo
// word a failure.
func recordedCause(tc *reporters.JUnitTestCase, failure string) string {
	if tc.Properties == nil {
		return ""
	}
	reported := oneLine(failure)
	for _, property := range tc.Properties.Properties {
		if property.Name != failureCauseProperty {
			continue
		}
		var cause string
		if err := json.Unmarshal([]byte(property.Value), &cause); err != nil {
			continue
		}
		if cause = oneLine(cause); cause != "" && strings.Contains(reported, cause) {
			return cause
		}
	}
	return ""
}

// failureSummary is one line, shorter than testGridMessageLimit: the recorded
// cause when there is one, and otherwise the failure message itself (or, when
// that is empty, its body), cut at the end rather than in the middle. A state
// other than a plain failure is labelled the way Ginkgo labels it
// ([PANICKED], [TIMEDOUT]), and UUIDs are cut to their first block.
func failureSummary(kind, cause, message, description string) string {
	text := oneLine(cause)
	if text == "" {
		text = oneLine(message)
	}
	if text == "" {
		text = oneLine(description)
	}
	if kind != "" && kind != "failed" {
		text = "[" + strings.ToUpper(kind) + "] " + text
	}
	text = uuidPattern.ReplaceAllString(text, "$1...")
	if len(text) < testGridMessageLimit {
		return text
	}
	const cut = "..."
	return truncateToBytes(text, testGridMessageLimit-1-len(cut)) + cut
}

// oneLine is s as one line of plain text: terminal control sequences are
// dropped, and each run of whitespace or other control characters, line
// breaks included, becomes a single space. U+FFFD counts as a control
// character, since it is what XML holds in place of one.
func oneLine(s string) string {
	s = terminalEscape.ReplaceAllString(s, "")
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError
	}), " ")
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
