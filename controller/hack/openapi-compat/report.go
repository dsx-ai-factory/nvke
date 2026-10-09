// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/blang/semver/v4"
)

// change is one entry of the engine's report. Only the fields the policy reads
// are modeled; the engine is free to add more.
//
// Path, Property and ChangeText together address a change, and are what a
// waiver matches on. ChangeHash is deliberately not used for that: it hashes the
// shape of a change and not its location, so 89 of the 91 breaking changes in
// one sample diff shared 5 hashes. A waiver keyed on it would silently cover
// every identically shaped change anywhere in the specification.
type change struct {
	Breaking   bool   `json:"breaking"`
	ChangeText string `json:"changeText"`
	Path       string `json:"path"`
	Property   string `json:"property"`
	Type       string `json:"type"`
	Original   string `json:"original"`
	New        string `json:"new"`
}

func (c change) describe() string {
	out := c.ChangeText
	if c.Property != "" {
		out += " " + c.Property
	}
	if c.Path != "" {
		out += " at " + c.Path
	}
	switch {
	case c.Original != "" && c.New != "":
		out += fmt.Sprintf(" (%q -> %q)", c.Original, c.New)
	case c.Original != "":
		out += fmt.Sprintf(" (was %q)", c.Original)
	}
	return out
}

// waiverStub is the YAML a reviewer can paste into the waiver file, so the
// failure message carries its own remedy rather than sending them to the
// schema.
func (c change) waiverStub(baseline string) []string {
	return []string{
		fmt.Sprintf("- change: %q", c.ChangeText),
		fmt.Sprintf("  path: %q", c.Path),
		fmt.Sprintf("  property: %q", c.Property),
		`  reason: "<why this break is acceptable, and what consumers must do>"`,
		`  owner: "<a person, not a team>"`,
		`  review: "<YYYY-MM-DD>"`,
		fmt.Sprintf("  baseline: %q", baseline),
	}
}

// report is one engine run. NoChanges distinguishes "the engine compared the
// two specifications and found nothing" from "the report held no changes
// because this tool failed to understand it" -- the second is an error, not a
// pass.
type report struct {
	Changes   []change
	NoChanges bool
}

// rawReport covers both shapes the engine emits. With differences it writes
// reportSummary and changes; with none it writes only a message.
type rawReport struct {
	ReportSummary map[string]json.RawMessage `json:"reportSummary"`
	Changes       []change                   `json:"changes"`
	Message       string                     `json:"message"`
}

const (
	// noChangesMessage is the engine's wording when the specifications are
	// equivalent. Matching it is intentional: an unrecognized message is an
	// error, so a future rewording fails the gate loudly instead of being read
	// as "compatible".
	noChangesMessage = "No changes found"
)

// parseReport turns engine output into a report, and fails on anything it
// cannot positively identify. An empty file, a truncated file and an object
// with neither changes nor the no-changes message are all errors: this gate
// only ever passes on evidence of compatibility, never on absence of evidence.
func parseReport(raw []byte) (*report, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("the change engine wrote an empty report; it usually also wrote the reason to stderr")
	}

	var parsed rawReport
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parsing the change engine report: %w", err)
	}

	switch {
	case parsed.Changes != nil:
		return &report{Changes: parsed.Changes}, nil
	case len(parsed.ReportSummary) > 0:
		// A summary with no changes array is a shape this tool does not know
		// how to read. Refuse it rather than report zero breaking changes.
		return nil, fmt.Errorf("the change engine report has a summary but no changes array; the engine's output format has moved")
	case strings.Contains(parsed.Message, noChangesMessage):
		return &report{NoChanges: true}, nil
	default:
		return nil, fmt.Errorf("unrecognized change engine report: %s", truncate(string(raw), 300))
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// reportRequest is everything needed to produce a report from git and the
// change engine.
type reportRequest struct {
	spec          string
	specInRepo    string
	baseline      string
	tagGlob       string
	differ        string
	differVersion string
}

// specPair is the two documents under comparison. The deprecation rules read
// them directly: the engine reports differences, and whether a removal was
// announced first is a fact about the documents, not about the diff.
type specPair struct {
	baseline []byte
	revision []byte
}

// resolve picks the baseline ref and reads both specifications. It is separate
// from running the engine so that --report, which supplies a report rather than
// producing one, still gets the documents the deprecation rules need. A run
// that cannot read both is an error either way: there is no mode in which this
// tool passes without having seen them.
func (r reportRequest) resolve(ctx context.Context) (specPair, string, error) {
	revision, err := os.ReadFile(r.spec)
	if err != nil {
		return specPair{}, "", fmt.Errorf("reading the working-tree specification: %w", err)
	}

	baseline := r.baseline
	if baseline == "" {
		if baseline, err = newestTag(ctx, r.tagGlob); err != nil {
			return specPair{}, "", err
		}
	}

	baselineSpec, err := showAtRef(ctx, baseline, r.specInRepo)
	if err != nil {
		return specPair{}, "", err
	}
	return specPair{baseline: baselineSpec, revision: revision}, baseline, nil
}

// produce runs the change engine over a resolved pair.
func (r reportRequest) produce(ctx context.Context, specs specPair) ([]byte, error) {
	if r.differVersion == "" {
		return nil, fmt.Errorf("--differ-version is required; run this through `make -C controller api-compat-check`, which passes the pinned version")
	}

	dir, err := os.MkdirTemp("", "openapi-compat")
	if err != nil {
		return nil, fmt.Errorf("creating a temporary directory: %w", err)
	}
	defer os.RemoveAll(dir)

	baselinePath := filepath.Join(dir, "baseline-openapi.yaml")
	if err := os.WriteFile(baselinePath, specs.baseline, 0o600); err != nil {
		return nil, fmt.Errorf("writing the baseline specification: %w", err)
	}

	return runDiffer(ctx, r.differ, r.differVersion, baselinePath, r.spec)
}

// newestTag picks the baseline. It fails when no tag qualifies rather than
// falling back to the merge base: a shallow clone has no tags, and comparing
// against nothing would otherwise look exactly like a clean run.
func newestTag(ctx context.Context, glob string) (string, error) {
	out, err := git(ctx, "tag", "--list", glob)
	if err != nil {
		return "", err
	}
	if tag, ok := newestRelease(strings.Split(out, "\n")); ok {
		return tag, nil
	}
	return "", fmt.Errorf(
		"no stable %sMAJOR.MINOR.PATCH tag matches %q, so there is no baseline to compare against. "+
			"Fetch tags (a shallow CI checkout has none: actions/checkout needs fetch-depth: 0 or fetch-tags: true), "+
			"or name a ref with --baseline",
		releaseTagPrefix, glob,
	)
}

// newestRelease picks the highest stable release among candidate tag names.
//
// The glob cannot do this on its own: git matches it with fnmatch, where
// `[0-9]*` is a digit followed by anything, so `controller/api/v0.0.29-rc1`
// matches `defaultTagGlob`. Nor can `--sort=-version:refname`, which orders a
// prerelease above the release it precedes unless versionsort.suffix is
// configured. A release candidate selected as the baseline could not be ordered
// by supersedes, so a waiver written against an earlier release would stay
// Unused and fail instead of expiring.
//
// Ordering therefore goes through releaseVersion, the same function supersedes
// uses, which keeps the invariant the tag constants describe: a tag either
// selects as a baseline and orders against a waiver, or does neither.
func newestRelease(tags []string) (string, bool) {
	var best string
	var bestVersion semver.Version
	for _, line := range tags {
		tag := strings.TrimSpace(line)
		if tag == "" {
			continue
		}
		v, ok := releaseVersion(tag)
		if !ok {
			continue
		}
		if best == "" || v.GT(bestVersion) {
			best, bestVersion = tag, v
		}
	}
	return best, best != ""
}

func showAtRef(ctx context.Context, ref, path string) ([]byte, error) {
	out, err := git(ctx, "show", ref+":"+path)
	if err != nil {
		return nil, fmt.Errorf("reading %s at %s: %w", path, ref, err)
	}
	if strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("%s is empty at %s", path, ref)
	}
	return []byte(out), nil
}

func git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// runDiffer executes the pinned change engine. GOWORK=off keeps the engine's
// own module graph out of this repository's workspace, the same reason
// generate.go sets it for oapi-codegen, and GOFLAGS is cleared so a caller's
// -mod=vendor cannot reach a `go run module@version`.
func runDiffer(ctx context.Context, module, version, baselinePath, specPath string) ([]byte, error) {
	// #nosec G204 -- module and version come from the pinned Makefile variable,
	// and both paths are this tool's own: the working-tree spec, and a file it
	// just wrote into its own temporary directory. Neither is user input.
	cmd := exec.CommandContext(ctx, "go", "run", module+"@"+version,
		"report", "--reproducible", "--no-logo", "--no-color", baselinePath, specPath)
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=auto")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// The engine exits non-zero when either specification fails to parse.
		// That is a gate failure, not a compatible result.
		return nil, fmt.Errorf("%s@%s: %w: %s", module, version, err, truncate(stderr.String(), 1000))
	}
	return stdout.Bytes(), nil
}
