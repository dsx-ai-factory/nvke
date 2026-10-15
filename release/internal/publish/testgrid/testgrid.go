// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testgrid is the TestGrid Destination: it writes started.json, a
// copy of the JUnit report, then finished.json to the shape TestGrid's
// updater reads (groups/{group}/{dashboard}/{tab}/{build}/), with group as
// the subject, dashboard as the full stream name (e.g.
// forge-gb200-amd64-ubuntu-24.04-kube-1.36), and tab as the Ginkgo suite
// that ran, slugged.
package testgrid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

// Uploader writes one object's bytes to a destination-defined location.
// The seam that keeps Destination testable without a real GCS bucket.
type Uploader interface {
	Upload(ctx context.Context, path string, data []byte, contentType string) error
}

// Destination is the TestGrid Destination.
type Destination struct {
	Uploader Uploader
}

// Name is the --destination value that selects this Destination.
func (d *Destination) Name() string { return "testgrid" }

type started struct {
	Timestamp int64 `json:"timestamp"`
	Metadata  meta  `json:"metadata"`
}

type finished struct {
	Timestamp int64  `json:"timestamp"`
	Passed    bool   `json:"passed"`
	Result    string `json:"result"`
	Metadata  meta   `json:"metadata"`
}

// releaseStream is the Release Source value config-gen's ColumnHeader
// surfaces on every build (tools/config-gen/main.go), and the one the
// Qualification Matrix's SPA groups under its own project entry. Derived
// from subject, not fixed: Subject's own doc already anticipates a second,
// per-component subject beside the release-wide one at
// release/qualification.yaml, and a fixed value would stamp that subject's
// results as "nvke-release" too, indistinguishable from nvke's own in the
// Qualification Matrix.
func releaseStream(subject string) string {
	return subject + "-release"
}

// meta is the subset of TestGrid's started.json/finished.json metadata this
// Destination fills, from the QualificationResult's own fields.
//
// ReleaseStream and ReleaseTag are not cosmetic: config-gen declares
// ColumnHeader{ConfigurationValue: "release_stream"/"release_tag"} on every
// TestGroup (tools/config-gen/main.go), and the SPA's Qualification Matrix
// page drops any build column whose release_tag comes back empty or
// "missing" (aggregator/fanout.js hasReleaseTag). Omitting these two fields
// doesn't error -- the build still renders fine on the plain TestGrid
// dashboard page -- it just never appears in the Qualification Matrix.
// ReleaseTag also has its leading "v" stripped (see releaseTag) -- TestGrid's
// own semver sort reads a leading non-digit as making the major version
// parse as 0, which would sort every nvke release equally regardless of its
// real version.
//
// Metadata must stay flat strings: TestGrid's Metadata.Strings() drops
// nested maps, so Capabilities is flattened into gpu/arch/ubuntu/kube here
// rather than passed through as a map.
//
// Commit is keyed "repo-commit", not "commit": TestGrid's own
// metadata.Version() reader only recognizes "revision", "repo-commit", or
// "job-version" -- any other key name is metadata TestGrid never reads back.
type meta struct {
	Subject       string `json:"subject"`
	Version       string `json:"version"`
	Combination   string `json:"combination"`
	Stream        string `json:"stream"`
	Repository    string `json:"repository"`
	RunID         string `json:"run_id"`
	RunAttempt    string `json:"run_attempt"`
	SHA           string `json:"sha"`
	Actor         string `json:"actor"`
	ReleaseStream string `json:"release_stream"`
	ReleaseTag    string `json:"release_tag"`
	Provider      string `json:"provider"`
	GPU           string `json:"gpu,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Ubuntu        string `json:"ubuntu,omitempty"`
	Kube          string `json:"kube,omitempty"`
	RunURL        string `json:"run_url,omitempty"`
	Commit        string `json:"repo-commit,omitempty"`
}

// Publish writes started.json, a copy of the JUnit report, then finished.json
// (in that order -- finished.json is never written unless everything ahead of
// it uploaded successfully) under a build prefix derived from result's
// combination and stream. Capabilities (gpu/kube/arch/ubuntu) come from
// result's own Status.Combination.Stream.Capabilities, stamped there by
// `qualify result` -- not looked up separately.
//
// junitPath may not exist: a run that never produced a Ginkgo report (a Forge
// provisioning failure, a wedged runner) has no JUnit either, and `qualify
// result` already wrote an errored record with an infrastructure-failure summary
// for exactly that case. Publish still uploads started.json/finished.json so
// that record reaches TestGrid instead of silently never publishing; it just
// skips the junit.xml artifact, since there is genuinely nothing to upload.
func (d *Destination) Publish(ctx context.Context, result *qualification.Result, junitPath string) error {
	combo := result.Status.Combination
	stream := combo.Stream
	passed := stream.Result == qualification.ResultPassed
	capabilities := stream.Capabilities

	m := meta{
		Subject:       result.Spec.Subject,
		Version:       result.Spec.Version,
		Combination:   combo.ID,
		Stream:        stream.Name,
		Repository:    result.Spec.Identity.Repository,
		RunID:         result.Spec.Identity.RunID,
		RunAttempt:    result.Spec.Identity.RunAttempt,
		SHA:           result.Spec.Identity.SHA,
		Actor:         result.Spec.Identity.Actor,
		ReleaseStream: releaseStream(result.Spec.Subject),
		ReleaseTag:    releaseTag(result.Spec.Version),
		Provider:      capabilities["provider"],
		GPU:           capabilities["gpu"],
		Arch:          capabilities["arch"],
		Ubuntu:        capabilities["ubuntu"],
		Kube:          capabilities["kube"],
		RunURL:        stream.RunURL,
		Commit:        result.Spec.Identity.SHA,
	}

	prefix, err := d.buildPrefix(result)
	if err != nil {
		return err
	}

	startedBytes, err := json.MarshalIndent(started{
		Timestamp: result.Spec.StartedAt.Unix(),
		Metadata:  m,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding started.json: %w", err)
	}
	if err := d.Uploader.Upload(ctx, prefix+"started.json", startedBytes, "application/json"); err != nil {
		return fmt.Errorf("uploading started.json: %w", err)
	}

	junitBytes, err := os.ReadFile(junitPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Nothing ran, so there is nothing to upload -- the errored record
		// above already carries why.
	case err != nil:
		return fmt.Errorf("reading JUnit report: %w", err)
	default:
		if err := d.Uploader.Upload(ctx, prefix+"artifacts/junit.xml", junitBytes, "application/xml"); err != nil {
			return fmt.Errorf("uploading junit.xml: %w", err)
		}
	}

	resultStr := "SUCCESS"
	if !passed {
		resultStr = "FAILURE"
	}
	finishedBytes, err := json.MarshalIndent(finished{
		Timestamp: result.Spec.FinishedAt.Unix(),
		Passed:    passed,
		Result:    resultStr,
		Metadata:  m,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding finished.json: %w", err)
	}
	if err := d.Uploader.Upload(ctx, prefix+"finished.json", finishedBytes, "application/json"); err != nil {
		return fmt.Errorf("uploading finished.json: %w", err)
	}
	return nil
}

// buildPrefix is the group/dashboard/tab/build GCS path for this leg.
//
// group is the subject (e.g. "nvke"), the same derivation releaseStream
// uses: a fixed value would stamp a second, per-component subject's results
// under the first subject's group, indistinguishable from the one it
// actually belongs to.
//
// dashboard is the stream's own name verbatim (e.g.
// forge-gb200-amd64-ubuntu-24.04-kube-1.36) -- no required suffix on this
// side; TestGrid's config-gen discovers dashboards by group rather than by a
// dashboard-name pattern.
//
// tab is the Ginkgo suite that ran (stream.Suite, e.g. "NVKE E2E Suite"),
// slugged. Not the stream name: multiple streams run the same suite, so
// group+dashboard already disambiguates the configuration under test, and
// tab is which suite ran against it -- a second suite later becomes a new
// tab on the same dashboard rather than a new dashboard.
//
// build is {FinishedAt as 19-digit nanoseconds}-{run_id}-{run_attempt}-
// {short hash of combination ID}. The updater drops any build whose name
// sorts <= the highest one it has already processed (pkg/updater/read.go,
// ListBuilds), so the sort key has to track the order builds actually land
// in GCS -- not run_id, which two overlapping runs on different refs, or a
// retried older attempt publishing after a newer one already did, can put
// out of order. A fixed-width zero-padded UnixNano sorts lexicographically
// the same as it sorts numerically.
//
// run_id is required, not merely appended when present: Upload is
// create-only, so two publishes that land on the same build path silently
// agree -- the second is dropped, not an error -- and a timestamp alone is
// not enough to rule that out. A result published outside GitHub Actions
// (no GITHUB_RUN_ID) would otherwise collapse every later publish for the
// same combination/stream onto one ambiguous path, each one printing
// "published" while doing nothing.
//
// The timestamp comes from the Result record's own FinishedAt, not from
// reading the clock when Publish runs: FinishedAt is set once by `qualify
// result` and persisted to the file `qualify publish` reads, so a retried
// `qualify publish` invocation against that same file computes the exact
// same build path as the attempt it is retrying, rather than a new one that
// orphans the first attempt's partially-uploaded files forever. A real
// upload-time clock would make every retry start a fresh, permanently
// incomplete build instead.
func (d *Destination) buildPrefix(result *qualification.Result) (string, error) {
	combo := result.Status.Combination
	stream := combo.Stream
	capabilities := stream.Capabilities

	provider, gpu, kube := capabilities["provider"], capabilities["gpu"], capabilities["kube"]
	if provider == "" || gpu == "" || kube == "" {
		return "", fmt.Errorf("stream %q capabilities have no provider/gpu/kube (got %v)", stream.Name, capabilities)
	}
	if stream.Suite == "" {
		return "", fmt.Errorf("stream %q has no suite, so there is no tab to publish it under", stream.Name)
	}
	if result.Spec.FinishedAt.IsZero() {
		return "", fmt.Errorf("result has no spec.finishedAt, so the build name has no ordering timestamp")
	}
	if result.Spec.Identity.RunID == "" {
		return "", fmt.Errorf("result has no spec.identity.runID, so this leg's build name has nothing distinguishing it from another run of the same combination/stream -- Upload's create-only write would then silently drop a second, corrected publish instead of erroring or overwriting")
	}

	group := result.Spec.Subject
	dashboard := stream.Name
	tab := slug(stream.Suite)

	build := fmt.Sprintf("%019d-%s", result.Spec.FinishedAt.UTC().UnixNano(), result.Spec.Identity.RunID)
	if result.Spec.Identity.RunAttempt != "" {
		build += "-" + result.Spec.Identity.RunAttempt
	}
	build += "-" + shortHash(combo.ID)

	return fmt.Sprintf("groups/%s/%s/%s/%s/", group, dashboard, tab, build), nil
}

// slug lowercases s and replaces every run of characters that are not
// lowercase letters, digits, or hyphens with a single hyphen, so a Ginkgo
// SuiteDescription like "NVKE E2E Suite" becomes a valid GCS path segment:
// "nvke-e2e-suite".
func slug(s string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// shortHash is the first 8 hex characters of the SHA-256 of s -- enough to
// make two different combination IDs collide only by chance, short enough to
// keep build names bounded against combinationID()'s unbounded length.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// releaseTag strips a version's leading "v" so TestGrid's semver sort reads
// its major version instead of always reading 0. Only strips when what
// follows is a digit, so a version that does not start with v<digit> (which
// should not happen, but costs nothing to guard) passes through unchanged
// rather than losing a real leading character.
func releaseTag(version string) string {
	if len(version) > 1 && (version[0] == 'v' || version[0] == 'V') && version[1] >= '0' && version[1] <= '9' {
		return version[1:]
	}
	return version
}
