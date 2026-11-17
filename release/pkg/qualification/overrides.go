// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qualification

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ParseOverrides reads versions chosen by a person, as comma-separated
// name=version pairs such as "controller=v0.1.1-rc.12,kube=1.36.2".
//
// This is the format of the e2e suite's -e2e.versions flag, so the string a
// person gives, the names a plan records and the flag the suite is handed are
// one grammar. Whitespace around a name or a value is trimmed, only the first
// "=" splits a pair so a value may contain one, and a value may not contain a
// comma.
//
// A malformed pair is an error and not a skipped entry. Dropping it would plan
// a different selection than the one the person asked for.
func ParseOverrides(s string) (map[string]string, error) {
	out := map[string]string{}
	if s == "" {
		return out, nil
	}
	for _, entry := range strings.Split(s, ",") {
		name, version, ok := strings.Cut(entry, "=")
		name, version = strings.TrimSpace(name), strings.TrimSpace(version)
		if !ok || name == "" || version == "" {
			return nil, fmt.Errorf("%q is not name=version", entry)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%q appears twice", name)
		}
		out[name] = version
	}
	return out, nil
}

// listedNames are the components then the dependencies of the subject.
func listedNames(s *Subject) []string {
	var listed []string
	for _, c := range s.Spec.Components {
		listed = append(listed, c.Name)
	}
	for _, d := range s.Spec.Dependencies {
		listed = append(listed, d.Name)
	}
	return listed
}

// checkOverrides holds every override to the subject. An override can replace a
// component or a dependency the subject lists, and never the subject itself,
// whose version is named with --version.
func checkOverrides(s *Subject, overrides map[string]string) error {
	listed := listedNames(s)
	for _, name := range sortedNames(overrides) {
		switch {
		case name == s.Spec.Subject:
			return fmt.Errorf("%s is the subject, so its version is named with --version", name)
		case !slices.Contains(listed, name):
			return fmt.Errorf("%s has no component or dependency %q, it lists %s",
				s.Spec.Subject, name, strings.Join(listed, ", "))
		}
	}
	return nil
}

// sortedNames returns the override names in order, so the plan lists them the
// same way every time.
func sortedNames(overrides map[string]string) []string {
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
