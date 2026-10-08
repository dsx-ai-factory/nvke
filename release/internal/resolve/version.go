// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"sort"
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
