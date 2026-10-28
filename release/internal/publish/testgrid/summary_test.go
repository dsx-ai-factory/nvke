// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testgrid

import (
	"bytes"
	"context"
	"encoding/xml"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/onsi/ginkgo/v2/reporters"
	"github.com/onsi/ginkgo/v2/types"
)

func TestFailureSummary(t *testing.T) {
	cases := []struct {
		name, kind, cause, message, description, want string
	}{
		{
			name:  "a recorded cause is the summary",
			kind:  "failed",
			cause: "cluster 4f2a9c1e has 2 control-plane nodes KubernetesReady; want 3",
			message: "Timed out after 5400.001s.\nthis spec needs a settled three-node control plane\n" +
				"Expected success, but got an error:\n    <*errors.errorString | 0xc000abc123>: \n" +
				"    cluster 4f2a9c1e has 2 control-plane nodes KubernetesReady; want 3\n    ",
			want: "cluster 4f2a9c1e has 2 control-plane nodes KubernetesReady; want 3",
		},
		{
			name:    "without a recorded cause, the message is the summary, on one line",
			kind:    "failed",
			message: "Expected\n    <int>: 3\nto equal\n    <int>: 2",
			want:    "Expected <int>: 3 to equal <int>: 2",
		},
		{
			name:  "UUIDs are cut to their first block",
			kind:  "failed",
			cause: "forge has no usable allocation for this run: cpu.x86_64 (0f4e2a91-1c3b-4d5e-8f60-7a8b9c0d1e2f): need 2, unusedUsable=1 (total=7 used=6 unused=1)",
			want:  "forge has no usable allocation for this run: cpu.x86_64 (0f4e2a91...): need 2, unusedUsable=1 (total=7 used=6 unused=1)",
		},
		{
			name:    "a panic is labelled and keeps the panic value",
			kind:    "panicked",
			message: "runtime error: invalid memory address or nil pointer dereference",
			want:    "[PANICKED] runtime error: invalid memory address or nil pointer dereference",
		},
		{
			name:    "a suite timeout is labelled",
			kind:    "timedout",
			message: "A suite timeout occurred",
			want:    "[TIMEDOUT] A suite timeout occurred",
		},
		{
			name:  "terminal colour codes are dropped",
			kind:  "failed",
			cause: "clusterctl init: \x1b[31merror\x1b[0m: provider \"nvke\" not found",
			want:  "clusterctl init: error: provider \"nvke\" not found",
		},
		{
			name:    "so are colour codes whose ESC XML replaced with U+FFFD",
			kind:    "failed",
			message: "clusterctl init: \uFFFD[31merror\uFFFD[0m: provider \"nvke\" not found",
			want:    "clusterctl init: error: provider \"nvke\" not found",
		},
		{
			name:    "other control characters become spaces",
			kind:    "failed",
			message: "connection reset\x00by peer\x07",
			want:    "connection reset by peer",
		},
		{
			name:        "an empty message falls back to the body",
			kind:        "failed",
			description: "[FAILED] boom\nIn [It] at: /work/e2e/a_test.go:1 @ 10/28/26 15:12:53.683\n",
			want:        "[FAILED] boom In [It] at: /work/e2e/a_test.go:1 @ 10/28/26 15:12:53.683",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := failureSummary(c.kind, c.cause, c.message, c.description)
			if got != c.want {
				t.Errorf("failureSummary() = %q\nwant              %q", got, c.want)
			}
			assertKeptWholeByTestGrid(t, got)
		})
	}
}

func TestFailureSummaryCutsALongCauseAtTheEnd(t *testing.T) {
	cause := strings.Repeat("connection refused by the upstream service; ", 10)
	got := failureSummary("failed", cause, "", "")
	if !strings.HasPrefix(got, "connection refused by the upstream service; connection refused") {
		t.Errorf("summary %q does not start with the cause", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("summary %q does not end in \"...\", so a reader cannot tell it was cut", got)
	}
	assertKeptWholeByTestGrid(t, got)
}

func TestFailureSummaryCutsMultibyteTextOnARuneBoundary(t *testing.T) {
	got := failureSummary("failed", "", strings.Repeat("ノードの準備ができていません。", 20), "")
	assertKeptWholeByTestGrid(t, got)
}

func TestRecordedCause(t *testing.T) {
	failure := "Expected success, but got an error:\n    <*fmt.wrapError | 0xc000abc123>: \n" +
		"    allocating machines: 403 Forbidden\n    {\"error\":\"project quota exceeded\"}\n    "
	cases := []struct {
		name       string
		properties *reporters.JUnitProperties
		want       string
	}{
		{name: "no properties", want: ""},
		{
			name: "the cause the failure carries, whole and on one line",
			properties: causes(
				`"allocating machines: 403 Forbidden\n{\"error\":\"project quota exceeded\"}"`,
			),
			want: `allocating machines: 403 Forbidden {"error":"project quota exceeded"}`,
		},
		{
			name: "a cause recorded for a later failure of the same spec is passed over",
			properties: causes(
				`"deleting the cluster: context canceled"`,
				`"allocating machines: 403 Forbidden\n{\"error\":\"project quota exceeded\"}"`,
			),
			want: `allocating machines: 403 Forbidden {"error":"project quota exceeded"}`,
		},
		{
			name:       "a value that is not a JSON string is ignored",
			properties: causes(`allocating machines: 403 Forbidden`),
			want:       "",
		},
		{
			name: "other properties are ignored",
			properties: &reporters.JUnitProperties{Properties: []reporters.JUnitProperty{
				{Name: "stage", Value: `"allocating machines: 403 Forbidden"`},
			}},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := &reporters.JUnitTestCase{Properties: c.properties}
			if got := recordedCause(tc, failure); got != c.want {
				t.Errorf("recordedCause() = %q, want %q", got, c.want)
			}
		})
	}
}

// Ginkgo writes the cause into a property as JSON, which keeps an ESC, but
// writes the failure message as XML text, which cannot hold one and gets
// U+FFFD instead. The cause still matches.
func TestRecordedCauseMatchesWhereXMLReplacedAnEscape(t *testing.T) {
	failure := "Expected success, but got an error:\n    <*errors.errorString | 0xc000abc123>: \n" +
		"    clusterctl init: \uFFFD[31merror\uFFFD[0m: provider \"nvke\" not found\n    "
	tc := &reporters.JUnitTestCase{Properties: causes(`"clusterctl init: \u001b[31merror\u001b[0m: provider \"nvke\" not found"`)}
	if got, want := recordedCause(tc, failure), `clusterctl init: error: provider "nvke" not found`; got != want {
		t.Errorf("recordedCause() = %q, want %q", got, want)
	}

	// A lone control character, outside any colour code, is replaced too.
	failure = "Unexpected error:\n    <*errors.errorString | 0xc000abc123>: \n    connection reset\uFFFDby peer\n    \noccurred"
	tc = &reporters.JUnitTestCase{Properties: causes(`"connection reset\u0000by peer"`)}
	if got, want := recordedCause(tc, failure), "connection reset by peer"; got != want {
		t.Errorf("recordedCause() = %q, want %q", got, want)
	}
}

// causes is the properties a test case carries when the e2e suite recorded
// each value, already JSON-encoded, as its failure cause.
func causes(values ...string) *reporters.JUnitProperties {
	properties := &reporters.JUnitProperties{}
	for _, v := range values {
		properties.Properties = append(properties.Properties, reporters.JUnitProperty{Name: failureCauseProperty, Value: v})
	}
	return properties
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

const (
	failureMessage = "Expected success, but got an error:\n    <*errors.errorString | 0xc000abc123>: \n" +
		"    no route to the site\n    "
	failureDescription = "[FAILED] " + failureMessage +
		"\nIn [BeforeAll] at: /work/e2e/site_controller_test.go:227 @ 10/28/26 15:12:53.683\n"
)

func TestSummarizeFailuresRewritesOnlyTheFailingCase(t *testing.T) {
	junit := ginkgoReport(t,
		reporters.JUnitTestCase{Name: "[It] passes", Status: "passed"},
		reporters.JUnitTestCase{Name: "[It] is skipped", Status: "skipped", Skipped: &reporters.JUnitSkipped{Message: "skipped"}},
		reporters.JUnitTestCase{
			Name:       "[It] fails",
			Status:     "failed",
			Failure:    &reporters.JUnitFailure{Message: failureMessage, Type: "failed", Description: failureDescription},
			SystemErr:  "> Enter [BeforeAll]\n" + failureDescription + "< Exit [BeforeAll]\n",
			Properties: causes(`"no route to the site"`),
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
	if want := "no route to the site"; failure.Message != want {
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
	if cases[2].Properties == nil || len(cases[2].Properties.Properties) != 1 {
		t.Errorf("properties = %+v, want the recorded cause kept", cases[2].Properties)
	}
}

func TestSummarizeFailuresUsesTheMessageWhenNoCauseWasRecorded(t *testing.T) {
	junit := ginkgoReport(t, reporters.JUnitTestCase{
		Name:    "[It] fails",
		Status:  "failed",
		Failure: &reporters.JUnitFailure{Message: failureMessage, Type: "failed", Description: failureDescription},
	})

	got, err := summarizeFailures(junit)
	if err != nil {
		t.Fatal(err)
	}

	failure := decodeReport(t, got).TestSuites[0].TestCases[0].Failure
	if want := "Expected success, but got an error: <*errors.errorString | 0xc000abc123>: no route to the site"; failure.Message != want {
		t.Errorf("failure message = %q, want %q", failure.Message, want)
	}
	if failure.Description != "" {
		t.Errorf("failure body = %q, want it empty", failure.Description)
	}
}

// The suite records the cause with AddReportEntry; this reads it back from a
// report Ginkgo's own JUnit reporter wrote, so the property's name and
// encoding are the ones Ginkgo produces.
func TestSummarizeFailuresReadsTheCauseFromGinkgosJUnit(t *testing.T) {
	cause := "cluster 12345678-1234-1234-1234-123456789abc has 2 control-plane nodes KubernetesReady; want 3"
	report := types.Report{
		SuiteDescription: "NVKE E2E Suite",
		SpecReports: types.SpecReports{{
			LeafNodeType: types.NodeTypeIt,
			LeafNodeText: "refuses deleting a healthy control-plane node but removes an unreachable one",
			State:        types.SpecStateFailed,
			Failure: types.Failure{
				Message: "Timed out after 5400.001s.\nthis spec needs a settled three-node control plane\n" +
					"Expected success, but got an error:\n    <*errors.errorString | 0xc000abc123>: \n    " + cause + "\n    ",
				Location:        types.NewCustomCodeLocation("cp_controller_test.go:361"),
				FailureNodeType: types.NodeTypeIt,
			},
			ReportEntries: types.ReportEntries{{
				Name:       failureCauseProperty,
				Value:      types.WrapEntryValue(cause),
				Visibility: types.ReportEntryVisibilityNever,
			}},
		}},
	}
	path := filepath.Join(t.TempDir(), "junit.xml")
	if err := reporters.GenerateJUnitReport(report, path); err != nil {
		t.Fatal(err)
	}
	junit, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	got, err := summarizeFailures(junit)
	if err != nil {
		t.Fatal(err)
	}

	failure := decodeReport(t, got).TestSuites[0].TestCases[0].Failure
	if want := "cluster 12345678... has 2 control-plane nodes KubernetesReady; want 3"; failure.Message != want {
		t.Errorf("failure message = %q, want %q", failure.Message, want)
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

func TestSummarizeFailuresKeepsAMessageThatHasNoBody(t *testing.T) {
	message := "creating the cluster: " + strings.Repeat("the API server did not answer; ", 10)
	junit := ginkgoReport(t, reporters.JUnitTestCase{
		Name:    "[It] fails",
		Status:  "failed",
		Failure: &reporters.JUnitFailure{Message: message, Type: "failed"},
	})

	got, err := summarizeFailures(junit)
	if err != nil {
		t.Fatal(err)
	}

	tc := decodeReport(t, got).TestSuites[0].TestCases[0]
	if len(tc.Failure.Message) >= testGridMessageLimit {
		t.Errorf("failure message is %d bytes, want it summarized", len(tc.Failure.Message))
	}
	if !strings.Contains(tc.SystemErr, strings.TrimSpace(message)) {
		t.Errorf("system-err = %q, want it to keep the full message the summary cut", tc.SystemErr)
	}
}

// On a spec timeout Ginkgo folds the additional failure into the failure body,
// but its timeline in system-err draws them as separate entries, so the body
// never matches system-err as a whole. The message does.
func TestSummarizeFailuresDoesNotRepeatGinkgosCopy(t *testing.T) {
	progress := "In [It] at: /work/e2e/a_test.go:10 @ 10/28/26 15:12:53.833\n\n" +
		"This is the Progress Report generated when the spec timeout occurred:\n  waits (Spec Runtime: 150ms)\n"
	timeline := "> Enter [It] waits - /work/e2e/a_test.go:10 @ 10/28/26 15:12:53.683\n" +
		"[TIMEDOUT] A spec timeout occurred\n" + progress +
		"failure-cause - /work/e2e/a_test.go:13 @ 10/28/26 15:12:53.834\n" +
		"  cluster c1 has 2 control-plane nodes KubernetesReady; want 3\n" +
		"< Exit [It] waits - /work/e2e/a_test.go:10 @ 10/28/26 15:12:53.835 (152ms)\n"
	body := "[TIMEDOUT] A spec timeout occurred\n" + progress +
		"\nThere were additional failures detected after the initial failure. These are visible in the timeline\n"
	junit := ginkgoReport(t, reporters.JUnitTestCase{
		Name:      "[It] waits",
		Status:    "timedout",
		Failure:   &reporters.JUnitFailure{Message: "A spec timeout occurred", Type: "timedout", Description: body},
		SystemErr: timeline,
	})

	got, err := summarizeFailures(junit)
	if err != nil {
		t.Fatal(err)
	}

	if systemErr := decodeReport(t, got).TestSuites[0].TestCases[0].SystemErr; systemErr != timeline {
		t.Errorf("system-err changed, though it already held Ginkgo's copy of the failure:\n%s", systemErr)
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
	if want := "[PANICKED] runtime error: index out of range [3] with length 3"; errored.Message != want {
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
		Name:       "[It] fails",
		Status:     "failed",
		Failure:    &reporters.JUnitFailure{Message: failureMessage, Type: "failed", Description: failureDescription},
		SystemErr:  failureDescription,
		Properties: causes(`"no route to the site"`),
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
	if want := "no route to the site"; failure.Message != want {
		t.Errorf("uploaded failure message = %q, want %q", failure.Message, want)
	}
}

func TestPublishUploadsAJUnitThatDoesNotParseAsWritten(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
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
	if !strings.Contains(logged.String(), "level=WARN") || !strings.Contains(logged.String(), "without failure summaries") {
		t.Errorf("logged %q, want a warning that the report went up without summaries", logged.String())
	}
}
