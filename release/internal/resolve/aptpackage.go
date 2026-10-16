// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// AptPackage resolves a version from the apt suites the packages are published
// to.
//
// The stream is part of the package name, so the versions on offer are the
// suffixes of the names a suite serves: nvke-cni-ovn-charts-1.4.11 offers
// 1.4.11. A version is on offer only when every package in Packages is served
// for it, in every suite in Suites and for every architecture in Arches, which
// is the same rule LockedPackage applies across architectures. A node installs
// all of them, so one missing anywhere is a version that cannot be installed.
//
// This resolves against what is published rather than against a number written
// down beside it. A version that was never built is therefore not something
// this can name, which is the failure it exists to prevent: a pin can go on
// naming a package the repository stopped serving, and nothing says so until a
// node runs apt-get.
type AptPackage struct {
	// Base is the apt repository root, such as
	// https://example.invalid. Suites hang below it.
	Base string

	// Suites are the suite codenames to require the version in, such as noble.
	// A run installs from the one its image matches, and the plan names one
	// version for every stream, so a version has to be in all of them.
	Suites []string

	// Arches are the Debian architectures to require the version for.
	Arches []string

	// Packages are the package name stems, without the stream suffix.
	Packages []string

	// Client makes the requests. Nil means a default one.
	Client *http.Client
}

// aptTimeout bounds one index fetch, and aptIndexLimit bounds how much of one
// is read. An index is a few hundred kilobytes, so a reply far past that is a
// wrong URL answering rather than a repository.
const (
	aptTimeout    = 30 * time.Second
	aptIndexLimit = 32 << 20
)

// streamSuffix matches the stream a package name ends with.
var streamSuffix = regexp.MustCompile(`-([0-9]+\.[0-9]+\.[0-9]+)$`)

// Resolve returns the highest published version that matches
// InputConstraintKey and is served everywhere.
//
// A constraint naming every part pins: 1.4 takes the highest of the 1.4 line,
// 1.4.11 takes that version and nothing else, so a plan stays on it when a
// higher patch is published. Either way the version has to be served, and a
// pin that is not fails rather than falling back to one that is.
//
// With InputOverrideKey it returns that version instead, after the same check.
// An override naming a version a suite does not serve would plan an install
// that cannot succeed, which is the whole point of asking the repository.
func (a AptPackage) Resolve(inputs map[string]string) (map[string]string, error) {
	if inputs[InputReleasesKey] != "" {
		return nil, fmt.Errorf("%s reads its versions from the apt suites, so it takes no %s",
			a.name(), InputReleasesKey)
	}
	want, constraint := inputs[InputOverrideKey], inputs[InputConstraintKey]
	if want == "" && constraint == "" {
		return nil, fmt.Errorf("no %s given for %s", InputConstraintKey, a.name())
	}
	if constraint != "" && !constraintPattern.MatchString(constraint) {
		return nil, fmt.Errorf("constraint %q is not a version prefix such as 1.4", constraint)
	}

	served, err := a.served()
	if err != nil {
		return nil, err
	}

	if want != "" {
		return a.named(want, constraint, served)
	}

	var versions []string
	for v, ok := range served {
		if ok && matches(v, constraint) {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%s serves no version matching %s in every suite (%s) for every architecture (%s)",
			a.name(), constraint, strings.Join(a.Suites, ", "), strings.Join(a.Arches, ", "))
	}
	return map[string]string{ResultVersionKey: strings.TrimPrefix(sortDescending(versions)[0], "v")}, nil
}

// named checks a version that was asked for and returns it.
func (a AptPackage) named(want, constraint string, served map[string]bool) (map[string]string, error) {
	v := normalise(want)
	if !semver.IsValid(v) {
		return nil, fmt.Errorf("%q is not a version", want)
	}
	if !served[v] {
		var have []string
		for candidate, ok := range served {
			if ok {
				have = append(have, candidate)
			}
		}
		return nil, fmt.Errorf("%s does not serve %s everywhere, it serves %s",
			a.name(), strings.TrimPrefix(v, "v"), listed(sortDescending(have)))
	}
	if constraint != "" && !matches(v, constraint) {
		return nil, fmt.Errorf("%s does not match the constraint %s", strings.TrimPrefix(v, "v"), constraint)
	}
	return map[string]string{ResultVersionKey: strings.TrimPrefix(v, "v")}, nil
}

// served reports, for each version offered anywhere, whether every package is
// offered for it in every suite and architecture.
func (a AptPackage) served() (map[string]bool, error) {
	want := len(a.Suites) * len(a.Arches) * len(a.Packages)
	count := map[string]int{}
	for _, suite := range a.Suites {
		for _, arch := range a.Arches {
			names, err := a.index(suite, arch)
			if err != nil {
				return nil, err
			}
			for _, stem := range a.Packages {
				for _, v := range streamsOf(names, stem) {
					count[v]++
				}
			}
		}
	}
	if len(count) == 0 {
		return nil, fmt.Errorf("%s serves no version of %s at all", a.name(), strings.Join(a.Packages, ", "))
	}
	out := make(map[string]bool, len(count))
	for v, n := range count {
		out[v] = n == want
	}
	return out, nil
}

// streamsOf returns the valid stream versions of the names built on stem.
func streamsOf(names []string, stem string) []string {
	var raw []string
	for _, n := range names {
		rest, ok := strings.CutPrefix(n, stem)
		if !ok {
			continue
		}
		if m := streamSuffix.FindStringSubmatch(stem + rest); m != nil {
			raw = append(raw, m[1])
		}
	}
	return validVersions(raw)
}

// index returns the package names one suite serves for one architecture.
//
// Both the plain and the gzipped index are tried, because a repository is free
// to publish either and apt reads whichever is there.
func (a AptPackage) index(suite, arch string) ([]string, error) {
	dir := fmt.Sprintf("%s/nvke-apt-%s/dists/nvke-%s/main/binary-%s",
		strings.TrimSuffix(a.Base, "/"), suite, suite, arch)

	var lastErr error
	for _, name := range []string{"Packages", "Packages.gz"} {
		body, err := a.fetch(dir + "/" + name)
		if err != nil {
			lastErr = err
			continue
		}
		return packageNames(body), nil
	}
	return nil, fmt.Errorf("reading the %s %s index: %w", suite, arch, lastErr)
}

// fetch reads one index, decompressing a gzipped one.
func (a AptPackage) fetch(url string) (string, error) {
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: aptTimeout}
	}
	resp, err := client.Get(url) //nolint:noctx // the client carries the timeout
	if err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}

	var r io.Reader = resp.Body
	if strings.HasSuffix(url, ".gz") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return "", fmt.Errorf("%s: %w", url, err)
		}
		defer gz.Close()
		r = gz
	}
	// One past the limit, so a reply that reaches it is known to be over rather
	// than exactly at it, and is reported instead of being silently truncated.
	// A truncated index resolves from part of a suite, which is the one answer
	// worse than no answer.
	body, err := io.ReadAll(io.LimitReader(r, aptIndexLimit+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	if len(body) > aptIndexLimit {
		return "", fmt.Errorf("%s: larger than %d bytes, which is not an index", url, aptIndexLimit)
	}
	return string(body), nil
}

// packageNames returns the Package: fields of an apt index.
func packageNames(index string) []string {
	var names []string
	for line := range strings.Lines(index) {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "Package:"); ok {
			names = append(names, strings.TrimSpace(name))
		}
	}
	return names
}

// name describes this resolver in an error.
func (a AptPackage) name() string {
	return fmt.Sprintf("%s (%s)", strings.Join(a.Packages, ", "), a.Base)
}

// Validate requires a constraint that is a version prefix, and takes nothing
// else, since the versions are read from the suites.
func (AptPackage) Validate(inputs map[string]string) error {
	if err := takesOnly(inputs, InputConstraintKey); err != nil {
		return err
	}
	if err := needs(inputs, InputConstraintKey); err != nil {
		return err
	}
	if c := inputs[InputConstraintKey]; !constraintPattern.MatchString(c) {
		return fmt.Errorf("constraint %q is not a version prefix such as 1.4", c)
	}
	return nil
}
