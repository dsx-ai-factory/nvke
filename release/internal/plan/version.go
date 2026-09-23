// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package plan

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/mod/semver"
)

// SelectorLatest is the only selector that exists. Anything else in a
// dependency's selectors list is rejected at plan time rather than ignored,
// because ignoring it would resolve the dependency by a rule nobody wrote.
const SelectorLatest = "latest"

// normalise puts a version in the form semver understands. Tags carry the v,
// versionConstraint in the policy files is written without it, and both mean
// the same thing.
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

// matchConstraint filters versions by a constraint such as ">=0.9.0".
//
// The grammar is one operator and one version, deliberately. A full range
// grammar would have to decide whether ">=0.9.0" admits v0.9.1-rc.1, and the
// popular libraries answer no, excluding prereleases unless the constraint
// carries one. That answer is wrong here: before the first release every
// version is a candidate, so a constraint that silently drops them would
// leave nothing to resolve. Comparing with semver.Compare keeps prereleases
// in, ordered by the spec.
func matchConstraint(versions []string, constraint string) ([]string, error) {
	if constraint == "" {
		return versions, nil
	}

	var op, bound string
	for _, o := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(constraint, o) {
			op = o
			bound = normalise(strings.TrimSpace(strings.TrimPrefix(constraint, o)))
			break
		}
	}
	if op == "" {
		return nil, fmt.Errorf(
			"versionConstraint %q does not start with one of >=, <=, >, <, =", constraint,
		)
	}
	if !semver.IsValid(bound) {
		return nil, fmt.Errorf("versionConstraint %q names %q, which is not a semantic version",
			constraint, bound)
	}

	var out []string
	for _, v := range versions {
		c := semver.Compare(v, bound)
		keep := false
		switch op {
		case ">=":
			keep = c >= 0
		case "<=":
			keep = c <= 0
		case ">":
			keep = c > 0
		case "<":
			keep = c < 0
		case "=":
			keep = c == 0
		}
		if keep {
			out = append(out, v)
		}
	}
	return out, nil
}

// selectLatest implements the three tiers of the `latest` selector over
// versions already filtered and sorted newest first.
//
//  1. The highest release. Qualification is not consulted, because a release
//     can be a hotfix cut without one and holding against what shipped is the
//     point.
//  2. The highest qualified candidate. Qualified at any scope counts.
//  3. The highest candidate, qualified or not.
//
// Tier 3 is what lets the framework start at all: on day one nothing has been
// qualified, so the tiers above it are empty and every run would otherwise be
// impossible. The result is visibly weaker rather than silently so, because
// the record says what each held version had been proven against.
func selectLatest(versions []string, qualified func(string) (bool, error)) (string, error) {
	for _, v := range versions {
		if isRelease(v) {
			return v, nil
		}
	}
	for _, v := range versions {
		ok, err := qualified(v)
		if err != nil {
			return "", err
		}
		if ok {
			return v, nil
		}
	}
	if len(versions) > 0 {
		return versions[0], nil
	}
	return "", errNoCandidate
}
