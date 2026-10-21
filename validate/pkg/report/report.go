// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package report renders a validation run as a machine-readable document and
// derives the process exit code from it.
package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
)

// SchemaVersion identifies the report document format, independent of the
// check-set version it carries.
const SchemaVersion = "1"

// Exit codes. Error outranks Fail: "could not evaluate" must never be read as
// "evaluated, non-compliant", and pipelines treat them differently.
const (
	ExitPass  = 0
	ExitFail  = 1
	ExitError = 2
)

// Subject is the artifact the report is about.
type Subject struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Report is the validation run's full result document.
type Report struct {
	SchemaVersion   string          `json:"schemaVersion"`
	CheckSetVersion string          `json:"checkSetVersion"`
	GeneratedAt     time.Time       `json:"generatedAt"`
	Subject         Subject         `json:"subject"`
	Target          TargetSummary   `json:"target"`
	Results         []checks.Result `json:"results"`
	// Summary counts are derived from Results; they exist so a pipeline can
	// branch without replaying the list.
	Summary Summary `json:"summary"`
	// RunError carries a failure outside the checks (setup, verdict signing)
	// in the same schema; it always pairs with a Summary.Error count.
	RunError string `json:"runError,omitempty"`
}

// NewRunError renders a run that could not start: no results, one error,
// exit 2, same schema.
func NewRunError(err error, now time.Time) Report {
	return Report{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now.UTC(),
		Summary:       Summary{Error: 1},
		RunError:      err.Error(),
	}
}

// TargetSummary echoes what the run validated against.
type TargetSummary struct {
	Variant checks.Variant `json:"variant"`
	Arch    string         `json:"arch"`
	Ubuntu  string         `json:"ubuntu"`
}

// Summary is the per-status tally plus the fail split the exit code is built
// from.
type Summary struct {
	Pass         int `json:"pass"`
	Fail         int `json:"fail"`
	BlockingFail int `json:"blockingFail"`
	WarnFail     int `json:"warnFail"`
	Skip         int `json:"skip"`
	Error        int `json:"error"`
}

// New assembles a Report from a finished run.
func New(reg *checks.Registry, t checks.Target, imageSHA256 string, results []checks.Result, now time.Time) Report {
	var s Summary
	for _, r := range results {
		switch r.Status {
		case checks.StatusPass:
			s.Pass++
		case checks.StatusFail:
			s.Fail++
			if r.Severity == checks.SeverityBlocking {
				s.BlockingFail++
			} else {
				s.WarnFail++
			}
		case checks.StatusSkip:
			s.Skip++
		case checks.StatusError:
			s.Error++
		}
	}
	return Report{
		SchemaVersion:   SchemaVersion,
		CheckSetVersion: reg.Version,
		GeneratedAt:     now.UTC(),
		Subject:         Subject{Name: t.ImageName, SHA256: imageSHA256},
		Target:          TargetSummary{Variant: t.Variant, Arch: t.Arch, Ubuntu: t.Ubuntu},
		Results:         results,
		Summary:         s,
	}
}

// ExitCode maps the run outcome to the process exit code: any error → 2, any
// blocking fail → 1, otherwise 0 (warn-level fails are reported, not fatal).
func (r Report) ExitCode() int {
	switch {
	case r.Summary.Error > 0:
		return ExitError
	case r.Summary.BlockingFail > 0:
		return ExitFail
	default:
		return ExitPass
	}
}

// JSON renders the report as indented JSON with a trailing newline.
func (r Report) JSON() ([]byte, error) {
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding report: %w", err)
	}
	return append(out, '\n'), nil
}

// Text renders a one-line-per-check human summary.
func (r Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (sha256:%s)\ncheck set %s\n", r.Subject.Name, r.Subject.SHA256, r.CheckSetVersion)
	for _, res := range r.Results {
		fmt.Fprintf(&b, "  %-6s %-12s %s\n", strings.ToUpper(string(res.Status)), res.ID, res.Evidence)
	}
	if r.RunError != "" {
		fmt.Fprintf(&b, "  ERROR  run          %s\n", r.RunError)
	}
	fmt.Fprintf(&b, "pass=%d fail=%d (blocking=%d warn=%d) skip=%d error=%d\n",
		r.Summary.Pass, r.Summary.Fail, r.Summary.BlockingFail, r.Summary.WarnFail, r.Summary.Skip, r.Summary.Error)
	return b.String()
}
