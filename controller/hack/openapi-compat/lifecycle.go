// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The deprecation half of the policy.
//
// The change engine answers one question: is this difference breaking? It has
// no opinion about process, so under its rules alone every removal is breaking
// and the only way past the gate is a waiver. That makes the correct, patient
// route -- announce it, wait, then remove -- cost exactly as much as the
// impatient one, which is the wrong incentive for the one operation a gate like
// this is supposed to encourage.
//
// So removal gets a second route. An operation that was deprecated, carried an
// x-sunset date, and whose date has passed may be removed with no waiver at
// all: the promise was made, kept and discharged, and there is nothing left for
// a reviewer to sign. Everything else about a removal is unchanged -- it still
// fails, and it is still waivable.
//
// The cost of offering that route is that the deprecation has to be real, which
// is what the rules below enforce. A `deprecated: true` with no date is not a
// deprecation, and a date a fortnight out is not notice.

const (
	// deprecationNoticeDays is how much warning a client is owed before an
	// operation it uses disappears.
	//
	// A number, not a per-operation setting, because it is a promise about the
	// API rather than a property of any one endpoint. Moving it is a change to
	// what consumers were told to expect, so it is a decision taken once, here,
	// in the open -- not a flag someone raises to get a pull request through.
	deprecationNoticeDays = 180

	// sunsetExtension is the sunset field. `x-sunset` is the convention
	// oasdiff, Zalando's guidelines and the Deprecation HTTP header draft all
	// converge on; OpenAPI itself has no sunset concept, only the boolean
	// `deprecated`.
	sunsetExtension = "x-sunset"
)

// Synthesized change kinds. They are plain `change` values so everything
// already built for the engine's findings works on them too: the waiver file
// matches them the same way, the failure message prints the same pasteable
// stub, and a waiver for one goes stale the same way.
//
// They are deliberately NOT emitted for removals. The engine already reports a
// removal, and a second finding for the same edit would mean two waivers for
// one decision. These cover only the deprecation itself, which the engine sees
// as additive and therefore says nothing about.
const (
	changeDeprecatedWithoutSunset = "deprecated_without_sunset"
	changeSunsetWithoutDeprecated = "sunset_without_deprecated"
	changeSunsetNotADate          = "sunset_not_a_date"
	changeSunsetTooSoon           = "sunset_too_soon"
)

// isHTTPMethod reports whether a path-item key is an operation. The others --
// parameters, summary, description, servers, $ref -- are not, and decoding one
// as an operation would fail on the first path item that has them.
func isHTTPMethod(key string) bool {
	switch strings.ToLower(key) {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	}
	return false
}

// operation is the lifecycle state of one operation: the two fields this policy
// reads, and nothing else.
type operation struct {
	Deprecated bool   `yaml:"deprecated"`
	Sunset     string `yaml:"x-sunset"`
}

// operations is every operation in a specification, keyed by "<path> <method>"
// with the method upper-cased, which is how they are named in messages.
type operations map[string]operation

func opKey(path, method string) string {
	return path + " " + strings.ToUpper(method)
}

type specPaths struct {
	Paths map[string]map[string]yaml.Node `yaml:"paths"`
}

// parseOperations reads the lifecycle state out of a specification.
//
// It unmarshals only `paths`, because that is all this policy needs and
// because a full OpenAPI model would be a second parser to keep in step with
// the engine's. A specification that will not parse at all is already an error
// by the time this runs: the engine is given both files and exits non-zero on
// either.
func parseOperations(raw []byte, what string) (operations, error) {
	var doc specPaths
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("reading the operations of the %s specification: %w", what, err)
	}
	ops := operations{}
	for path, item := range doc.Paths {
		for method, node := range item {
			if !isHTTPMethod(method) {
				continue
			}
			var op operation
			if err := node.Decode(&op); err != nil {
				return nil, fmt.Errorf("reading %s %s of the %s specification: %w",
					strings.ToUpper(method), path, what, err)
			}
			ops[opKey(path, method)] = op
		}
	}
	return ops, nil
}

// sunsetState is what a parsed x-sunset date amounts to for one operation.
type sunsetState struct {
	declared bool      // x-sunset is present
	valid    bool      // and is a YYYY-MM-DD date
	date     time.Time // the date, when valid
}

func (op operation) sunsetState() sunsetState {
	if op.Sunset == "" {
		return sunsetState{}
	}
	date, err := time.Parse(time.DateOnly, op.Sunset)
	if err != nil {
		return sunsetState{declared: true}
	}
	return sunsetState{declared: true, valid: true, date: date}
}

// discharged reports whether this operation may now be removed without a
// waiver: deprecated, with a real date, and the date has arrived.
func (op operation) discharged(now time.Time) bool {
	s := op.sunsetState()
	return op.Deprecated && s.valid && !s.date.After(now)
}

// lifecycleResult is what the deprecation rules contribute to the verdict:
// findings to add, and removals to stop treating as findings.
type lifecycleResult struct {
	// Findings are synthesized breaking changes for deprecations that are not
	// real deprecations.
	Findings []change
	// SanctionedOps are "<path> <METHOD>" keys whose removal has been earned.
	SanctionedOps map[string]bool
	// SanctionedPaths are paths every operation of which was earned, which is
	// how the engine reports a path item that disappeared whole.
	SanctionedPaths map[string]bool
}

// evaluateLifecycle applies the deprecation rules to a pair of specifications.
//
// Every rule is scoped to what CHANGED, never to the state of the document.
// That distinction is the whole design: a state rule would fail a pull request
// that never touched the specification, the moment some pre-existing
// deprecation drifted out of compliance, and a gate that fails work unrelated
// to it gets switched off. So an operation whose deprecation fields are
// identical on both sides is not examined at all, however they are set.
func evaluateLifecycle(baselineSpec, revisionSpec []byte, now time.Time, noticeDays int) (lifecycleResult, error) {
	base, err := parseOperations(baselineSpec, "baseline")
	if err != nil {
		return lifecycleResult{}, err
	}
	rev, err := parseOperations(revisionSpec, "working-tree")
	if err != nil {
		return lifecycleResult{}, err
	}

	result := lifecycleResult{
		SanctionedOps:   map[string]bool{},
		SanctionedPaths: map[string]bool{},
	}

	// Deprecations that are not deprecations.
	for _, key := range sortedKeys(rev) {
		op := rev[key]
		prev, existed := base[key]
		if existed && prev == op {
			continue
		}
		result.Findings = append(result.Findings, deprecationFindings(key, op, now, noticeDays)...)
	}

	// Removals that earned their way out.
	removedPaths := map[string]int{}
	for _, key := range sortedKeys(base) {
		if _, still := rev[key]; still {
			continue
		}
		if !base[key].discharged(now) {
			continue
		}
		result.SanctionedOps[key] = true
		removedPaths[pathOf(key)]++
	}
	// A path item is reported as one removal only when all of it went. Counting
	// is how that is told from a path that lost some of its operations: the
	// engine reports the first as `object_removed` naming the path, and the
	// second as `property_removed` naming each method.
	for path, sanctioned := range removedPaths {
		total := 0
		for key := range base {
			if pathOf(key) == path {
				total++
			}
		}
		if sanctioned == total {
			result.SanctionedPaths[path] = true
		}
	}

	return result, nil
}

// deprecationFindings holds one operation's declaration to the standard that
// lets a later removal skip the waiver.
func deprecationFindings(key string, op operation, now time.Time, noticeDays int) []change {
	path, method := splitKey(key)
	jsonPath := fmt.Sprintf("$.paths['%s'].%s", path, strings.ToLower(method))
	s := op.sunsetState()

	switch {
	case op.Deprecated && !s.declared:
		return []change{{
			Breaking:   true,
			ChangeText: changeDeprecatedWithoutSunset,
			Path:       jsonPath,
			Property:   "deprecated",
			New:        fmt.Sprintf("%s %s is deprecated with no %s date, which announces nothing a client can act on", method, path, sunsetExtension),
		}}

	case !op.Deprecated && s.declared:
		// The dangerous direction of the same mistake: a date is recorded, the
		// gate would honor it at removal time, and no client was ever told,
		// because `deprecated` is the only half of this that reaches a
		// generated client or the rendered reference.
		return []change{{
			Breaking:   true,
			ChangeText: changeSunsetWithoutDeprecated,
			Path:       jsonPath,
			Property:   sunsetExtension,
			New:        fmt.Sprintf("%s %s has %s %q but is not marked deprecated, so no consumer is told", method, path, sunsetExtension, op.Sunset),
		}}

	case op.Deprecated && !s.valid:
		return []change{{
			Breaking:   true,
			ChangeText: changeSunsetNotADate,
			Path:       jsonPath,
			Property:   sunsetExtension,
			Original:   op.Sunset,
			New:        fmt.Sprintf("%s %s has %s %q, which is not a YYYY-MM-DD date", method, path, sunsetExtension, op.Sunset),
		}}

	case op.Deprecated && s.date.Before(now.AddDate(0, 0, noticeDays)):
		return []change{{
			Breaking:   true,
			ChangeText: changeSunsetTooSoon,
			Path:       jsonPath,
			Property:   sunsetExtension,
			Original:   op.Sunset,
			New: fmt.Sprintf("%s %s is to be removed on %s, which is less than %d days away; that is not notice",
				method, path, op.Sunset, noticeDays),
		}}
	}
	return nil
}

// applyLifecycle removes from the engine's findings the removals the
// deprecation process has already paid for.
//
// The two shapes are what the engine actually emits, confirmed against it: a
// path item that disappeared whole is one `object_removed` at `$.paths` naming
// the path, and a single operation is one `property_removed` at
// `$.paths['<path>']` naming the method. Anything else is left alone -- this
// discharges removals, and is not a general-purpose filter.
func applyLifecycle(changes []change, life lifecycleResult) ([]change, []change) {
	var kept, discharged []change
	for _, c := range changes {
		if !c.Breaking {
			kept = append(kept, c)
			continue
		}
		if lifecycleDischarges(c, life) {
			discharged = append(discharged, c)
			continue
		}
		kept = append(kept, c)
	}
	return kept, discharged
}

func lifecycleDischarges(c change, life lifecycleResult) bool {
	switch c.ChangeText {
	case "object_removed":
		return c.Path == "$.paths" && life.SanctionedPaths[c.Property]
	case "property_removed":
		path, ok := pathFromJSONPath(c.Path)
		if !ok || !isHTTPMethod(c.Property) {
			return false
		}
		return life.SanctionedOps[opKey(path, c.Property)]
	}
	return false
}

// pathFromJSONPath reads the API path back out of `$.paths['/clusters']`.
func pathFromJSONPath(jsonPath string) (string, bool) {
	const prefix = "$.paths['"
	const suffix = "']"
	if !strings.HasPrefix(jsonPath, prefix) || !strings.HasSuffix(jsonPath, suffix) {
		return "", false
	}
	inner := jsonPath[len(prefix) : len(jsonPath)-len(suffix)]
	// A nested path such as `$.paths['/clusters'].get` is an operation, not a
	// path item, and is not the shape a removed operation is reported in.
	if strings.Contains(inner, "'") {
		return "", false
	}
	return inner, true
}

func pathOf(key string) string {
	path, _ := splitKey(key)
	return path
}

// splitKey reverses opKey. The method never contains a space and an API path
// may, so the split is from the right.
func splitKey(key string) (string, string) {
	i := strings.LastIndex(key, " ")
	if i < 0 {
		return key, ""
	}
	return key[:i], key[i+1:]
}

func sortedKeys(ops operations) []string {
	out := make([]string, 0, len(ops))
	for key := range ops {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
