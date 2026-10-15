// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

// LockedPackage resolves a version from the lock files a package commits.
//
// A lock file is <LockDir>/<arch>/<version>.lock, and one exists for each
// version the package is built at, so the set of lock files at a ref is the
// set of versions the image built from that ref carries.
type LockedPackage struct {
	// Dir is the repository to read. Empty means the working directory.
	Dir string

	// LockDir is the directory holding one subdirectory of lock files per
	// architecture.
	LockDir string

	// At names the component whose ref the lock files are read at. This is not
	// that component's version: the lock files say what the image built at that
	// ref carries, which is a different version entirely.
	At string
}

// constraintPattern is one to three numeric parts, such as 1.34 or 1.34.2.
var constraintPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}$`)

// Resolve returns the highest locked version that matches InputConstraintKey and
// is locked for every architecture.
//
// With InputOverrideKey it returns that version instead, after checking that it
// is locked for every architecture and, when a constraint is also given, that it
// matches it. An override that did not match would put a Kubernetes minor on a
// stream that is not qualifying it.
//
// The ref comes from the input "<At>.ref". A version that exists for one
// architecture only is not offered, because an image carries both and a plan
// naming it would fail on the architecture that lacks it.
func (l LockedPackage) Resolve(inputs map[string]string) (map[string]string, error) {
	if inputs[InputReleasesKey] != "" {
		return nil, fmt.Errorf("%s reads its versions from lock files, so it takes no %s", l.LockDir, InputReleasesKey)
	}
	want, constraint := inputs[InputOverrideKey], inputs[InputConstraintKey]
	if want == "" && constraint == "" {
		return nil, fmt.Errorf("no %s given for %s", InputConstraintKey, l.LockDir)
	}
	if constraint != "" && !constraintPattern.MatchString(constraint) {
		return nil, fmt.Errorf("constraint %q is not a version prefix such as 1.34", constraint)
	}

	refKey := l.At + "." + ResultRefKey
	ref := inputs[refKey]
	if ref == "" {
		return nil, fmt.Errorf("no %s given, so there is no ref to read %s at", refKey, l.LockDir)
	}

	byArch, err := l.locked(ref)
	if err != nil {
		return nil, err
	}
	inEvery := everywhere(byArch)

	if want != "" {
		return l.named(want, constraint, ref, inEvery)
	}

	var versions []string
	for v, ok := range inEvery {
		if ok && matches(v, constraint) {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%s at %s locks no version matching %s for every architecture",
			l.LockDir, ref, constraint)
	}
	return map[string]string{ResultVersionKey: strings.TrimPrefix(sortDescending(versions)[0], "v")}, nil
}

// named checks a version that was asked for and returns it.
func (l LockedPackage) named(want, constraint, ref string, inEvery map[string]bool) (map[string]string, error) {
	v := normalise(want)
	if !semver.IsValid(v) {
		return nil, fmt.Errorf("%q is not a version", want)
	}
	if !inEvery[v] {
		var locked []string
		for candidate, ok := range inEvery {
			if ok {
				locked = append(locked, candidate)
			}
		}
		return nil, fmt.Errorf("%s at %s does not lock %s for every architecture, it locks %s",
			l.LockDir, ref, strings.TrimPrefix(v, "v"), listed(sortDescending(locked)))
	}
	if constraint != "" && !matches(v, constraint) {
		return nil, fmt.Errorf("%s does not match the constraint %s", strings.TrimPrefix(v, "v"), constraint)
	}
	return map[string]string{ResultVersionKey: strings.TrimPrefix(v, "v")}, nil
}

// listed names the first few versions, newest first, without the leading v.
func listed(versions []string) string {
	const show = 5
	suffix := ""
	if len(versions) > show {
		versions, suffix = versions[:show], ", ..."
	}
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = strings.TrimPrefix(v, "v")
	}
	return strings.Join(out, ", ") + suffix
}

// locked lists the versions locked under each architecture at the ref.
func (l LockedPackage) locked(ref string) (map[string][]string, error) {
	cmd := exec.Command("git", "ls-tree", "-r", "--name-only", ref, "--", l.LockDir)
	cmd.Dir = l.Dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("reading %s at %s: %w", l.LockDir, ref, err)
	}

	byArch := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		rest, ok := strings.CutPrefix(line, strings.TrimSuffix(l.LockDir, "/")+"/")
		if !ok {
			continue
		}
		arch, file, ok := strings.Cut(rest, "/")
		if !ok || !strings.HasSuffix(file, ".lock") {
			continue
		}
		byArch[arch] = append(byArch[arch], strings.TrimSuffix(file, ".lock"))
	}
	if len(byArch) == 0 {
		return nil, fmt.Errorf("%s at %s holds no lock files", l.LockDir, ref)
	}
	return byArch, nil
}

// everywhere reports, for each valid version, whether every architecture
// locks it.
func everywhere(byArch map[string][]string) map[string]bool {
	count := map[string]int{}
	for _, raw := range byArch {
		for _, v := range validVersions(raw) {
			count[v]++
		}
	}
	out := make(map[string]bool, len(count))
	for v, n := range count {
		out[v] = n == len(byArch)
	}
	return out
}

// Validate requires a constraint that is a version prefix, and takes nothing
// else, since the versions are read from the lock files.
func (LockedPackage) Validate(inputs map[string]string) error {
	if err := takesOnly(inputs, InputConstraintKey); err != nil {
		return err
	}
	if err := needs(inputs, InputConstraintKey); err != nil {
		return err
	}
	if c := inputs[InputConstraintKey]; !constraintPattern.MatchString(c) {
		return fmt.Errorf("constraint %q is not a version prefix such as 1.34", c)
	}
	return nil
}
