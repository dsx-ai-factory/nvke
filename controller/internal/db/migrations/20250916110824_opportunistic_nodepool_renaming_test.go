// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestCalculateNodePoolRenames(t *testing.T) {
	tests := []struct {
		name          string
		nodePools     []NodePool20250916110824
		clusterName   string
		expectedNames []string
		description   string
	}{
		{
			name:        "basic renaming with single pools",
			clusterName: "test",
			nodePools: []NodePool20250916110824{
				{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Name: "550e8400-e29b-41d4-a716-446655440001", NodeNamePrefix: "cp", ControlPlane: true},
				{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Name: "550e8400-e29b-41d4-a716-446655440002", NodeNamePrefix: "w", ControlPlane: false},
			},
			expectedNames: []string{"test-cp", "test-w"},
			description:   "Should rename UUID names to cluster-prefix format",
		},
		{
			name:        "multiple pools with same prefix",
			clusterName: "test",
			nodePools: []NodePool20250916110824{
				{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Name: "550e8400-e29b-41d4-a716-446655440001", NodeNamePrefix: "cp", ControlPlane: true},
				{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Name: "550e8400-e29b-41d4-a716-446655440002", NodeNamePrefix: "w", ControlPlane: false},
				{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Name: "550e8400-e29b-41d4-a716-446655440003", NodeNamePrefix: "w", ControlPlane: false},
			},
			expectedNames: []string{"test-cp", "test-w-1", "test-w-2"},
			description:   "Should handle multiple pools with same prefix using indices",
		},
		{
			name:        "collision avoidance",
			clusterName: "test",
			nodePools: []NodePool20250916110824{
				{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Name: "test-cp", NodeNamePrefix: "cp", ControlPlane: true},                              // Already descriptive - ignored
				{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Name: "550e8400-e29b-41d4-a716-446655440002", NodeNamePrefix: "cp", ControlPlane: true}, // UUID - will rename
				{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Name: "test-w-1", NodeNamePrefix: "w", ControlPlane: false},                             // Already descriptive - ignored
				{ID: uuid.MustParse("44444444-4444-4444-4444-444444444444"), Name: "550e8400-e29b-41d4-a716-446655440004", NodeNamePrefix: "w", ControlPlane: false}, // UUID - will rename
			},
			expectedNames: []string{"test-cp-2", "test-w"},
			description:   "Should avoid collisions with existing descriptive names",
		},
		{
			name:        "various prefixes and ignore non-UUID names",
			clusterName: "prod",
			nodePools: []NodePool20250916110824{
				{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Name: "550e8400-e29b-41d4-a716-446655440001", NodeNamePrefix: "cp", ControlPlane: true},
				{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Name: "already-descriptive", NodeNamePrefix: "w", ControlPlane: false}, // Non-UUID - ignored
				{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Name: "550e8400-e29b-41d4-a716-446655440003", NodeNamePrefix: "monitoring-v2", ControlPlane: false},
				{ID: uuid.MustParse("44444444-4444-4444-4444-444444444444"), Name: "550e8400-e29b-41d4-a716-446655440004", NodeNamePrefix: "w", ControlPlane: false},
			},
			expectedNames: []string{"prod-cp", "prod-monitoring-v2", "prod-w"},
			description:   "Should handle various prefixes and ignore non-UUID names",
		},
		{
			name:        "multiple pools with collision handling",
			clusterName: "test",
			nodePools: []NodePool20250916110824{
				{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Name: "550e8400-e29b-41d4-a716-446655440001", NodeNamePrefix: "w", ControlPlane: false},
				{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Name: "550e8400-e29b-41d4-a716-446655440002", NodeNamePrefix: "w", ControlPlane: false},
				{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), Name: "test-w-1", NodeNamePrefix: "some-other", ControlPlane: false}, // Existing collision
			},
			expectedNames: []string{"test-w-1-2", "test-w-2"},
			description:   "Should handle collisions when multiple pools have same prefix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updates := calculateNodePoolRenames(tt.nodePools, tt.clusterName)

			assert.Len(t, updates, len(tt.expectedNames), tt.description)

			// Verify expected names (order matters due to deterministic ID sorting within prefix groups)
			actualNames := make([]string, len(updates))
			for i, update := range updates {
				actualNames[i] = update.newName
			}

			assert.ElementsMatch(t, tt.expectedNames, actualNames, "Expected names: %v, Actual names: %v. %s", tt.expectedNames, actualNames, tt.description)
		})
	}
}
