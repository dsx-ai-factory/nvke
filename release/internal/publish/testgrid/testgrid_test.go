// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testgrid

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

// fixedClock is a Destination.Now that always returns t, for a build name
// test's timestamp to be reproducible instead of racing the real clock.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// newFormatDashboard mirrors testgrid's own tools/config-gen/main.go
// isNewFormatDashboard regex. config-gen silently drops any discovered
// dashboard that does not match this -- not an error, just never rendered in
// the UI -- so a dashboard name that stops matching it is a regression this
// test catches locally instead of only on a live TestGrid check.
var newFormatDashboard = regexp.MustCompile(`-[a-z0-9]+-(e2e|tenant)(-(nclave|cpu))?$`)

func testResult(subject, combinationID, streamName, runID, runAttempt string, capabilities map[string]string) *qualification.Result {
	return &qualification.Result{
		Spec: qualification.ResultSpec{
			Subject: subject,
			Identity: qualification.ResultIdentity{
				RunID:      runID,
				RunAttempt: runAttempt,
			},
		},
		Status: qualification.ResultStatus{
			Combinations: []qualification.ResultCombination{{
				ID:      combinationID,
				Streams: []qualification.ResultStream{{Name: streamName, Capabilities: capabilities}},
			}},
		},
	}
}

func TestBuildPrefix(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	clock := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	d := &Destination{Now: fixedClock(clock)}

	got, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("groups/forge-prod/h100-v1.34-nvke-e2e/amd64-1.34/%019d-12345-%s/", clock.UnixNano(), shortHash("cli-v1.0.0"))
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildPrefixRunAttemptIsAppendedToBuild(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "2", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	clock := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	d := &Destination{Now: fixedClock(clock)}

	got, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("groups/forge-prod/h100-v1.34-nvke-e2e/amd64-1.34/%019d-12345-2-%s/", clock.UnixNano(), shortHash("cli-v1.0.0"))
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The updater drops any build whose name sorts <= the highest one already
// processed -- so a later publish must always sort after an earlier one,
// even when the later publish carries a lower run_id (an older attempt
// retried after a newer run already published).
func TestBuildPrefixBuildNameSortsByUploadTimeNotRunID(t *testing.T) {
	capabilities := map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"}
	earlier := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Nanosecond)

	dEarlier := &Destination{Now: fixedClock(earlier)}
	earlierBuild, err := dEarlier.buildPrefix(testResult("nvke", "cli-v1.0.0", "amd64-1.34", "99999", "", capabilities))
	if err != nil {
		t.Fatal(err)
	}
	dLater := &Destination{Now: fixedClock(later)}
	laterBuild, err := dLater.buildPrefix(testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", capabilities))
	if err != nil {
		t.Fatal(err)
	}

	names := []string{laterBuild, earlierBuild}
	sort.Strings(names)
	if names[0] != earlierBuild || names[1] != laterBuild {
		t.Errorf("sorted names = %v, want the earlier-published build (run 99999) before the later one (run 12345) despite its lower run_id", names)
	}
}

func TestBuildPrefixGroupUsesCapabilitiesProvider(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", map[string]string{"provider": "some-other-provider", "gpu": "h100", "kube": "1.34"})
	d := &Destination{}

	got, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	if group := strings.Split(got, "/")[1]; group != "some-other-provider-prod" {
		t.Errorf("group is %q, want %q", group, "some-other-provider-prod")
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

// buildPrefix's dashboard segment must match config-gen's new-format
// dashboard naming, or every result published here silently never appears in
// the TestGrid UI while still sitting in the bucket.
func TestBuildPrefixDashboardMatchesConfigGenNewFormat(t *testing.T) {
	result := testResult("nvke", "cli-v1.0.0", "amd64-1.34", "12345", "", map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"})
	d := &Destination{}

	prefix, err := d.buildPrefix(result)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.Trim(prefix, "/"), "/")
	if len(parts) < 3 {
		t.Fatalf("prefix %q does not have a dashboard segment", prefix)
	}
	dashboard := parts[2] // groups/{group}/{dashboard}/...
	if !newFormatDashboard.MatchString(dashboard) {
		t.Errorf("dashboard %q does not match config-gen's new-format regex %s", dashboard, newFormatDashboard)
	}
}

// Two combinations that share a stream's gpu/kube (the normal case for a
// qualification matrix -- same hardware, different dependency versions) must
// land at different build paths. Before combination was folded into build,
// these collided and silently overwrote each other's evidence.
func TestBuildPrefixDoesNotCollideAcrossCombinations(t *testing.T) {
	capabilities := map[string]string{"provider": "forge", "gpu": "h100", "kube": "1.34"}
	clock := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	d := &Destination{Now: fixedClock(clock)}

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
	resultState := "FAIL"
	if passed {
		resultState = "PASS"
	}
	return &qualification.Result{
		Spec: qualification.ResultSpec{
			Subject:    "nvke",
			Version:    "v1.0.0",
			StartedAt:  started,
			FinishedAt: finished,
			Identity: qualification.ResultIdentity{
				Repository: "dsx-ai-factory/nvke",
				RunID:      "12345",
				RunAttempt: "1",
				SHA:        "abc123",
				Actor:      "byron",
			},
		},
		Status: qualification.ResultStatus{
			Combinations: []qualification.ResultCombination{{
				ID: "cli-v1.0.0",
				Streams: []qualification.ResultStream{{
					Name:   "amd64-1.34",
					Result: resultState,
					RunURL: "https://github.com/dsx-ai-factory/nvke/actions/runs/12345",
					Capabilities: map[string]string{
						"provider": "forge", "gpu": "h100", "kube": "1.34", "arch": "amd64", "ubuntu": "24.04",
					},
				}},
			}},
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
	d := &Destination{Uploader: uploader, Now: fixedClock(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))}

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

// A run that never produced a Ginkgo report has no JUnit file either -- the
// FAIL record `qualify result` already wrote still needs to reach TestGrid,
// so Publish must not error just because the artifact it would have carried
// never existed.
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

func TestPublishPropagatesBuildPrefixErrors(t *testing.T) {
	result := publishTestResult(t, true)
	result.Status.Combinations[0].Streams[0].Capabilities = nil // no gpu/kube
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
