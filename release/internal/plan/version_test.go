// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package plan

import (
	"errors"
	"slices"
	"strings"
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

func TestMatchConstraint(t *testing.T) {
	versions := []string{"v1.0.0", "v0.9.1-rc.1", "v0.9.0", "v0.8.0"}

	for _, tc := range []struct {
		constraint string
		want       []string
	}{
		{"", versions},
		{">=0.9.0", []string{"v1.0.0", "v0.9.1-rc.1", "v0.9.0"}},
		{">0.9.0", []string{"v1.0.0", "v0.9.1-rc.1"}},
		{"<0.9.0", []string{"v0.8.0"}},
		{"<=0.9.0", []string{"v0.9.0", "v0.8.0"}},
		{"=0.9.0", []string{"v0.9.0"}},
		// The leading v is optional in a policy file and required by semver.
		{">=v0.9.0", []string{"v1.0.0", "v0.9.1-rc.1", "v0.9.0"}},
	} {
		t.Run(tc.constraint, func(t *testing.T) {
			got, err := matchConstraint(versions, tc.constraint)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// A prerelease above the bound stays in. The popular constraint libraries
// drop it, which would leave nothing to resolve while every version is still
// a candidate.
func TestMatchConstraintKeepsPrereleases(t *testing.T) {
	got, err := matchConstraint([]string{"v0.9.1-rc.1"}, ">=0.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("v0.9.1-rc.1 was excluded by >=0.9.0")
	}
}

func TestMatchConstraintRejectsGarbage(t *testing.T) {
	for _, c := range []string{"0.9.0", "~>0.9.0", ">=nonsense", "^1.0.0"} {
		if _, err := matchConstraint([]string{"v1.0.0"}, c); err == nil {
			t.Errorf("%q was accepted", c)
		}
	}
}

func TestSelectLatestTiers(t *testing.T) {
	never := func(string) (bool, error) { return false, nil }

	t.Run("tier 1 prefers a release over a newer candidate", func(t *testing.T) {
		// The candidate is higher, and qualified. The release still wins:
		// holding against what shipped is the point.
		always := func(string) (bool, error) { return true, nil }
		got, err := selectLatest([]string{"v1.1.0-rc.1", "v1.0.0"}, always)
		if err != nil {
			t.Fatal(err)
		}
		if got != "v1.0.0" {
			t.Errorf("got %q, want v1.0.0", got)
		}
	})

	t.Run("tier 2 prefers a qualified candidate over a higher unqualified one", func(t *testing.T) {
		qualified := func(v string) (bool, error) { return v == "v1.0.0-rc.1", nil }
		got, err := selectLatest([]string{"v1.0.0-rc.3", "v1.0.0-rc.2", "v1.0.0-rc.1"}, qualified)
		if err != nil {
			t.Fatal(err)
		}
		if got != "v1.0.0-rc.1" {
			t.Errorf("got %q, want v1.0.0-rc.1", got)
		}
	})

	t.Run("tier 3 takes the highest candidate when none is qualified", func(t *testing.T) {
		got, err := selectLatest([]string{"v1.0.0-rc.3", "v1.0.0-rc.1"}, never)
		if err != nil {
			t.Fatal(err)
		}
		if got != "v1.0.0-rc.3" {
			t.Errorf("got %q, want v1.0.0-rc.3", got)
		}
	})

	t.Run("nothing at all is an error, not an empty version", func(t *testing.T) {
		if _, err := selectLatest(nil, never); !errors.Is(err, errNoCandidate) {
			t.Errorf("got %v, want errNoCandidate", err)
		}
	})

	t.Run("a history that fails stops resolution", func(t *testing.T) {
		boom := errors.New("registry unreachable")
		_, err := selectLatest([]string{"v1.0.0-rc.1"}, func(string) (bool, error) {
			return false, boom
		})
		if !errors.Is(err, boom) {
			t.Errorf("got %v, want the history error", err)
		}
	})
}

func TestNormalise(t *testing.T) {
	for in, want := range map[string]string{
		"0.9.0":  "v0.9.0",
		"v0.9.0": "v0.9.0",
		"":       "",
	} {
		if got := normalise(in); got != want {
			t.Errorf("normalise(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOutcomeSkippable(t *testing.T) {
	// passed and failed settle the question; error and unschedulable leave
	// it open, so a rerun is still worth doing.
	for outcome, want := range map[Outcome]bool{
		OutcomePassed:        true,
		OutcomeFailed:        true,
		OutcomeError:         false,
		OutcomeUnschedulable: false,
		OutcomeNone:          false,
	} {
		if got := outcome.skippable(); got != want {
			t.Errorf("%q.skippable() = %v, want %v", outcome, got, want)
		}
	}
}

func TestNormaliseHandlesRealTags(t *testing.T) {
	// The tag forms release.yml accepts, so that a version this rejects is
	// one the release workflow would have rejected too.
	for _, tag := range []string{"v0.1.0", "v0.1.1-rc.8", "v10.20.30-rc.100"} {
		if !strings.HasPrefix(normalise(tag), "v") {
			t.Errorf("%q did not normalise", tag)
		}
	}
}
