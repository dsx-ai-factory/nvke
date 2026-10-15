// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testgrid

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

// testResultFinishedAt is the default FinishedAt testResult stamps -- the
// build name's ordering source, so tests that are not specifically about
// ordering need a stable non-zero value to not trip buildPrefix's "no
// spec.finishedAt" guard.
var testResultFinishedAt = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

func testResult(subject, combinationID, streamName, runID, runAttempt string, capabilities map[string]string) *qualification.Result {
	return &qualification.Result{
		Spec: qualification.ResultSpec{
			Subject:    subject,
			FinishedAt: testResultFinishedAt,
			Identity: qualification.RunIdentity{
				RunID:      runID,
				RunAttempt: runAttempt,
			},
		},
		Status: qualification.ResultStatus{
			Combination: qualification.ResultCombination{
				ID:     combinationID,
				Stream: qualification.ResultStream{Name: streamName, Suite: "NVKE E2E Suite", Capabilities: capabilities},
			},
		},
	}
}

func TestBuildPrefix(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	d := &Destination{}

	got, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("groups/nvke/amd64-1.34/nvke-e2e-suite/%019d-12345-%s/", testResultFinishedAt.UnixNano(), shortHash("cli-v1.0.0"))
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildPrefixRunAttemptIsAppendedToBuild(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "2", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	d := &Destination{}

	got, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("groups/nvke/amd64-1.34/nvke-e2e-suite/%019d-12345-2-%s/", testResultFinishedAt.UnixNano(), shortHash("cli-v1.0.0"))
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The updater drops any build whose name sorts <= the highest one already
// processed -- so a later-finishing leg must always sort after an
// earlier-finishing one, even when the later leg carries a lower run_id (an
// older attempt retried after a newer run already finished).
func TestBuildPrefixBuildNameSortsByFinishedAtNotRunID(t *testing.T) {
	capabilities := map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"}
	earlier := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "99999", "", capabilities)
	earlier.Spec.FinishedAt = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	later := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", capabilities)
	later.Spec.FinishedAt = earlier.Spec.FinishedAt.Add(time.Nanosecond)

	d := &Destination{}
	earlierBuild, err := d.buildPrefix(earlier)
	if err != nil {
		t.Fatal(err)
	}
	laterBuild, err := d.buildPrefix(later)
	if err != nil {
		t.Fatal(err)
	}

	names := []string{laterBuild, earlierBuild}
	sort.Strings(names)
	if names[0] != earlierBuild || names[1] != laterBuild {
		t.Errorf("sorted names = %v, want the earlier-finishing build (run 99999) before the later one (run 12345) despite its lower run_id", names)
	}
}

// A retried qualify publish re-reads the same already-written Result record,
// so it must compute the exact same build path as the attempt it is
// retrying -- not a new one that orphans the first attempt's partial upload.
func TestBuildPrefixIsStableAcrossRepeatedCalls(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	d := &Destination{}

	first, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("buildPrefix(result) = %q then %q, want the same path both times", first, second)
	}
}

func TestBuildPrefixRejectsZeroFinishedAt(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	result.Spec.FinishedAt = time.Time{}
	d := &Destination{}

	if _, err := d.buildPrefix(result); err == nil {
		t.Fatal("buildPrefix returned nil, want an error for a zero FinishedAt")
	}
}

// Without a run_id, Upload's create-only write would silently drop every
// later publish for the same combination/stream instead of erroring, so
// buildPrefix must reject an empty one up front.
func TestBuildPrefixRejectsEmptyRunID(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "", "", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	d := &Destination{}

	if _, err := d.buildPrefix(result); err == nil {
		t.Fatal("buildPrefix returned nil, want an error for an empty run_id")
	}
}

func TestBuildPrefixGroupUsesSubject(t *testing.T) {
	result := testResult("widget", "cli-v1.0.0", "amd64-1.34", "12345", "", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	d := &Destination{}

	got, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	if group := strings.Split(got, "/")[1]; group != "widget" {
		t.Errorf("group is %q, want %q", group, "widget")
	}
}

func TestBuildPrefixDashboardIsStreamNameVerbatim(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "forge-gb200-amd64-ubuntu-24.04-kube-1.36", "12345", "", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	d := &Destination{}

	got, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	if dashboard := strings.Split(got, "/")[2]; dashboard != "forge-gb200-amd64-ubuntu-24.04-kube-1.36" {
		t.Errorf("dashboard is %q, want %q", dashboard, "forge-gb200-amd64-ubuntu-24.04-kube-1.36")
	}
}

func TestBuildPrefixTabIsSlugifiedSuite(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	result.Status.Combination.Stream.Suite = "NVKE E2E Suite"
	d := &Destination{}

	got, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	if tab := strings.Split(got, "/")[3]; tab != "nvke-e2e-suite" {
		t.Errorf("tab is %q, want %q", tab, "nvke-e2e-suite")
	}
}

// Without a suite name, there is nothing meaningful to tab the leg under --
// this is the case `qualify publish` is supposed to skip before ever
// reaching buildPrefix, but buildPrefix rejects it directly too rather than
// trusting every caller to have done that check.
func TestBuildPrefixRejectsEmptySuite(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	result.Status.Combination.Stream.Suite = ""
	d := &Destination{}

	if _, err := d.buildPrefix(result); err == nil {
		t.Fatal("buildPrefix returned nil, want an error for an empty suite")
	}
}

func TestBuildPrefixRejectsMissingCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capabilities map[string]string
	}{
		{"no provider", map[string]string{"gpu": "h100", "kube": "1.34"}},
		{"no gpu", map[string]string{"provider": "forge", "kube": "1.34"}},
		{"no kube", map[string]string{"provider": "forge", "gpu": "h100"}},
		{"no capabilities at all", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", tc.capabilities)
			d := &Destination{}

			if _, err := d.buildPrefix(result); err == nil {
				t.Fatal("buildPrefix returned nil, want an error for missing provider/gpu/kube")
			}
		})
	}
}

// Two combinations that share a stream's gpu/kube (the normal case for a
// qualification matrix -- same hardware, different dependency versions) must
// land at different build paths. Before combination was folded into build,
// these collided and silently overwrote each other's evidence.
func TestBuildPrefixDoesNotCollideAcrossCombinations(t *testing.T) {
	capabilities := map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"}
	d := &Destination{}

	a, err := d.buildPrefix(testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", capabilities))
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.buildPrefix(testResult("nvke", "cli-v2.0.0", "amd64-1.34", "12345", "", capabilities))
	if err != nil {
		t.Fatal(err)
	}

	if a == b {
		t.Errorf("combinations cli-v1.0.0 and cli-v2.0.0 produced the same prefix %q", a)
	}
}

func TestShortHashIsStableAndEightChars(t *testing.T) {
	a := shortHash("cli-v1.0.0_os-image-v0.1.0")
	b := shortHash("cli-v1.0.0_os-image-v0.1.0")
	if a != b {
		t.Errorf("shortHash is not stable: %q != %q", a, b)
	}
	if len(a) != 8 {
		t.Errorf("shortHash is %d characters, want 8", len(a))
	}
}

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"NVKE E2E Suite", "nvke-e2e-suite"},
		{"nvke-e2e-suite", "nvke-e2e-suite"},
		{"  NVKE   E2E  Suite  ", "nvke-e2e-suite"},
		{"NVKE_E2E.Suite!", "nvke-e2e-suite"},
		{"", ""},
	} {
		if got := slug(tc.in); got != tc.want {
			t.Errorf("slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReleaseTagStripsLeadingV(t *testing.T) {
	for _, tc := range []struct{ version, want string }{
		{"v1.0.0", "1.0.0"},
		{"V2.3.4", "2.3.4"},
		{"v0.0.0-beta-abc1234", "0.0.0-beta-abc1234"},
		{"1.0.0", "1.0.0"},
		{"version-with-no-leading-digit", "version-with-no-leading-digit"},
		{"v", "v"},
		{"", ""},
	} {
		if got := releaseTag(tc.version); got != tc.want {
			t.Errorf("releaseTag(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

// upload is one call a fakeUploader captured.
type upload struct {
	path        string
	data        []byte
	contentType string
}

// fakeUploader records every Upload call, in order, instead of writing
// anywhere -- what Publish's tests assert against.
type fakeUploader struct {
	calls []upload
}

func (f *fakeUploader) Upload(_ context.Context, path string, data []byte, contentType string) error {
	f.calls = append(f.calls, upload{path, data, contentType})
	return nil
}

func publishTestResult(t *testing.T, passed bool) *qualification.Result {
	t.Helper()
	started := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	finished := started.Add(45 * time.Minute)
	resultState := "failed"
	if passed {
		resultState = "passed"
	}
	return &qualification.Result{
		Spec: qualification.ResultSpec{
			Subject:    "nvke",
			Version:    "v1.0.0",
			StartedAt:  started,
			FinishedAt: finished,
			Identity: qualification.RunIdentity{
				Repository: "dsx-ai-factory/nvke",
				RunID:      "12345",
				RunAttempt: "1",
				SHA:        "abc123",
				Actor:      "byron",
			},
		},
		Status: qualification.ResultStatus{
			Combination: qualification.ResultCombination{
				ID: "cli-v1.0.0",
				Stream: qualification.ResultStream{
					Name:   "amd64-1.34",
					Result: resultState,
					Suite:  "NVKE E2E Suite",
					RunURL: "https://github.com/dsx-ai-factory/nvke/actions/runs/12345",
					Capabilities: map[string]string{
						"provider": "forge", "gpu": "h100", "kube": "1.34", "arch": "amd64", "ubuntu": "24.04",
					},
				},
			},
		},
	}
}

func TestPublishUploadsInOrderWithTheRightPaths(t *testing.T) {
	result := publishTestResult(t, true)
	junitPath := filepath.Join(t.TempDir(), "qualify-e2e.junit.xml")
	if err := os.WriteFile(junitPath, []byte("<testsuites/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	uploader := &fakeUploader{}
	d := &Destination{Uploader: uploader}

	if err := d.Publish(context.Background(), result, junitPath); err != nil {
		t.Fatal(err)
	}

	wantPrefix, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploader.calls) != 3 {
		t.Fatalf("got %d uploads, want 3", len(uploader.calls))
	}
	wantOrder := []string{wantPrefix + "started.json", wantPrefix + "artifacts/junit.xml", wantPrefix + "finished.json"}
	for i, want := range wantOrder {
		if uploader.calls[i].path != want {
			t.Errorf("upload %d: path %q, want %q", i, uploader.calls[i].path, want)
		}
	}
	if string(uploader.calls[1].data) != "<testsuites/>" {
		t.Errorf("junit upload carries %q, want the file's own content", uploader.calls[1].data)
	}
}

// Retrying Publish against the same already-written Result record (what a
// retried `qualify publish` invocation actually does) must land at the same
// build path both times -- a different path on the second attempt would
// orphan the first attempt's uploads instead of completing them.
func TestPublishIsIdempotentAcrossRetries(t *testing.T) {
	result := publishTestResult(t, true)
	junitPath := filepath.Join(t.TempDir(), "qualify-e2e.junit.xml")
	if err := os.WriteFile(junitPath, []byte("<testsuites/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := &fakeUploader{}
	if err := (&Destination{Uploader: first}).Publish(context.Background(), result, junitPath); err != nil {
		t.Fatal(err)
	}
	second := &fakeUploader{}
	if err := (&Destination{Uploader: second}).Publish(context.Background(), result, junitPath); err != nil {
		t.Fatal(err)
	}

	if len(first.calls) != len(second.calls) {
		t.Fatalf("first attempt uploaded %d objects, second uploaded %d", len(first.calls), len(second.calls))
	}
	for i := range first.calls {
		if first.calls[i].path != second.calls[i].path {
			t.Errorf("upload %d: first attempt wrote %q, retry wrote %q, want the same path", i, first.calls[i].path, second.calls[i].path)
		}
	}
}

// A run that never produced a Ginkgo report has no JUnit file either -- the
// errored record `qualify result` already wrote still needs to reach
// TestGrid, so Publish must not error just because the artifact it would
// have carried never existed.
func TestPublishSkipsJUnitWhenMissing(t *testing.T) {
	result := publishTestResult(t, false)
	junitPath := filepath.Join(t.TempDir(), "does-not-exist.xml")
	uploader := &fakeUploader{}
	d := &Destination{Uploader: uploader}

	if err := d.Publish(context.Background(), result, junitPath); err != nil {
		t.Fatal(err)
	}

	if len(uploader.calls) != 2 {
		t.Fatalf("got %d uploads, want 2 (started.json, finished.json)", len(uploader.calls))
	}
	for _, call := range uploader.calls {
		if strings.Contains(call.path, "junit") {
			t.Errorf("uploaded %q, want no junit.xml artifact when the file does not exist", call.path)
		}
	}
	if !strings.HasSuffix(uploader.calls[0].path, "started.json") || !strings.HasSuffix(uploader.calls[1].path, "finished.json") {
		t.Errorf("uploads = %v, want started.json then finished.json", uploader.calls)
	}
}

func TestPublishFinishedJSONReflectsPassFail(t *testing.T) {
	for _, tc := range []struct {
		passed     bool
		wantResult string
	}{
		{true, "SUCCESS"},
		{false, "FAILURE"},
	} {
		t.Run(tc.wantResult, func(t *testing.T) {
			result := publishTestResult(t, tc.passed)
			junitPath := filepath.Join(t.TempDir(), "qualify-e2e.junit.xml")
			if err := os.WriteFile(junitPath, []byte("<testsuites/>"), 0o644); err != nil {
				t.Fatal(err)
			}
			uploader := &fakeUploader{}
			d := &Destination{Uploader: uploader}

			if err := d.Publish(context.Background(), result, junitPath); err != nil {
				t.Fatal(err)
			}

			var got finished
			if err := json.Unmarshal(uploader.calls[2].data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Passed != tc.passed {
				t.Errorf("passed = %v, want %v", got.Passed, tc.passed)
			}
			if got.Result != tc.wantResult {
				t.Errorf("result = %q, want %q", got.Result, tc.wantResult)
			}
			if got.Timestamp != result.Spec.FinishedAt.Unix() {
				t.Errorf("timestamp = %d, want %d", got.Timestamp, result.Spec.FinishedAt.Unix())
			}
		})
	}
}

func TestPublishMetadataFlattensCapabilitiesAndFixesReleaseStream(t *testing.T) {
	result := publishTestResult(t, true)
	junitPath := filepath.Join(t.TempDir(), "qualify-e2e.junit.xml")
	if err := os.WriteFile(junitPath, []byte("<testsuites/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	uploader := &fakeUploader{}
	d := &Destination{Uploader: uploader}

	if err := d.Publish(context.Background(), result, junitPath); err != nil {
		t.Fatal(err)
	}

	var got started
	if err := json.Unmarshal(uploader.calls[0].data, &got); err != nil {
		t.Fatal(err)
	}
	m := got.Metadata
	want := meta{
		Subject:       "nvke",
		Version:       "v1.0.0",
		Combination:   "cli-v1.0.0",
		Stream:        "amd64-1.34",
		Repository:    "dsx-ai-factory/nvke",
		RunID:         "12345",
		RunAttempt:    "1",
		SHA:           "abc123",
		Actor:         "byron",
		ReleaseStream: "nvke-release",
		ReleaseTag:    "1.0.0",
		Provider:      "forge",
		GPU:           "h100",
		Arch:          "amd64",
		Ubuntu:        "24.04",
		Kube:          "1.34",
		RunURL:        "https://github.com/dsx-ai-factory/nvke/actions/runs/12345",
		Commit:        "abc123",
	}
	if m != want {
		t.Errorf("metadata = %+v, want %+v", m, want)
	}

	// TestGrid's own metadata.Version() reader only recognizes "revision",
	// "repo-commit", or "job-version" -- a "commit" key is metadata it never
	// reads back.
	var raw struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(uploader.calls[0].data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw.Metadata["repo-commit"]; !ok {
		t.Errorf("metadata has no repo-commit key: %v", raw.Metadata)
	}
	if _, ok := raw.Metadata["commit"]; ok {
		t.Errorf("metadata still has a commit key, which TestGrid never reads: %v", raw.Metadata)
	}
}

// A second subject must not render as nvke's own release stream in the
// Qualification Matrix -- that is the whole reason releaseStream derives
// from Spec.Subject instead of being a fixed "nvke-release" constant.
func TestPublishDerivesReleaseStreamFromSubject(t *testing.T) {
	result := publishTestResult(t, true)
	result.Spec.Subject = "widget"
	junitPath := filepath.Join(t.TempDir(), "qualify-e2e.junit.xml")
	if err := os.WriteFile(junitPath, []byte("<testsuites/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	uploader := &fakeUploader{}
	d := &Destination{Uploader: uploader}

	if err := d.Publish(context.Background(), result, junitPath); err != nil {
		t.Fatal(err)
	}

	var got started
	if err := json.Unmarshal(uploader.calls[0].data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Metadata.ReleaseStream != "widget-release" {
		t.Errorf("release_stream = %q, want widget-release", got.Metadata.ReleaseStream)
	}
}

func TestPublishPropagatesBuildPrefixErrors(t *testing.T) {
	result := publishTestResult(t, true)
	result.Status.Combination.Stream.Capabilities = nil // no gpu/kube
	junitPath := filepath.Join(t.TempDir(), "qualify-e2e.junit.xml")
	uploader := &fakeUploader{}
	d := &Destination{Uploader: uploader}

	if err := d.Publish(context.Background(), result, junitPath); err == nil {
		t.Fatal("Publish returned nil, want an error from buildPrefix")
	}
	if len(uploader.calls) != 0 {
		t.Errorf("got %d uploads, want 0 -- buildPrefix should fail before any upload", len(uploader.calls))
	}
}
