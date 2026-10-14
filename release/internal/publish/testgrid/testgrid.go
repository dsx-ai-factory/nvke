// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testgrid is the TestGrid Destination: it writes started.json, a
// copy of the JUnit report, then finished.json to the shape TestGrid's
// updater reads (groups/{group}/{dashboard}/{tab}/{build}/), with dashboard
// as {gpu}-v{kube}-nvke-e2e. The trailing -nvke-e2e is not cosmetic --
// config-gen's isNewFormatDashboard only discovers and renders dashboards
// whose name matches that {track}(e2e|tenant) suffix; anything else is
// silently dropped from the UI while still sitting in the bucket.
package testgrid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dsx-ai-factory/nvke/release/internal/qualification"
)

// Uploader writes one object's bytes to a destination-defined location.
// The seam that keeps Destination testable without a real GCS bucket.
type Uploader interface {
	Upload(ctx context.Context, path string, data []byte, contentType string) error
}

// Destination is the TestGrid Destination. Environment picks the group's
// {provider}-{env} suffix (non-prod/prod) -- it must match whatever bucket
// Uploader actually writes to, so it is set alongside Uploader at
// construction, not re-derived.
type Destination struct {
	Uploader    Uploader
	Environment string
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

// provider is the only Forge environment qualify-e2e.yml runs against today.
// Not derived from anything -- there is nothing yet to derive it from.
const provider = "forge"

// releaseStream is the Release Source value config-gen's ColumnHeader
// surfaces on every build (tools/config-gen/main.go), and the one the
// Qualification Matrix's SPA groups under its own project entry. Fixed, not
// derived from Subject, because the project grouping it feeds is a fixed
// taxonomy decision, not a per-subject one.
const releaseStream = "nvke-release"

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
//
// Metadata must stay flat strings: TestGrid's Metadata.Strings() drops
// nested maps, so Capabilities is flattened into gpu/arch/ubuntu/kube here
// rather than passed through as a map.
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
	Commit        string `json:"commit,omitempty"`
}

// Publish writes started.json, a copy of the JUnit report, then finished.json
// (in that order -- finished.json is never written unless the JUnit evidence
// behind it uploaded successfully) under a build prefix derived from result's
// combination and stream. Capabilities (gpu/kube/arch/ubuntu) come from
// result's own Status.Combinations[0].Streams[0].Capabilities, stamped there
// by `qualify result` -- not looked up separately.
func (d *Destination) Publish(ctx context.Context, result *qualification.Result, junitPath string) error {
	combo := result.Status.Combinations[0]
	stream := combo.Streams[0]
	passed := stream.Result == "PASS"
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
		ReleaseStream: releaseStream,
		ReleaseTag:    result.Spec.Version,
		Provider:      provider,
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
	if err != nil {
		return fmt.Errorf("reading JUnit report: %w", err)
	}
	if err := d.Uploader.Upload(ctx, prefix+"artifacts/junit.xml", junitBytes, "application/xml"); err != nil {
		return fmt.Errorf("uploading junit.xml: %w", err)
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

// environmentSuffix maps --environment to the group suffix autotest's own
// writers use (normalizeGroupName trims "-non-prod" for display; "-prod" is
// kept verbatim so both can co-exist under one bucket without colliding).
//
// cmd/qualify/publish.go's buildDestination validates --environment too, for
// bucket selection. The two checks are intentionally separate: this one
// backs buildPrefix's own tests directly, without going through
// buildDestination.
func environmentSuffix(environment string) (string, error) {
	switch environment {
	case "staging":
		return "non-prod", nil
	case "prod":
		return "prod", nil
	default:
		return "", fmt.Errorf("--environment=%s is not staging or prod", environment)
	}
}

// buildPrefix is the group/dashboard/tab/build GCS path for this leg.
//
// group is {provider}-{env}, matching autotest's real CSP-group convention
// (testgrid_destination.py _publish_to_group) -- not a bare subject name.
//
// dashboard is {gpu}-v{kube}-nvke-e2e: the {track}-e2e suffix is required
// for config-gen to discover and render the dashboard at all, and "nvke" is
// the track the SPA derives this subject's "nvke-release" release source
// from.
//
// build is {run_id}-{run_attempt}-{short hash of combination ID}: GitHub run
// IDs increase monotonically, which the updater's build-discovery (by name
// order, not timestamp) depends on, and combination IDs can run 68-84 chars
// (an unbounded name-version join) -- hashed down to keep the object path
// short, with the full ID still in metadata. The hash is what keeps two
// combinations sharing a stream's gpu/kube from colliding.
func (d *Destination) buildPrefix(result *qualification.Result) (string, error) {
	suffix, err := environmentSuffix(d.Environment)
	if err != nil {
		return "", err
	}

	combo := result.Status.Combinations[0]
	stream := combo.Streams[0]
	capabilities := stream.Capabilities

	gpu, kube := capabilities["gpu"], capabilities["kube"]
	if gpu == "" || kube == "" {
		return "", fmt.Errorf("stream %q capabilities have no gpu/kube (got %v)", stream.Name, capabilities)
	}

	group := fmt.Sprintf("%s-%s", provider, suffix)
	dashboard := fmt.Sprintf("%s-v%s-nvke-e2e", gpu, kube)

	build := result.Spec.Identity.RunID
	if result.Spec.Identity.RunAttempt != "" {
		build += "-" + result.Spec.Identity.RunAttempt
	}
	build += "-" + shortHash(combo.ID)

	return fmt.Sprintf("groups/%s/%s/%s/%s/", group, dashboard, stream.Name, build), nil
}

// shortHash is the first 8 hex characters of the SHA-256 of s -- enough to
// make two different combination IDs collide only by chance, short enough to
// keep build names bounded against combinationID()'s unbounded length.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}
