// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package failuredomains

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPlaceable(t *testing.T) {
	t.Parallel()

	now := time.Now()
	eligible := []string{"A", "B", "C"}

	tests := []struct {
		name   string
		marked map[string]time.Time
		want   []string
	}{
		{"none marked", nil, []string{"A", "B", "C"}},
		{"recent mark drops the domain", map[string]time.Time{"A": now.Add(-time.Hour)}, []string{"B", "C"}},
		{"expired mark is ignored", map[string]time.Time{"A": now.Add(-UnplaceableTTL - time.Minute)}, []string{"A", "B", "C"}},
		{"every domain marked falls back to all", map[string]time.Time{"A": now, "B": now, "C": now}, []string{"A", "B", "C"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, Placeable(eligible, tt.marked, now))
		})
	}
}

func TestMarkUnplaceable(t *testing.T) {
	t.Parallel()

	now := time.Now()
	recent := now.Add(-time.Hour)
	marked := map[string]time.Time{
		"A": now.Add(-2 * time.Hour),
		"B": recent,
		"C": now.Add(-UnplaceableTTL - time.Minute),
	}

	got := MarkUnplaceable(marked, "A", now)

	assert.Equal(t, map[string]time.Time{"A": now, "B": recent}, got)
	assert.Contains(t, marked, "C", "the input map is not modified")
}
