// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
)

// Version is a chart version of the form MAJOR.MINOR.PATCH.
type Version struct {
	Major, Minor, Patch uint64
}

// String returns the version as MAJOR.MINOR.PATCH.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

func (v Version) compare(w Version) int {
	return cmp.Or(cmp.Compare(v.Major, w.Major), cmp.Compare(v.Minor, w.Minor), cmp.Compare(v.Patch, w.Patch))
}

// VersionRange holds the chart versions from Min up to, not including, Max.
// Min is the version the embedded values are written for; Max is the next
// version semver lets break them.
type VersionRange struct {
	Min, Max Version
}

func (r VersionRange) contains(v Version) bool {
	return v.compare(r.Min) >= 0 && v.compare(r.Max) < 0
}

func parseVersion(s string) (Version, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%q is not MAJOR.MINOR.PATCH", s)
	}
	var n [3]uint64
	for i, p := range parts {
		var err error
		if n[i], err = strconv.ParseUint(p, 10, 64); err != nil {
			return Version{}, fmt.Errorf("%q is not MAJOR.MINOR.PATCH", s)
		}
	}
	return Version{n[0], n[1], n[2]}, nil
}

// Validate checks the artifacts in Expected, in that order, for an install on
// platform (os/arch). It ignores every other artifact and returns every
// problem it finds, one per line.
func Validate(rel *Release, platform string) error {
	var errs []error
	for _, want := range Expected {
		var found []*Artifact
		for i := range rel.Artifacts {
			if rel.Artifacts[i].Name == want.Name {
				found = append(found, &rel.Artifacts[i])
			}
		}
		switch {
		case len(found) == 0 && want.Required:
			errs = append(errs, fmt.Errorf("required artifact %q is missing", want.Name))
		case len(found) == 0 && want.RequiredWith != "" && rel.Artifact(want.RequiredWith) != nil:
			errs = append(errs, fmt.Errorf("artifact %q is required when %q is present", want.Name, want.RequiredWith))
		case len(found) == 2:
			errs = append(errs, fmt.Errorf("artifact %q: appears twice", want.Name))
		case len(found) > 2:
			errs = append(errs, fmt.Errorf("artifact %q: appears %d times", want.Name, len(found)))
		case len(found) == 1:
			for _, msg := range artifactProblems(found[0], want, platform) {
				errs = append(errs, fmt.Errorf("artifact %q: %s", want.Name, msg))
			}
		}
	}
	return errors.Join(errs...)
}

func artifactProblems(a *Artifact, want Expectation, platform string) []string {
	if a.Kind != want.Kind {
		return []string{fmt.Sprintf("kind %q, want %q", a.Kind, want.Kind)}
	}
	var problems []string
	if a.Reference == "" {
		problems = append(problems, "reference is missing")
	} else if _, d, ok := strings.Cut(a.Reference, "@"); !ok || digest.Digest(d).Validate() != nil {
		problems = append(problems, "reference has no digest")
	}
	if want.Kind == KindImage {
		if !slices.Contains(a.Platforms(), platform) {
			problems = append(problems, "no manifest for platform "+platform)
		}
		return problems
	}
	if a.ChartVersion == "" {
		return append(problems, "chartVersion is missing")
	}
	v, err := parseVersion(a.ChartVersion)
	switch {
	case err != nil:
		problems = append(problems, "chartVersion "+err.Error())
	case !want.ChartVersions.contains(v):
		problems = append(problems, fmt.Sprintf("chartVersion %q is outside the supported range >=%s, <%s",
			a.ChartVersion, want.ChartVersions.Min, want.ChartVersions.Max))
	}
	return problems
}
