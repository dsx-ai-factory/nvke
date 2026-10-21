// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
)

func testReport(results []checks.Result) Report {
	reg := &checks.Registry{Version: "0.0.1-test"}
	target := checks.Target{ImageName: "img.qcow2", Variant: checks.VariantFDE, Arch: "amd64", Ubuntu: "24.04.5"}
	return New(reg, target, "abc123", results, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name    string
		results []checks.Result
		want    int
	}{
		{
			name:    "all pass",
			results: []checks.Result{{ID: "A", Status: checks.StatusPass}},
			want:    ExitPass,
		},
		{
			name: "blocking fail exits 1",
			results: []checks.Result{
				{ID: "A", Status: checks.StatusPass},
				{ID: "B", Status: checks.StatusFail, Severity: checks.SeverityBlocking},
			},
			want: ExitFail,
		},
		{
			name:    "warn-only fail exits 0 but is counted",
			results: []checks.Result{{ID: "A", Status: checks.StatusFail, Severity: checks.SeverityWarn}},
			want:    ExitPass,
		},
		{
			name: "error outranks blocking fail",
			results: []checks.Result{
				{ID: "A", Status: checks.StatusFail, Severity: checks.SeverityBlocking},
				{ID: "B", Status: checks.StatusError},
			},
			want: ExitError,
		},
		{
			name:    "skips alone exit 0",
			results: []checks.Result{{ID: "A", Status: checks.StatusSkip}},
			want:    ExitPass,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, testReport(tt.results).ExitCode())
		})
	}
}

func TestSummaryCounts(t *testing.T) {
	rep := testReport([]checks.Result{
		{ID: "A", Status: checks.StatusPass},
		{ID: "B", Status: checks.StatusFail, Severity: checks.SeverityBlocking},
		{ID: "C", Status: checks.StatusFail, Severity: checks.SeverityWarn},
		{ID: "D", Status: checks.StatusSkip},
		{ID: "E", Status: checks.StatusError},
	})
	assert.Equal(t, Summary{Pass: 1, Fail: 2, BlockingFail: 1, WarnFail: 1, Skip: 1, Error: 1}, rep.Summary)
}

func TestJSONRoundTrip(t *testing.T) {
	rep := testReport([]checks.Result{{ID: "A", Status: checks.StatusPass, Severity: checks.SeverityBlocking, Evidence: "ok"}})
	out, err := rep.JSON()
	require.NoError(t, err)

	var back Report
	require.NoError(t, json.Unmarshal(out, &back))
	assert.Equal(t, rep.SchemaVersion, back.SchemaVersion)
	assert.Equal(t, rep.CheckSetVersion, back.CheckSetVersion)
	assert.Equal(t, rep.Subject, back.Subject)
	assert.Equal(t, rep.Results, back.Results)
	assert.Equal(t, rep.Summary, back.Summary)
}

func TestTextRenderer(t *testing.T) {
	text := testReport([]checks.Result{
		{ID: "INT-001", Status: checks.StatusPass, Severity: checks.SeverityBlocking, Evidence: "digest matches"},
	}).Text()
	assert.Contains(t, text, "img.qcow2")
	assert.Contains(t, text, "check set 0.0.1-test")
	assert.Contains(t, text, "PASS")
	assert.Contains(t, text, "INT-001")
}

func TestNewRunError(t *testing.T) {
	rep := NewRunError(assertableErr{}, time.Now())
	assert.Equal(t, ExitError, rep.ExitCode())
	out, err := rep.JSON()
	require.NoError(t, err)
	assert.Contains(t, string(out), `"runError": "boom"`)
	assert.Contains(t, string(out), `"schemaVersion": "1"`)
}

type assertableErr struct{}

func (assertableErr) Error() string { return "boom" }
