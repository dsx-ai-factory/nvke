// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
)

// MachineHealthIssue is our own type to avoid exposing the forgesdk dependency. It's a thin wrapper around the forgesdk.MachineHealthIssue.
const (
	otherValue = "Other"
	hardware   = "Hardware"
)

type MachineHealthIssue struct {
	Category string  `json:"category"`
	Summary  string  `json:"summary"`
	Details  *string `json:"details,omitempty"`
}

var validCategories = []string{hardware, "Network", "Performance", otherValue} //nolint:gochecknoglobals //local

func isValidCategory(category string) bool {
	return slices.Contains(validCategories, category)
}

const (
	machineHealthIssueSummaryMinLen  = 2
	machineHealthIssueSummaryMaxLen  = 256
	machineHealthIssueDetailsMinLen  = 2
	machineHealthIssueDetailsMaxLen  = 256
	defaultMachineHealthIssueSummary = "Node removed via NKE remediation"
)

func truncateToRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func isValidSummary(summary string) bool {
	l := utf8.RuneCountInString(summary)
	return l >= machineHealthIssueSummaryMinLen && l <= machineHealthIssueSummaryMaxLen
}

func (m *MachineHealthIssue) Validate() error {
	if m == nil {
		return nil
	}

	var errs []error

	if !isValidCategory(m.Category) {
		errs = append(errs, fmt.Errorf("category is required and must be a valid enum value: %v", validCategories))
	}

	if !isValidSummary(m.Summary) {
		errs = append(errs, fmt.Errorf("summary must be between %d and %d characters", machineHealthIssueSummaryMinLen, machineHealthIssueSummaryMaxLen))
	}

	if m.Details != nil {
		l := utf8.RuneCountInString(*m.Details)
		if l < machineHealthIssueDetailsMinLen || l > machineHealthIssueDetailsMaxLen {
			errs = append(errs, fmt.Errorf("details must be between %d and %d characters", machineHealthIssueDetailsMinLen, machineHealthIssueDetailsMaxLen))
		}
	}

	return errors.Join(errs...)
}

func toForgeSDKMachineHealthIssue(m *MachineHealthIssue) *forgesdk.MachineHealthIssue {
	if m == nil {
		return nil
	}

	result := &forgesdk.MachineHealthIssue{
		Category: &m.Category,
		Summary:  &m.Summary,
	}
	if m.Details != nil {
		result.Details = *forgesdk.NewNullableString(m.Details)
	}
	return result
}

// NewMachineHealthIssueFromJSON creates a MachineHealthIssue from JSON data,
// normalizing it to ensure required fields have sensible defaults.
// Use this when reading MachineHealthIssue from external/untrusted sources (e.g., annotations).
// Empty or whitespace-only data returns nil, nil (no machine health issue).
func NewMachineHealthIssueFromJSON(data []byte) (*MachineHealthIssue, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil //nolint:nilnil // no machine health issue
	}

	var m MachineHealthIssue
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("invalid machine health issue JSON: %s: %w", string(data), err)
	}

	if !isValidCategory(m.Category) {
		m.Category = otherValue
	}

	switch {
	case utf8.RuneCountInString(m.Summary) > machineHealthIssueSummaryMaxLen:
		m.Summary = truncateToRunes(m.Summary, machineHealthIssueSummaryMaxLen)
	case !isValidSummary(m.Summary):
		m.Summary = defaultMachineHealthIssueSummary
	}

	if m.Details != nil {
		l := utf8.RuneCountInString(*m.Details)
		if l < machineHealthIssueDetailsMinLen {
			m.Details = nil
		} else if l > machineHealthIssueDetailsMaxLen {
			truncated := truncateToRunes(*m.Details, machineHealthIssueDetailsMaxLen)
			m.Details = &truncated
		}
	}

	return &m, nil
}
