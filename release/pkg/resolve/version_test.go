// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"slices"
	"testing"
)

func TestSortDescending(t *testing.T) {
	// rc.10 above rc.4 is the case a lexical sort gets wrong, and this
	// repository is already past rc.9 on two components.
	got := sortDescending([]string{"v0.1.0-rc.4", "v0.1.0", "v0.1.0-rc.10", "v0.2.0-rc.1"})
	want := []string{"v0.2.0-rc.1", "v0.1.0", "v0.1.0-rc.10", "v0.1.0-rc.4"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNormalise(t *testing.T) {
	for in, want := range map[string]string{"": "", "1.2.3": "v1.2.3", "v1.2.3": "v1.2.3"} {
		if got := normalise(in); got != want {
			t.Errorf("normalise(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidVersionsSkipsWhatSemverRejects(t *testing.T) {
	got := validVersions([]string{"v1.0.0", "1.1.0", "garbage", "v1.2"})
	// v1.2 is shorthand semver accepts, so it stays. A word does not.
	if want := []string{"v1.0.0", "v1.1.0", "v1.2"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMatches(t *testing.T) {
	for _, tc := range []struct {
		version, constraint string
		want                bool
	}{
		{"v1.34.2", "1.34", true},
		{"v1.34.2", "1.34.2", true},
		{"v1.34.2", "1.3", false},
		{"v1.34.2", "1", true},
		{"v1.34.2", "1.34.3", false},
		{"v1.4.0", "1.4", true},
		{"v1.4.0", "1.4.0.1", false},
		{"v1.4.0-rc.1", "1.4", true},
	} {
		if got := matches(tc.version, tc.constraint); got != tc.want {
			t.Errorf("matches(%q, %q) = %v, want %v", tc.version, tc.constraint, got, tc.want)
		}
	}
}
