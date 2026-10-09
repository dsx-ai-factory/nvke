// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"gopkg.in/yaml.v3"
)

// waiver is one accepted breaking change.
//
// It carries an owner and a review date for the same reason GOVULNCHECK_WAIVED
// does: a waiver without a named person and a date is indistinguishable from an
// oversight six months later. Owner is a person, not a team, so there is
// someone to ask.
type waiver struct {
	Change   string `yaml:"change"`
	Path     string `yaml:"path"`
	Property string `yaml:"property"`
	Reason   string `yaml:"reason"`
	Owner    string `yaml:"owner"`
	Review   string `yaml:"review"`
	// Baseline is the ref the break was measured against when the waiver was
	// written. Optional: it lets the gate tell a waiver that has done its job
	// (the baseline moved on and carries the break) from one that never matched.
	// Only a published release tag orders against a later one; see supersedes.
	Baseline string `yaml:"baseline"`
}

type waiverFile struct {
	Waivers []waiver `yaml:"waivers"`
}

func (w waiver) matches(c change) bool {
	return w.Change == c.ChangeText && w.Path == c.Path && w.Property == c.Property
}

// loadWaivers reads and validates the waiver file. A missing file is an error,
// not an empty waiver set: the file is committed, and its absence means the
// checkout or the path is wrong, which must not read as "nothing is waived and
// therefore everything is fine".
func loadWaivers(path string) ([]waiver, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the waiver file: %w", err)
	}
	return parseWaivers(raw)
}

func parseWaivers(raw []byte) ([]waiver, error) {
	var parsed waiverFile
	// KnownFields, so a misspelled key is an error. Silently ignoring one would
	// turn a waiver into a no-op and fail the gate with no explanation of why.
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	// io.EOF is how the decoder reports a file with no documents in it, which
	// is the shape of a waiver file holding only its explanatory comments.
	if err := dec.Decode(&parsed); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing the waiver file: %w", err)
	}

	for i, w := range parsed.Waivers {
		if err := w.validate(); err != nil {
			return nil, fmt.Errorf("waiver %d: %w", i+1, err)
		}
	}
	return parsed.Waivers, nil
}

func (w waiver) validate() error {
	// Property is allowed to be empty: not every change the engine reports
	// names one. Everything else must be present.
	for _, field := range []struct{ name, value string }{
		{"change", w.Change},
		{"path", w.Path},
		{"reason", w.Reason},
		{"owner", w.Owner},
		{"review", w.Review},
	} {
		if field.value == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	if _, err := time.Parse(time.DateOnly, w.Review); err != nil {
		return fmt.Errorf("review %q is not a YYYY-MM-DD date", w.Review)
	}
	return nil
}

// policyResult is the verdict. Unused is reported, and fails the gate, so a
// waiver cannot outlive the change it was written for -- the same property the
// govulncheck waivers are held to.
//
// Expired is the one exception: a waiver that matches nothing because the
// baseline it was written against has since been superseded by a later release.
// The break it accepted has shipped in a newer tag, so the entry is spent
// rather than wrong, and failing every branch that rebases past that tag would
// blame the wrong people. It is reported and does not fail the gate.
type policyResult struct {
	Unwaived []change
	Waived   []change
	Unused   []waiver
	Expired  []waiver
}

// evaluate applies the policy: every breaking change the engine reports fails,
// unless a waiver addresses exactly that change.
//
// The classification of breaking versus additive is the engine's, not this
// tool's. Reimplementing it here would mean maintaining a second opinion about
// the OpenAPI specification, and the two would diverge. What this tool owns is
// what happens next, and that is the part a repository has to decide for
// itself.
//
// baseline is the ref the specification is being compared against. A waiver
// that matches nothing is Expired, not Unused, only when baseline is a later
// release than the one it names: the tag has moved past the one it was written
// for. Any other relation -- the same tag, an earlier one, or a ref that is not
// a release tag at all -- leaves it Unused, and failing.
func evaluate(changes []change, waivers []waiver, baseline string) policyResult {
	used := make([]bool, len(waivers))
	var result policyResult

	for _, c := range changes {
		if !c.Breaking {
			continue
		}
		matched := false
		for i, w := range waivers {
			if w.matches(c) {
				used[i] = true
				matched = true
			}
		}
		if matched {
			result.Waived = append(result.Waived, c)
		} else {
			result.Unwaived = append(result.Unwaived, c)
		}
	}

	for i, w := range waivers {
		switch {
		case used[i]:
		case supersedes(baseline, w.Baseline):
			result.Expired = append(result.Expired, w)
		default:
			result.Unused = append(result.Unused, w)
		}
	}
	return result
}

// supersedes reports whether current is a later release than the baseline a
// waiver was written against, which is the only relation that expires it.
//
// Inequality alone would not do. The baseline is a caller-supplied ref
// (--baseline, API_COMPAT_BASELINE), so "different" also covers checking
// against an *older* release, and covers naming today's release some other way
// -- a commit SHA, refs/tags/..., a branch. Treating those as expiry would turn
// the run a reviewer reaches for into the run where the matches-nothing rule
// quietly stops applying, and would let a waiver stubbed out during such a run
// sit in the file for good. A pair that cannot be ordered is therefore not a
// supersession: the waiver stays Unused, and still fails.
func supersedes(current, recorded string) bool {
	c, ok := releaseVersion(current)
	if !ok {
		return false
	}
	r, ok := releaseVersion(recorded)
	if !ok {
		return false
	}
	return c.GT(r)
}

// releaseVersion reads the version out of a published baseline tag. Prerelease
// and build suffixes are rejected along with everything else that is not such a
// tag, for the reason defaultTagGlob excludes them: a release candidate is not
// something a consumer has, so it cannot be the release a break shipped in.
func releaseVersion(ref string) (semver.Version, bool) {
	rest, found := strings.CutPrefix(ref, releaseTagPrefix)
	if !found {
		return semver.Version{}, false
	}
	v, err := semver.Parse(rest)
	if err != nil || len(v.Pre) > 0 || len(v.Build) > 0 {
		return semver.Version{}, false
	}
	return v, true
}
