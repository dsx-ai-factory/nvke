// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	"k8s.io/apimachinery/pkg/util/version"
)

// VersionRange holds the chart versions from Min up to, not including, Max.
// Min is the version the embedded values are written for; Max is the next
// version semver lets break them.
type VersionRange struct {
	Min, Max *version.Version
}

func versions(min, max string) VersionRange {
	return VersionRange{version.MustParseSemantic(min), version.MustParseSemantic(max)}
}

func (r VersionRange) contains(v *version.Version) bool {
	return v.AtLeast(r.Min) && v.LessThan(r.Max)
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
	v, err := version.ParseSemantic(a.ChartVersion)
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("chartVersion %q is not a semantic version", a.ChartVersion))
	case !want.ChartVersions.contains(v):
		problems = append(problems, fmt.Sprintf("chartVersion %q is outside the supported range >=%s, <%s",
			a.ChartVersion, want.ChartVersions.Min, want.ChartVersions.Max))
	}
	return problems
}
