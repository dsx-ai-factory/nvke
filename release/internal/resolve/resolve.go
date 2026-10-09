// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package resolve answers "what version, given these inputs".
//
// A resolver sees a flat map of plain values and nothing else. It has no
// notion of a stream, a capability or a qualification subject, so the code
// that does know those things stays in one place and the strategies stay
// testable with a map literal. The package imports nothing from this
// repository.
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
)

// SelectCandidate asks for the highest release candidate and ignores releases.
const SelectCandidate = "candidate"

// Strategy names, as written in the registry.
const (
	StrategyGitTag        = "gitTag"
	StrategyLockedPackage = "lockedPackage"
	StrategyMakeVar       = "makeVar"
	StrategyOSImage       = "osImage"
	StrategyURL           = "url"
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

// New builds the resolver a registry entry names. repo is the repository root
// that git and file reads are made against.
//
// Unknown keys in with are rejected as well as missing ones, so a misspelling
// cannot read as an omission.
func New(strategy string, with map[string]string, repo string) (Resolver, error) {
	switch strategy {
	case StrategyGitTag:
		if err := checkWith(strategy, with, []string{"tagPrefix"}, nil); err != nil {
			return nil, err
		}
		return GitTag{Dir: repo, TagPrefix: with["tagPrefix"]}, nil
	case StrategyLockedPackage:
		if err := checkWith(strategy, with, []string{"lockDir", "from"}, nil); err != nil {
			return nil, err
		}
		return LockedPackage{Dir: repo, LockDir: with["lockDir"], From: with["from"]}, nil
	case StrategyMakeVar:
		if err := checkWith(strategy, with, []string{"var"}, []string{"path"}); err != nil {
			return nil, err
		}
		path := with["path"]
		if path == "" {
			path = DefaultToolVersions
		}
		return MakeVar{Dir: repo, Path: path, Var: with["var"]}, nil
	case StrategyOSImage:
		if err := checkWith(strategy, with, []string{"base", "from", "arch", "variant"}, []string{"path"}); err != nil {
			return nil, err
		}
		return OSImage{
			Base: with["base"], Path: with["path"], From: with["from"],
			Arch: with["arch"], Variant: with["variant"],
		}, nil
	case StrategyURL:
		if err := checkWith(strategy, with, nil, []string{"base"}); err != nil {
			return nil, err
		}
		return URL{Base: with["base"]}, nil
	case "":
		return nil, fmt.Errorf("no strategy named")
	default:
		return nil, fmt.Errorf("strategy %q is not one of %s", strategy, strings.Join(strategies(), ", "))
	}
}

func strategies() []string {
	return []string{StrategyGitTag, StrategyLockedPackage, StrategyMakeVar, StrategyOSImage, StrategyURL}
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
