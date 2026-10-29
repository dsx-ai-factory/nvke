// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleport

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const testManagedByValue = "nke-cloud-workflow-engine"

func TestOwnershipLabels(t *testing.T) {
	got := OwnershipLabels(testManagedByValue)
	expected := map[string]string{
		LabelManagedBy: testManagedByValue,
	}
	assert.Equal(t, expected, got)
}

func TestMergeLabels(t *testing.T) {
	tests := []struct {
		name       string
		userLabels map[string]string
		expected   map[string]string
	}{
		{
			name:       "nil user labels",
			userLabels: nil,
			expected: map[string]string{
				LabelManagedBy: testManagedByValue,
			},
		},
		{
			name:       "empty user labels",
			userLabels: map[string]string{},
			expected: map[string]string{
				LabelManagedBy: testManagedByValue,
			},
		},
		{
			name: "user labels preserved",
			userLabels: map[string]string{
				"custom-key": "custom-value",
				"another":    "label",
			},
			expected: map[string]string{
				"custom-key":   "custom-value",
				"another":      "label",
				LabelManagedBy: testManagedByValue,
			},
		},
		{
			name: "ownership labels override user labels",
			userLabels: map[string]string{
				LabelManagedBy: "should-be-overwritten",
				"custom":       "preserved",
			},
			expected: map[string]string{
				LabelManagedBy: testManagedByValue,
				"custom":       "preserved",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeLabels(tt.userLabels, testManagedByValue)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestIsOwnedByController(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected bool
	}{
		{
			name:     "nil labels",
			labels:   nil,
			expected: false,
		},
		{
			name:     "empty labels",
			labels:   map[string]string{},
			expected: false,
		},
		{
			name: "missing managed-by label",
			labels: map[string]string{
				"other": "label",
			},
			expected: false,
		},
		{
			name: "wrong managed-by value",
			labels: map[string]string{
				LabelManagedBy: "some-other-system",
			},
			expected: false,
		},
		{
			name: "correct managed-by value",
			labels: map[string]string{
				LabelManagedBy: testManagedByValue,
			},
			expected: true,
		},
		{
			name: "correct managed-by with other labels",
			labels: map[string]string{
				LabelManagedBy: testManagedByValue,
				"custom":       "label",
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsOwnedByController(tt.labels, []string{testManagedByValue})
			assert.Equal(t, tt.expected, got)
		})
	}
}

// A value this controller accepts but no longer writes is still owned: that is
// the whole point of the accept list, and the case a rename gets wrong.
func TestIsOwnedByControllerAcceptsPreviousValues(t *testing.T) {
	accepted := []string{"nvke-controller", "nke-cloud-workflow-engine"}

	tests := []struct {
		name     string
		labels   map[string]string
		accepted []string
		expected bool
	}{
		{
			name:     "value this controller writes",
			labels:   map[string]string{LabelManagedBy: "nvke-controller"},
			accepted: accepted,
			expected: true,
		},
		{
			name:     "value an earlier release wrote",
			labels:   map[string]string{LabelManagedBy: "nke-cloud-workflow-engine"},
			accepted: accepted,
			expected: true,
		},
		{
			name:     "value belonging to another controller",
			labels:   map[string]string{LabelManagedBy: "some-other-system"},
			accepted: accepted,
			expected: false,
		},
		{
			name:     "no accepted values owns nothing",
			labels:   map[string]string{LabelManagedBy: "nvke-controller"},
			accepted: nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsOwnedByController(tt.labels, tt.accepted))
		})
	}
}

func TestNodeRoleLabel(t *testing.T) {
	assert.Equal(t, LabelNodeRoleControlPlane, NodeRoleLabel(true))
	assert.Equal(t, LabelNodeRoleWorker, NodeRoleLabel(false))
}

func TestBuildLabelsString(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected string
	}{
		{
			name:     "nil map",
			labels:   nil,
			expected: "",
		},
		{
			name:     "empty map",
			labels:   map[string]string{},
			expected: "",
		},
		{
			name:     "single label",
			labels:   map[string]string{"site": "az01"},
			expected: "site=az01",
		},
		{
			name: "multiple labels sorted alphabetically",
			labels: map[string]string{
				"site":       "az01",
				"cluster":    "c1",
				"managed-by": "nke-cloud-workflow-engine",
			},
			expected: "cluster=c1,managed-by=nke-cloud-workflow-engine,site=az01",
		},
		{
			name: "labels with empty values",
			labels: map[string]string{
				"b": "",
				"a": "val",
			},
			expected: "a=val,b=",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildLabelsString(tt.labels)
			assert.Equal(t, tt.expected, got)
		})
	}
}
