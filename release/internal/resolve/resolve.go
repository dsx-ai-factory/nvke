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
// strategy that can name more than one thing about the same version returns the
// rest under their own keys, and the caller passes them on to later strategies
// as "<component>.<key>".
const (
	// ResultVersionKey holds the resolved version, without any component prefix.
	ResultVersionKey = "version"

	// ResultRefKey holds the git ref the version was read from.
	ResultRefKey = "ref"
)

// Input keys a caller may pass.
const (
	// InputVersionKey asks for exactly this version in place of resolving one.
	// Each strategy checks the version against what it can know, so a git tag
	// strategy requires the tag to exist and a locked package requires the
	// version to be locked.
	InputVersionKey = "version"

	// InputSelectKey picks how a git tag strategy chooses when no version is
	// named. Empty means the highest release, else the highest candidate.
	// SelectCandidate means the highest candidate only.
	InputSelectKey = "select"

	// InputConstraintKey is the major, minor or patch prefix a locked package
	// must match, such as "1.34".
	InputConstraintKey = "constraint"
)

// SelectCandidate asks for the highest release candidate and ignores releases.
const SelectCandidate = "candidate"

// Strategy names, as written in the registry.
const (
	StrategyGitTag        = "gitTag"
	StrategyLockedPackage = "lockedPackage"
	StrategyMakeVar       = "makeVar"
)

// Resolver turns inputs into the values a plan records.
type Resolver interface {
	// Resolve returns the resolved values. A resolver that needs an input it
	// was not given fails, it never guesses.
	Resolve(inputs map[string]string) (map[string]string, error)
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
	case "":
		return nil, fmt.Errorf("no strategy named")
	default:
		return nil, fmt.Errorf("strategy %q is not one of %s", strategy, strings.Join(strategies(), ", "))
	}
}

func strategies() []string {
	return []string{StrategyGitTag, StrategyLockedPackage, StrategyMakeVar}
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
