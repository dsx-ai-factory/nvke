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

	// From names the component whose ref the lock files are read at.
	From string
}

// constraintPattern is one to three numeric parts, such as 1.34 or 1.34.2.
var constraintPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}$`)

// Resolve returns the highest locked version that matches InputConstraintKey and
// is locked for every architecture.
//
// The ref comes from the input "<From>.ref". A version that exists for one
// architecture only is not offered, because an image carries both and a plan
// naming it would fail on the architecture that lacks it.
func (l LockedPackage) Resolve(inputs map[string]string) (map[string]string, error) {
	constraint := inputs[InputConstraintKey]
	if constraint == "" {
		return nil, fmt.Errorf("no %s given for %s", InputConstraintKey, l.LockDir)
	}
	if !constraintPattern.MatchString(constraint) {
		return nil, fmt.Errorf("constraint %q is not a version prefix such as 1.34", constraint)
	}

	refKey := l.From + "." + ResultRefKey
	ref := inputs[refKey]
	if ref == "" {
		return nil, fmt.Errorf("no %s given, so there is no ref to read %s at", refKey, l.LockDir)
	}

	byArch, err := l.locked(ref)
	if err != nil {
		return nil, err
	}

	var versions []string
	for v, inEvery := range everywhere(byArch) {
		if inEvery && matches(v, constraint) {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%s at %s locks no version matching %s for every architecture",
			l.LockDir, ref, constraint)
	}
	return map[string]string{ResultVersionKey: strings.TrimPrefix(sortDescending(versions)[0], "v")}, nil
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

// matches reports whether a version starts with the constraint's numeric
// parts. The comparison is on parts, so 1.3 matches 1.3.9 and not 1.34.2.
func matches(version, constraint string) bool {
	core := strings.TrimPrefix(semver.Canonical(version), "v")
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
