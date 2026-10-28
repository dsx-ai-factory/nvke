// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testgrid

import (
	"bytes"
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/onsi/ginkgo/v2/reporters"
)

// The messages below have the shapes Gomega and Ginkgo give real failures:
// an error behind "Unexpected error:" or "Expected success, but got an
// error:" with a type tag and field dump around it, a value mismatch, an
// Eventually timeout, a panic and a suite timeout.
func TestFailureSummary(t *testing.T) {
	cases := []struct {
		name, kind, message, description, want string
	}{
		{
			name: "an unexpected error keeps the assertion's description and the error's text",
			kind: "failed",
			message: "should install the NVKE stack\nUnexpected error:\n    <*fmt.wrapError | 0xc000123456>: \n" +
				"    clusterctl init: exit status 1\n    {\n        msg: \"clusterctl init: exit status 1\",\n" +
				"        err: <*exec.ExitError | 0xc000654321>{ProcessState: {pid: 4242}},\n    }",
			description: "[FAILED] should install the NVKE stack\nUnexpected error:\n    ...\n" +
				"In [It] at: /home/runner/work/nvke/controller/test/e2e/site_controller_test.go:277 @ 10/19/26 09:37:51.511\n",
			want: "should install the NVKE stack clusterctl init: exit status 1 [It site_controller_test.go:277]",
		},
		{
			name: "an expected-success error drops the framing and shortens IDs, keeping the node when file:line does not fit",
			kind: "failed",
			message: "Expected success, but got an error:\n    <*errors.errorString | 0xc000abc123>: \n" +
				"    forge has no usable allocation for this run:\n" +
				"    cpu.x86_64 (0f4e2a91-1c3b-4d5e-8f60-7a8b9c0d1e2f): need 2, unusedUsable=1 (total=7 used=6 unused=1)\n    ",
			description: "[FAILED] Expected success, but got an error:\n    ...\n" +
				"In [BeforeAll] at: /home/runner/work/nvke/controller/test/e2e/site_controller_test.go:227 @ 10/28/26 15:12:53.683\n",
			want: "forge has no usable allocation for this run: cpu.x86_64 (0f4e2a91...): need 2, unusedUsable=1 (total=7 used=6 unused=1) [BeforeAll]",
		},
		{
			name:        "a value mismatch keeps both sides",
			kind:        "failed",
			message:     "Expected\n    <int>: 3\nto equal\n    <int>: 2",
			description: "[FAILED] Expected\n    <int>: 3\nto equal\n    <int>: 2\nIn [It] at: /work/e2e/quota_test.go:41 @ 10/28/26 15:12:53.683\n",
			want:        "Expected <int>: 3 to equal <int>: 2 [It quota_test.go:41]",
		},
		{
			name: "an Eventually timeout keeps the wait and the last value",
			kind: "failed",
			message: "Timed out after 900.001s.\nwaiting for the cluster to be ready\nExpected\n    <string>: Provisioning\n" +
				"to equal\n    <string>: Ready",
			description: "[FAILED] Timed out after 900.001s.\n...\nIn [It] at: /work/e2e/tenant_test.go:520 @ 10/28/26 15:12:53.683\n",
			want:        "Timed out after 900.001s. waiting for the cluster to be ready Expected <string>: Provisioning to equal <string>: Ready [It]",
		},
		{
			name:    "a panic is labelled and keeps the panic value",
			kind:    "panicked",
			message: "runtime error: invalid memory address or nil pointer dereference",
			description: "[PANICKED] Test Panicked\nIn [It] at: /usr/local/go/src/runtime/panic.go:262 @ 10/28/26 15:12:53.683\n\n" +
				"runtime error: invalid memory address or nil pointer dereference\n\nFull Stack Trace\n  main.go:1\n",
			want: "[PANICKED] runtime error: invalid memory address or nil pointer dereference [It panic.go:262]",
		},
		{
			name:    "a suite timeout is labelled",
			kind:    "timedout",
			message: "A suite timeout occurred",
			description: "[TIMEDOUT] A suite timeout occurred\nIn [It] at: /work/e2e/tenant_test.go:534 @ 10/28/26 15:12:53.683\n\n" +
				"This is the Progress Report generated when the suite timeout occurred:\n  ...\n",
			want: "[TIMEDOUT] A suite timeout occurred [It tenant_test.go:534]",
		},
		{
			name:        "an empty message falls back to the description",
			kind:        "failed",
			description: "[FAILED] boom\nIn [It] at: /work/e2e/a_test.go:1 @ 10/28/26 15:12:53.683\n",
			want:        "boom [It a_test.go:1]",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := failureSummary(c.kind, c.message, c.description)
			if got != c.want {
				t.Errorf("failureSummary() = %q\nwant              %q", got, c.want)
			}
			assertKeptWholeByTestGrid(t, got)
		})
	}
}

func TestFailureSummaryCutsALongCauseAtTheEnd(t *testing.T) {
	cause := strings.Repeat("connection refused by the upstream service; ", 10)
	got := failureSummary("failed", cause, "In [It] at: /work/e2e/a_test.go:1 @ 10/28/26 15:12:53.683\n")
	if !strings.HasPrefix(got, "connection refused by the upstream service; connection refused") {
		t.Errorf("summary %q does not start with the cause", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("summary %q does not end in \"...\", so a reader cannot tell it was cut", got)
	}
	assertKeptWholeByTestGrid(t, got)
}

func TestFailureSummaryCutsMultibyteTextOnARuneBoundary(t *testing.T) {
	got := failureSummary("failed", strings.Repeat("ノードの準備ができていません。", 20), "")
	assertKeptWholeByTestGrid(t, got)
}

// assertKeptWholeByTestGrid fails unless TestGrid would store s as it is:
// shorter than its 140-byte limit, valid UTF-8, and one line.
func assertKeptWholeByTestGrid(t *testing.T, s string) {
	t.Helper()
	if len(s) >= testGridMessageLimit {
		t.Errorf("summary is %d bytes, want fewer than %d: %q", len(s), testGridMessageLimit, s)
	}
	if !utf8.ValidString(s) {
		t.Errorf("summary is not valid UTF-8: %q", s)
	}
	if strings.Contains(s, "\n") {
		t.Errorf("summary spans more than one line: %q", s)
	}
}

// ginkgoReport encodes cases the way Ginkgo's JUnit reporter writes them.
func ginkgoReport(t *testing.T, cases ...reporters.JUnitTestCase) []byte {
	t.Helper()
	report := reporters.JUnitTestSuites{
		Tests: len(cases),
		TestSuites: []reporters.JUnitTestSuite{{
			Name:      "NVKE E2E Suite",
			Tests:     len(cases),
			TestCases: cases,
		}},
	}
	var out bytes.Buffer
	out.WriteString(xml.Header)
	encoder := xml.NewEncoder(&out)
	encoder.Indent("  ", "    ")
	if err := encoder.Encode(report); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func decodeReport(t *testing.T, junit []byte) reporters.JUnitTestSuites {
	t.Helper()
	var report reporters.JUnitTestSuites
	if err := xml.Unmarshal(junit, &report); err != nil {
		t.Fatalf("summarized report does not parse: %v", err)
	}
	return report
}

const failureDescription = "[FAILED] Expected success, but got an error:\n    <*errors.errorString | 0xc000abc123>: \n" +
	"    no route to the site\n    \nIn [BeforeAll] at: /work/e2e/site_controller_test.go:227 @ 10/28/26 15:12:53.683\n"

func TestSummarizeFailuresRewritesOnlyTheFailingCase(t *testing.T) {
	junit := ginkgoReport(t,
		reporters.JUnitTestCase{Name: "[It] passes", Status: "passed"},
		reporters.JUnitTestCase{Name: "[It] is skipped", Status: "skipped", Skipped: &reporters.JUnitSkipped{Message: "skipped"}},
		reporters.JUnitTestCase{
			Name:   "[It] fails",
			Status: "failed",
			Failure: &reporters.JUnitFailure{
				Message:     "Expected success, but got an error:\n    <*errors.errorString | 0xc000abc123>: \n    no route to the site\n    ",
				Type:        "failed",
				Description: failureDescription,
			},
			SystemErr: "> Enter [BeforeAll]\n" + failureDescription + "< Exit [BeforeAll]\n",
		},
	)

	got, err := summarizeFailures(junit)
	if err != nil {
		t.Fatal(err)
	}

	report := decodeReport(t, got)
	if len(report.TestSuites) != 1 || len(report.TestSuites[0].TestCases) != 3 {
		t.Fatalf("summarized report has %d suites, want 1 with 3 cases", len(report.TestSuites))
	}
	cases := report.TestSuites[0].TestCases
	if cases[0].Failure != nil || cases[0].Error != nil || cases[0].Status != "passed" {
		t.Errorf("passing case changed: %+v", cases[0])
	}
	if cases[1].Skipped == nil || cases[1].Skipped.Message != "skipped" {
		t.Errorf("skipped case changed: %+v", cases[1])
	}
	failure := cases[2].Failure
	if failure == nil {
		t.Fatal("failing case lost its failure")
	}
	if want := "no route to the site [BeforeAll site_controller_test.go:227]"; failure.Message != want {
		t.Errorf("failure message = %q, want %q", failure.Message, want)
	}
	if failure.Description != "" {
		t.Errorf("failure body = %q, want it empty so TestGrid does not join it onto the summary", failure.Description)
	}
	if failure.Type != "failed" {
		t.Errorf("failure type = %q, want it kept as %q", failure.Type, "failed")
	}
	if n := strings.Count(cases[2].SystemErr, strings.TrimSpace(failureDescription)); n != 1 {
		t.Errorf("system-err holds the full failure %d times, want exactly once (Ginkgo's own copy):\n%s", n, cases[2].SystemErr)
	}
}

func TestSummarizeFailuresCopiesTheFailureIntoSystemErrWhenItIsMissing(t *testing.T) {
	junit := ginkgoReport(t, reporters.JUnitTestCase{
		Name:    "[It] fails",
		Status:  "failed",
		Failure: &reporters.JUnitFailure{Message: "no route to the site", Type: "failed", Description: failureDescription},
	})

	got, err := summarizeFailures(junit)
	if err != nil {
		t.Fatal(err)
	}

	systemErr := decodeReport(t, got).TestSuites[0].TestCases[0].SystemErr
	if !strings.Contains(systemErr, strings.TrimSpace(failureDescription)) {
		t.Errorf("system-err = %q, want it to carry the full failure the body no longer does", systemErr)
	}
}

func TestSummarizeFailuresSummarizesErrorElements(t *testing.T) {
	junit := ginkgoReport(t, reporters.JUnitTestCase{
		Name:   "[It] panics",
		Status: "panicked",
		Error: &reporters.JUnitError{
			Message:     "runtime error: index out of range [3] with length 3",
			Type:        "panicked",
			Description: "[PANICKED] Test Panicked\nIn [It] at: /usr/local/go/src/runtime/panic.go:115 @ 10/28/26 15:12:53.683\n",
		},
	})

	got, err := summarizeFailures(junit)
	if err != nil {
		t.Fatal(err)
	}

	errored := decodeReport(t, got).TestSuites[0].TestCases[0].Error
	if want := "[PANICKED] runtime error: index out of range [3] with length 3 [It panic.go:115]"; errored.Message != want {
		t.Errorf("error message = %q, want %q", errored.Message, want)
	}
	if errored.Description != "" {
		t.Errorf("error body = %q, want it empty", errored.Description)
	}
}

func TestSummarizeFailuresLeavesAReportWithoutFailuresUnchanged(t *testing.T) {
	junit := ginkgoReport(t,
		reporters.JUnitTestCase{Name: "[It] passes", Status: "passed"},
		reporters.JUnitTestCase{Name: "[It] is skipped", Status: "skipped", Skipped: &reporters.JUnitSkipped{Message: "skipped"}},
	)

	got, err := summarizeFailures(junit)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, junit) {
		t.Errorf("a report with no failures changed:\n got %s\nwant %s", got, junit)
	}
}

func TestSummarizeFailuresRejectsAReportThatDoesNotParse(t *testing.T) {
	if _, err := summarizeFailures([]byte("not a JUnit report")); err == nil {
		t.Error("summarizeFailures accepted a report that is not XML")
	}
}

func TestPublishUploadsTheJUnitWithFailuresSummarized(t *testing.T) {
	result := publishTestResult(t, false)
	junitPath := filepath.Join(t.TempDir(), "qualify-e2e.junit.xml")
	junit := ginkgoReport(t, reporters.JUnitTestCase{
		Name:      "[It] fails",
		Status:    "failed",
		Failure:   &reporters.JUnitFailure{Message: "no route to the site", Type: "failed", Description: failureDescription},
		SystemErr: failureDescription,
	})
	if err := os.WriteFile(junitPath, junit, 0o644); err != nil {
		t.Fatal(err)
	}
	uploader := &fakeUploader{}

	if err := (&Destination{Uploader: uploader}).Publish(context.Background(), result, junitPath); err != nil {
		t.Fatal(err)
	}

	if len(uploader.calls) != 3 {
		t.Fatalf("got %d uploads, want 3", len(uploader.calls))
	}
	failure := decodeReport(t, uploader.calls[1].data).TestSuites[0].TestCases[0].Failure
	if want := "no route to the site [BeforeAll site_controller_test.go:227]"; failure.Message != want {
		t.Errorf("uploaded failure message = %q, want %q", failure.Message, want)
	}
}

func TestPublishUploadsAJUnitThatDoesNotParseAsWritten(t *testing.T) {
	result := publishTestResult(t, false)
	junitPath := filepath.Join(t.TempDir(), "qualify-e2e.junit.xml")
	if err := os.WriteFile(junitPath, []byte("<testsuites><testsuite"), 0o644); err != nil {
		t.Fatal(err)
	}
	uploader := &fakeUploader{}

	if err := (&Destination{Uploader: uploader}).Publish(context.Background(), result, junitPath); err != nil {
		t.Fatal(err)
	}

	if len(uploader.calls) != 3 {
		t.Fatalf("got %d uploads, want 3", len(uploader.calls))
	}
	if got := string(uploader.calls[1].data); got != "<testsuites><testsuite" {
		t.Errorf("junit upload carries %q, want the file's own content", got)
	}
}
