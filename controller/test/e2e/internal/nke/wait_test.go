// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nke

import (
	"testing"
	"time"
)

// A stall window at or above the budget can never elapse first, which is how
// the short lanes keep the behavior they had: the deadline ends them and
// nothing else does. The bare-metal lanes are the ones long enough for a
// silence to mean something.
func TestStallWindow(t *testing.T) {
	for name, tc := range map[string]struct {
		budget time.Duration
		want   time.Duration
		fires  bool
	}{
		"forge tenant":    {budget: 45 * time.Minute, want: 15 * time.Minute, fires: true},
		"long container":  {budget: 30 * time.Minute, want: 15 * time.Minute, fires: true},
		"two hours":       {budget: 2 * time.Hour, want: 40 * time.Minute, fires: true},
		"at the floor":    {budget: 15 * time.Minute, want: 15 * time.Minute, fires: false},
		"short simulator": {budget: 10 * time.Minute, want: 15 * time.Minute, fires: false},
		"shortest of all": {budget: 5 * time.Minute, want: 15 * time.Minute, fires: false},
	} {
		t.Run(name, func(t *testing.T) {
			got := stallWindow(tc.budget)
			if got != tc.want {
				t.Errorf("stallWindow(%v) = %v, want %v", tc.budget, got, tc.want)
			}
			if fires := got < tc.budget; fires != tc.fires {
				t.Errorf("stallWindow(%v) = %v fires=%v, want fires=%v", tc.budget, got, fires, tc.fires)
			}
		})
	}
}
