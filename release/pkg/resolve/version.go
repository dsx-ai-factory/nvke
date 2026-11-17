// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// normalise puts a version in the form semver understands. Tags carry the v
// and package locks may not, and both mean the same thing.
func normalise(v string) string {
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

// isRelease reports whether a version is a release rather than a candidate.
func isRelease(v string) bool { return semver.Prerelease(v) == "" }

// sortDescending orders versions newest first. semver.Compare orders
// prerelease identifiers correctly, so v0.1.0-rc.10 sorts above v0.1.0-rc.4
// rather than lexically below it.
func sortDescending(versions []string) []string {
	out := append([]string(nil), versions...)
	sort.SliceStable(out, func(i, j int) bool {
		return semver.Compare(out[i], out[j]) > 0
	})
	return out
}

// validVersions keeps the entries semver accepts, normalised. A tag the
// release workflow would reject is not a version this can resolve to, and
// skipping it keeps one malformed tag from making every plan unbuildable.
func validVersions(raw []string) []string {
	var out []string
	for _, v := range raw {
		if n := normalise(v); semver.IsValid(n) {
			out = append(out, n)
		}
	}
	return out
}

// matches reports whether a version starts with the constraint's parts. The
// comparison is on parts, so 1.3 matches 1.3.9 and not 1.34.2, and 24.04 matches
// 24.04.5 and not 24.045.1. A prerelease or build suffix is not part of the
// comparison.
func matches(version, constraint string) bool {
	core := strings.TrimPrefix(version, "v")
	core, _, _ = strings.Cut(core, "+")
	core, _, _ = strings.Cut(core, "-")
	got := strings.Split(core, ".")
	want := strings.Split(constraint, ".")
	if len(want) > len(got) {
		return false
	}
	for i, w := range want {
		if got[i] != w {
			return false
		}
	}
	return true
}

// HighestMatching returns the highest of the candidates that starts with the
// constraint's parts. It is how a strategy that is given its candidates picks
// one, as a strategy that reads them from a source does with what it reads.
//
// Candidates are compared part by part as numbers, so 24.04.10 is above 24.04.5.
// A candidate with a part that is not a number is never chosen.
func HighestMatching(candidates []string, constraint string) (string, bool) {
	var best string
	var bestParts []int
	for _, c := range candidates {
		parts, ok := numericParts(c)
		if !ok || !matches(c, constraint) {
			continue
		}
		if best == "" || slices.Compare(parts, bestParts) > 0 {
			best, bestParts = c, parts
		}
	}
	return best, best != ""
}

// numericParts splits a dotted version into numbers.
func numericParts(v string) ([]int, bool) {
	var out []int
	for _, part := range strings.Split(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
