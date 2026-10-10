// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
)

// GitTag resolves a version from the repository's tags.
//
// A tag is TagPrefix followed by a version, so "controller/v0.1.1-rc.11" is
// the controller at v0.1.1-rc.11. An empty prefix reads the unprefixed tags
// that belong to the repository as a whole.
type GitTag struct {
	// Dir is the repository to read. Empty means the working directory.
	Dir string

	// TagPrefix is everything in a tag before the version.
	TagPrefix string
}

// Resolve returns the version and the tag it was read from.
//
// With InputOverrideKey it checks that the version exists and returns it. With no
// version it returns the highest version of any kind, ordered by semver, so a
// candidate is chosen when it is newer than every release and a release is chosen
// when it is newer than its candidates: v0.1.1 beats v0.1.1-rc.8, and
// v0.1.1-rc.1 beats v0.1.0. Under SelectCandidate it returns only the highest
// candidate.
func (g GitTag) Resolve(inputs map[string]string) (map[string]string, error) {
	versions, err := g.versions()
	if err != nil {
		return nil, err
	}

	version, err := g.pick(versions, inputs)
	if err != nil {
		return nil, err
	}
	return map[string]string{ResultVersionKey: version, ResultRefKey: g.TagPrefix + version}, nil
}

func (g GitTag) pick(versions []string, inputs map[string]string) (string, error) {
	if want := inputs[InputOverrideKey]; want != "" {
		return g.exists(versions, want)
	}

	switch inputs[InputSelectKey] {
	case SelectCandidate:
		for _, v := range versions {
			if !isRelease(v) {
				return v, nil
			}
		}
		return "", fmt.Errorf("%s has no release candidate, so a version has to be named", g.name())
	case "":
		if len(versions) > 0 {
			return versions[0], nil
		}
		return "", fmt.Errorf("%s has no tagged versions", g.name())
	default:
		return "", fmt.Errorf("select %q is not one this understands", inputs[InputSelectKey])
	}
}

// exists holds a named version to the same standard as a resolved one: it has
// to be a version, and it has to exist. Without this a typo produces a plan
// naming a version that was never cut, and every later step then goes looking
// for artifacts under it.
func (g GitTag) exists(versions []string, want string) (string, error) {
	v := normalise(want)
	if !semver.IsValid(v) {
		return "", fmt.Errorf(
			"%q is not a version: expected vX.Y.Z or vX.Y.Z-rc.N, and the dot in -rc.N is required",
			want,
		)
	}
	if slices.Contains(versions, v) {
		return v, nil
	}
	if len(versions) == 0 {
		return "", fmt.Errorf("%s has no tagged versions, so %s cannot be one of them", g.name(), v)
	}

	// Name what does exist. The usual cause is reaching for a candidate that
	// was never cut, and the answer is nearly always in the first few.
	const show = 5
	nearest, suffix := versions, ""
	if len(nearest) > show {
		nearest, suffix = nearest[:show], ", ..."
	}
	return "", fmt.Errorf("%s has no version %s, the newest are %s%s",
		g.name(), v, strings.Join(nearest, ", "), suffix)
}

// versions lists the tags under the prefix, valid and newest first.
//
// No tags yields an empty list and no error. That is a real state, a
// component before its first candidate, and the caller reports it against
// the thing that wanted it, which is a better message than one naming git.
func (g GitTag) versions() ([]string, error) {
	cmd := exec.Command("git", "tag", "--list", g.TagPrefix+"v*")
	cmd.Dir = g.Dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing %s tags: %w", g.name(), err)
	}

	var raw []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			raw = append(raw, strings.TrimPrefix(line, g.TagPrefix))
		}
	}
	return sortDescending(validVersions(raw)), nil
}

// name is how an error refers to what the tags belong to.
func (g GitTag) name() string {
	if g.TagPrefix == "" {
		return "the repository"
	}
	return strings.TrimSuffix(g.TagPrefix, "/")
}

// Validate accepts no declared input, because a tag is found by prefix alone.
func (GitTag) Validate(inputs map[string]string) error { return takesOnly(inputs) }
