// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package triage names why a qualification job failed.
//
// A rule table, kept in configuration and not in code, maps the step that
// failed and the real error text of its log to a class of failure, a suggested
// action and a note for a person. The package only classifies. It never
// re-runs, posts or deletes anything: what a caller does with the answer is its
// own decision, and the first caller only reports it.
package triage

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// Action is what a class of failure is allowed to have done about it. It is a
// recommendation: classifying never acts.
type Action string

// The actions a rule may name.
const (
	// ActionNone leaves the failure to a person.
	ActionNone Action = "none"
	// ActionRerunFailed re-runs only the failed jobs of the run.
	ActionRerunFailed Action = "rerun-failed"
	// ActionRerunImageBuild re-runs the failed jobs of the OS image build for
	// the tag first, and the qualification only after its publish jobs pass.
	ActionRerunImageBuild Action = "rerun-image-build"
	// ActionWaitThenRerun waits before the single re-run, because an
	// immediate one fails the same way.
	ActionWaitThenRerun Action = "wait-then-rerun"
)

// ClassUnknown is the class of a failure no rule matches.
const ClassUnknown = "unknown"

// Config is the contents of a triage rules file.
type Config struct {
	Rules []Rule `json:"rules"`
}

// Rule maps a failure to a class. Rules are tried in order and the first match
// wins, so a specific rule goes before a general one.
type Rule struct {
	// Name identifies the rule in reports and must be unique.
	Name string `json:"name"`
	// Class groups rules that mean the same thing, such as one failure that
	// reads differently on two architectures.
	Class string `json:"class"`
	// Step is a regular expression matched against the name of the failed
	// step. Empty matches any step.
	Step string `json:"step,omitempty"`
	// Match is a regular expression matched against the evidence text.
	Match string `json:"match"`
	// Action is what may be done about this class automatically.
	Action Action `json:"action"`
	// Note says what the failure means and what to do, for a person.
	Note string `json:"note"`

	step  *regexp.Regexp
	match *regexp.Regexp
}

// Load reads and validates a rules file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the triage rules: %w", err)
	}

	return Parse(b)
}

// Parse validates the contents of a rules file and compiles its expressions.
func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("parsing the triage rules: %w", err)
	}

	if err := c.compile(); err != nil {
		return nil, err
	}

	return &c, nil
}

func (c *Config) compile() error {
	var errs []error

	if len(c.Rules) == 0 {
		return errors.New("the triage rules file has no rules")
	}

	seen := map[string]bool{}

	for i := range c.Rules {
		r := &c.Rules[i]

		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("rules[%d] %q: %s", i, r.Name, fmt.Sprintf(format, args...)))
		}

		switch {
		case r.Name == "":
			fail("has no name")
		case seen[r.Name]:
			fail("name is used by an earlier rule")
		}

		seen[r.Name] = true

		if r.Class == "" {
			fail("has no class")
		}

		if r.Class == ClassUnknown {
			fail("class %q is reserved for failures no rule matches", ClassUnknown)
		}

		if !validAction(r.Action) {
			fail("action %q is not one of none, rerun-failed, rerun-image-build, wait-then-rerun", r.Action)
		}

		if strings.TrimSpace(r.Note) == "" {
			fail("has no note")
		}

		if r.Match == "" {
			fail("has no match")
			continue
		}

		var err error

		if r.match, err = regexp.Compile(r.Match); err != nil {
			fail("match: %v", err)
		}

		if r.Step != "" {
			if r.step, err = regexp.Compile(r.Step); err != nil {
				fail("step: %v", err)
			}
		}
	}

	return errors.Join(errs...)
}

func validAction(a Action) bool {
	switch a {
	case ActionNone, ActionRerunFailed, ActionRerunImageBuild, ActionWaitThenRerun:
		return true
	}

	return false
}
