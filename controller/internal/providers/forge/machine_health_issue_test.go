// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewMachineHealthIssueFromJSON_EmptyOrWhitespace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
	}{
		{name: "nil", data: nil},
		{name: "empty_slice", data: []byte{}},
		{name: "empty_string", data: []byte("")},
		{name: "whitespace_only", data: []byte(" \t\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, err := NewMachineHealthIssueFromJSON(tt.data)
			require.NoError(t, err)
			assert.Nil(t, m)
		})
	}
}

func TestNewMachineHealthIssueFromJSON_InvalidJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
	}{
		{name: "truncated", data: []byte(`{`)},
		{name: "not_object", data: []byte(`"string"`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, err := NewMachineHealthIssueFromJSON(tt.data)
			require.Error(t, err)
			assert.Nil(t, m)
		})
	}
}

func TestNewMachineHealthIssueFromJSON_Valid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		json string
		want *MachineHealthIssue
	}{
		{
			name: "hardware_with_details",
			json: `{"category":"Hardware","summary":"GPU thermal trip","details":"slot 3 ECC error"}`,
			want: &MachineHealthIssue{
				Category: hardware,
				Summary:  "GPU thermal trip",
				Details:  strPtr("slot 3 ECC error"),
			},
		},
		{
			name: "network_minimal",
			json: `{"category":"Network","summary":"Link down on eth0"}`,
			want: &MachineHealthIssue{
				Category: "Network",
				Summary:  "Link down on eth0",
				Details:  nil,
			},
		},
		{
			name: "performance_other_enum",
			json: `{"category":"Performance","summary":"Throttling observed"}`,
			want: &MachineHealthIssue{
				Category: "Performance",
				Summary:  "Throttling observed",
				Details:  nil,
			},
		},
		{
			name: "leading_trailing_whitespace_on_payload",
			json: "  \n{\"category\":\"Other\",\"summary\":\"Manual flag\"}  ",
			want: &MachineHealthIssue{
				Category: otherValue,
				Summary:  "Manual flag",
				Details:  nil,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NewMachineHealthIssueFromJSON([]byte(tt.json))
			require.NoError(t, err)
			require.NotNil(t, got)
			assertMachineHealthIssueEqual(t, tt.want, got)
		})
	}
}

func TestNewMachineHealthIssueFromJSON_Defaulting(t *testing.T) {
	t.Parallel()

	defaultSummary := defaultMachineHealthIssueSummary

	tests := []struct {
		name string
		json string
		want *MachineHealthIssue
	}{
		{
			name: "invalid_category_defaults_to_other",
			json: `{"category":"NotARealCategory","summary":"Something wrong"}`,
			want: &MachineHealthIssue{
				Category: otherValue,
				Summary:  "Something wrong",
				Details:  nil,
			},
		},
		{
			name: "empty_summary_gets_default",
			json: `{"category":"Hardware","summary":""}`,
			want: &MachineHealthIssue{
				Category: hardware,
				Summary:  defaultSummary,
				Details:  nil,
			},
		},
		{
			name: "empty_object_both_fields_defaulted",
			json: `{}`,
			want: &MachineHealthIssue{
				Category: otherValue,
				Summary:  defaultSummary,
				Details:  nil,
			},
		},
		{
			name: "missing_keys_both_defaulted",
			json: `{"details":"note only"}`,
			want: &MachineHealthIssue{
				Category: otherValue,
				Summary:  defaultSummary,
				Details:  strPtr("note only"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NewMachineHealthIssueFromJSON([]byte(tt.json))
			require.NoError(t, err)
			require.NotNil(t, got)
			assertMachineHealthIssueEqual(t, tt.want, got)
		})
	}
}

func TestNewMachineHealthIssueFromJSON_LengthConstraints(t *testing.T) {
	t.Parallel()

	defaultSummary := defaultMachineHealthIssueSummary
	longSummary := strings.Repeat("a", 257)
	truncatedSummary := strings.Repeat("a", 256)
	longDetails := strings.Repeat("b", 257)
	truncatedDetails := strings.Repeat("b", 256)

	tests := []struct {
		name string
		json string
		want *MachineHealthIssue
	}{
		{
			name: "one_char_summary_gets_default",
			json: `{"category":"Hardware","summary":"x"}`,
			want: &MachineHealthIssue{Category: hardware, Summary: defaultSummary},
		},
		{
			name: "summary_at_max_256_is_kept",
			json: `{"category":"Hardware","summary":"` + truncatedSummary + `"}`,
			want: &MachineHealthIssue{Category: hardware, Summary: truncatedSummary},
		},
		{
			name: "summary_over_256_is_truncated",
			json: `{"category":"Hardware","summary":"` + longSummary + `"}`,
			want: &MachineHealthIssue{Category: hardware, Summary: truncatedSummary},
		},
		{
			name: "details_over_256_is_truncated",
			json: `{"category":"Hardware","summary":"disk failure","details":"` + longDetails + `"}`,
			want: &MachineHealthIssue{Category: hardware, Summary: "disk failure", Details: strPtr(truncatedDetails)},
		},
		{
			name: "one_char_details_is_dropped",
			json: `{"category":"Hardware","summary":"disk failure","details":"x"}`,
			want: &MachineHealthIssue{Category: hardware, Summary: "disk failure", Details: nil},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NewMachineHealthIssueFromJSON([]byte(tt.json))
			require.NoError(t, err)
			require.NotNil(t, got)
			assertMachineHealthIssueEqual(t, tt.want, got)
		})
	}
}

func assertMachineHealthIssueEqual(t *testing.T, want, got *MachineHealthIssue) {
	t.Helper()
	assert.Equal(t, want.Category, got.Category)
	assert.Equal(t, want.Summary, got.Summary)
	if want.Details == nil {
		assert.Nil(t, got.Details)
		return
	}
	require.NotNil(t, got.Details)
	assert.Equal(t, *want.Details, *got.Details)
}

func strPtr(s string) *string {
	return &s
}
