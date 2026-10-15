// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package resolve answers "what version, given these inputs".
//
// A resolver sees a flat map of plain values and nothing else. It has no
// notion of a stream, a capability or a qualification subject, so the code
// that does know those things stays in one place and the strategies stay
// testable with a map literal. The only package it takes from this repository
// is internal/artifact, which answers whether a location holds something, for
// the strategies asked to check.
package resolve

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Result keys. Most strategies return one value under ResultVersionKey. A
// strategy whose answer is a location returns it under ResultURLKey, and one
// that can name more than one thing about the same version returns the rest
// under their own keys. The caller passes them on to later strategies as
// "<component>.<key>".
const (
	// ResultVersionKey holds the resolved version, without any component prefix.
	ResultVersionKey = "version"

	// ResultRefKey holds the git ref the version was read from.
	ResultRefKey = "ref"

	// ResultURLKey holds a location, for a strategy whose answer is something to
	// fetch and not a version.
	ResultURLKey = "url"

	// ResultStrategyKey names what actually answered, for a resolver that is
	// more than one thing. A resolver that is only itself leaves it unset, and
	// the name the registry gave it stands.
	ResultStrategyKey = "strategy"
)

// Input keys a caller may pass.
const (
	// InputOverrideKey asks for exactly this in place of resolving. What it is
	// depends on the strategy, and each checks it against what it can know. A git
	// tag strategy reads it as a tag that must exist, a locked package as a
	// version that must be locked, and a URL strategy as a whole URL that must
	// exist.
	InputOverrideKey = "override"

	// InputSelectKey picks how a git tag strategy chooses when no version is
	// named. Empty means the highest version of any kind.
	// SelectCandidate means the highest candidate only.
	InputSelectKey = "select"

	// InputConstraintKey is the major, minor or patch prefix a locked package
	// must match, such as "1.34".
	InputConstraintKey = "constraint"

	// InputReleasesKey lists, separated by commas, the versions a strategy that
	// cannot list them itself may choose from, such as the Ubuntu point
	// releases an image is published for.
	InputReleasesKey = "releases"

	// InputValueKey is a declared answer that nothing derives, such as the path
	// of a bootloader.
	InputValueKey = "value"

	// InputVersionKey is the version a location strategy is naming the artifact
	// for. The version half of an entry resolves it and the planner passes it
	// here, so a location strategy never resolves a version of its own.
	InputVersionKey = "version"

	// InputCommitKey asks a strategy that can for the component as it is at this
	// commit, rather than at whatever its tags say. A strategy that cannot
	// ignores it, the way one that does not choose ignores InputSelectKey.
	InputCommitKey = "commit"
)

// SelectCandidate asks for the highest release candidate and ignores releases.
const SelectCandidate = "candidate"

// Version strategy names, as written in a registry entry's version block.
const (
	VersionStrategyGitTag = "gitTag"

	// VersionStrategyCommitDescribe is never written in the registry, because
	// nothing resolves only this way. It names the arm of
	// gitTagOrCommitDescribe that answered, in a plan and in the errors below.
	VersionStrategyCommitDescribe = "commitDescribe"

	VersionStrategyGitTagOrCommitDescribe = "gitTagOrCommitDescribe"
	VersionStrategyLockedPackage          = "lockedPackage"
	VersionStrategyMakeVar                = "makeVar"
)

// Location strategy names, as written in a registry entry's location block.
const (
	LocationStrategyOCIArtifact = "ociArtifact"
	LocationStrategyOSImage     = "osImage"
	LocationStrategyURL         = "url"
)

// Resolver turns inputs into the values a plan records.
type Resolver interface {
	// Resolve returns the resolved values. A resolver that needs an input it
	// was not given fails, it never guesses.
	Resolve(inputs map[string]string) (map[string]string, error)
}

// Validator is implemented by a resolver that can say, without doing any I/O,
// whether the inputs a subject declares for it are ones it can resolve. The
// inputs are the declared ones only: the constraint, the releases and the value.
// A strategy knows what it takes and what it needs, so nothing else has to.
type Validator interface {
	Validate(inputs map[string]string) error
}

// Validate asks a resolver whether the declared inputs suit it. A resolver that
// does not validate accepts them.
func Validate(r Resolver, inputs map[string]string) error {
	if v, ok := r.(Validator); ok {
		return v.Validate(inputs)
	}
	return nil
}

// declared are the inputs a subject can declare for a dependency.
var declared = []string{InputConstraintKey, InputReleasesKey, InputValueKey}

// takesOnly rejects a declared input that is not among those allowed. One handed
// to a strategy that ignores it would resolve by a rule nobody wrote.
func takesOnly(inputs map[string]string, allowed ...string) error {
	for _, k := range declared {
		if inputs[k] != "" && !slices.Contains(allowed, k) {
			return fmt.Errorf("takes no %s", k)
		}
	}
	return nil
}

// needs rejects a missing input. One missing would only fail when a plan is
// made.
func needs(inputs map[string]string, keys ...string) error {
	for _, k := range keys {
		if inputs[k] == "" {
			return fmt.Errorf("needs %s", k)
		}
	}
	return nil
}

// Options are the choices a caller makes about every resolver it builds, as
// opposed to the per-component configuration that comes from the registry.
type Options struct {
	// CheckExists asks the strategies whose answer is a location to confirm it
	// holds something. Off is the default, which keeps planning to git alone and
	// leaves the question to whoever checks the whole plan.
	CheckExists bool
}

// NewVersion builds the resolver a registry entry's version block names. repo
// is the repository root that git and file reads are made against.
//
// A location strategy named here is rejected rather than built. It would
// resolve a location and the planner would read no version from it, which is a
// misconfiguration that should fail at the line that wrote it.
//
// Unknown keys in with are rejected as well as missing ones, so a misspelling
// cannot read as an omission.
func NewVersion(strategy string, with map[string]string, repo string) (Resolver, error) {
	switch strategy {
	case VersionStrategyGitTag:
		if err := checkWith(strategy, with, []string{"tagPrefix"}, nil); err != nil {
			return nil, err
		}
		return GitTag{Dir: repo, TagPrefix: with["tagPrefix"]}, nil
	case VersionStrategyGitTagOrCommitDescribe:
		if err := checkWith(strategy, with, []string{"tagPrefix"}, nil); err != nil {
			return nil, err
		}
		return GitTagOrCommitDescribe{
			Tag:      GitTag{Dir: repo, TagPrefix: with["tagPrefix"]},
			Describe: CommitDescribe{Dir: repo, TagPrefix: with["tagPrefix"]},
		}, nil
	case VersionStrategyLockedPackage:
		if err := checkWith(strategy, with, []string{"lockDir", "at"}, nil); err != nil {
			return nil, err
		}
		return LockedPackage{Dir: repo, LockDir: with["lockDir"], At: with["at"]}, nil
	case VersionStrategyMakeVar:
		if err := checkWith(strategy, with, []string{"var"}, []string{"path"}); err != nil {
			return nil, err
		}
		path := with["path"]
		if path == "" {
			path = DefaultToolVersions
		}
		return MakeVar{Dir: repo, Path: path, Var: with["var"]}, nil
	default:
		return nil, unknownStrategy("version", strategy, versionStrategies(), locationStrategies())
	}
}

// NewLocation builds the resolver a registry entry's location block names.
//
// opts carries the choices a caller makes about every resolver it builds, as
// opposed to the per-entry configuration that comes from the registry. Only the
// location strategies take any, because only a location can be asked whether it
// holds something.
func NewLocation(strategy string, with map[string]string, opts Options) (Resolver, error) {
	switch strategy {
	case LocationStrategyOCIArtifact:
		if err := checkWith(strategy, with, []string{"base", "repository"}, []string{"os", "arch"}); err != nil {
			return nil, err
		}
		// An os with no arch would build "<repository>:<version>-linux", which
		// is a tag nothing publishes, so it is a misconfiguration and not a
		// third shape.
		if with["arch"] == "" && with["os"] != "" {
			return nil, fmt.Errorf("strategy %s takes os only with arch", strategy)
		}
		os := with["os"]
		if os == "" && with["arch"] != "" {
			os = DefaultOS
		}
		return OCIArtifact{
			Base: with["base"], Repository: with["repository"],
			OS: os, Arch: with["arch"],
		}, nil
	case LocationStrategyOSImage:
		if err := checkWith(strategy, with, []string{"base", "arch", "variant"}, []string{"path"}); err != nil {
			return nil, err
		}
		return OSImage{
			Base: with["base"], Path: with["path"],
			Arch: with["arch"], Variant: with["variant"],
			CheckExists: opts.CheckExists,
		}, nil
	case LocationStrategyURL:
		if err := checkWith(strategy, with, nil, []string{"base"}); err != nil {
			return nil, err
		}
		return URL{Base: with["base"], CheckExists: opts.CheckExists}, nil
	default:
		return nil, unknownStrategy("location", strategy, locationStrategies(), versionStrategies())
	}
}

// unknownStrategy reports a strategy this half does not have. A name that is a strategy
// of the other half is said to be so, because the fix is then to move it rather
// than to look for a spelling that does not exist.
func unknownStrategy(half, strategy string, mine, theirs []string) error {
	switch {
	case strategy == "":
		return fmt.Errorf("no strategy named")
	case slices.Contains(theirs, strategy):
		other := "location"
		if half == "location" {
			other = "version"
		}
		return fmt.Errorf("strategy %q is a %s strategy, and this is a %s", strategy, other, half)
	default:
		// The candidate list names the half, so saying it again here would
		// repeat whatever the caller already said about which block it read.
		return fmt.Errorf("%q is not one of %s", strategy, strings.Join(mine, ", "))
	}
}

func versionStrategies() []string {
	return []string{
		VersionStrategyGitTag, VersionStrategyGitTagOrCommitDescribe,
		VersionStrategyLockedPackage, VersionStrategyMakeVar,
	}
}

func locationStrategies() []string {
	return []string{
		LocationStrategyOCIArtifact, LocationStrategyOSImage, LocationStrategyURL,
	}
}

// checkWith rejects a with map that lacks a required key or carries a key the
// strategy does not take.
func checkWith(strategy string, with map[string]string, required, optional []string) error {
	for _, k := range required {
		if _, ok := with[k]; !ok {
			return fmt.Errorf("strategy %s needs %q in with", strategy, k)
		}
	}
	var unknown []string
	for k := range with {
		if !slices.Contains(required, k) && !slices.Contains(optional, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("strategy %s does not take %s in with", strategy, strings.Join(unknown, ", "))
	}
	return nil
}
