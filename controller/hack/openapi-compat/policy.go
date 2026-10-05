// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

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
type policyResult struct {
	Unwaived []change
	Waived   []change
	Unused   []waiver
}

// evaluate applies the policy: every breaking change the engine reports fails,
// unless a waiver addresses exactly that change.
//
// The classification of breaking versus additive is the engine's, not this
// tool's. Reimplementing it here would mean maintaining a second opinion about
// the OpenAPI specification, and the two would diverge. What this tool owns is
// what happens next, and that is the part a repository has to decide for
// itself.
func evaluate(changes []change, waivers []waiver) policyResult {
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
		if !used[i] {
			result.Unused = append(result.Unused, w)
		}
	}
	return result
}
