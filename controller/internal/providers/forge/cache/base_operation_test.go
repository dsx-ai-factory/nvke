// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"testing"
	"time"
)

func TestCacheRefreshInterval(t *testing.T) {
	tests := []struct {
		name     string
		cacheTTL time.Duration
		want     time.Duration
	}{
		{
			name:     "thirty second TTL",
			cacheTTL: 30 * time.Second,
			want:     24 * time.Second,
		},
		{
			name:     "five minute TTL",
			cacheTTL: 5 * time.Minute,
			want:     4 * time.Minute,
		},
		{
			name:     "twenty four hour TTL",
			cacheTTL: 24 * time.Hour,
			want:     19*time.Hour + 12*time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cacheRefreshInterval(tt.cacheTTL)
			if got != tt.want {
				t.Errorf("Expected refresh interval %v, got %v", tt.want, got)
			}
		})
	}
}
