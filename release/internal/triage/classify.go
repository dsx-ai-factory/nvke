// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package triage

import "strings"

// Result is what the rules say about a failure.
type Result struct {
	// Class names the failure, or is ClassUnknown.
	Class string `json:"class"`
	// Rule is the rule that matched, empty for an unknown failure.
	Rule string `json:"rule,omitempty"`
	// Action is what may be done about it. ActionNone for an unknown failure.
	Action Action `json:"action"`
	// Evidence is the line of the log where the match ends, for a person to
	// check. For a rule that spans lines, that is the line that says why.
	Evidence string `json:"evidence,omitempty"`
	// Note says what the failure means and what to do.
	Note string `json:"note"`
}

// unknownNote is what a person is told when no rule matches.
const unknownNote = "No rule matches this failure. Read the failed step's log and add a rule if it recurs."

// Classify returns the first rule that matches the failed job, its failed step
// and the evidence.
func (c *Config) Classify(job, step string, e Evidence) Result {
	text := e.Text()

	for i := range c.Rules {
		r := &c.Rules[i]

		if r.job != nil && !r.job.MatchString(job) {
			continue
		}

		if r.step != nil && !r.step.MatchString(step) {
			continue
		}

		loc := r.match.FindStringIndex(text)
		if loc == nil {
			continue
		}

		return Result{
			Class:    r.Class,
			Rule:     r.Name,
			Action:   r.Action,
			Evidence: lineAround(text, max(loc[0], loc[1]-1)),
			Note:     strings.TrimSpace(r.Note),
		}
	}

	return Result{Class: ClassUnknown, Action: ActionNone, Note: unknownNote}
}

// lineAround returns the line of text that holds offset, trimmed to a length a
// message can carry.
func lineAround(text string, offset int) string {
	start := strings.LastIndexByte(text[:offset], '\n') + 1

	end := strings.IndexByte(text[offset:], '\n')
	if end < 0 {
		end = len(text)
	} else {
		end += offset
	}

	line := strings.TrimSpace(text[start:end])

	const maxEvidence = 240
	if len(line) > maxEvidence {
		line = line[:maxEvidence] + "…"
	}

	return line
}
