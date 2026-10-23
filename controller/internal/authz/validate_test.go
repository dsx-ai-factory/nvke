// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ValidateBundle is the exported API the whole design in
// design-authz-chart-validation.md is organized around, and NewLocal's own
// tests only exercise it indirectly -- NewLocal calls the same loadBundle
// underneath, but joins its []error into one error, which is not the same
// contract as calling ValidateBundle directly and getting the slice back.
// These tests call it directly.

// ValidateBundle must accept the bundle NKE actually ships, with the test
// mappings in place of the operator's: the same guarantee NewLocal's own
// conformance tests give NewLocal, given to the exported function a
// `post-install,post-upgrade` hook or any other external tooling would call.
func TestValidateBundleAcceptsTheShippedBundle(t *testing.T) {
	errs := ValidateBundle(shippedBundleWithMappings(t, testRoleMappings))
	assert.Empty(t, errs, "the shipped bundle must validate clean: %v", errs)
}

// ValidateBundle's own doc comment claims it reports every problem rather
// than stopping at the first -- the same choice TestEveryPolicyTypeChecks
// makes. This is the direct proof: a bundle with two independent, unrelated
// vocabulary problems must come back with more than one error, not just the
// first one found.
//
// roles.json here (oneReader) declares only "reader" -- named
// directly by the permit below, so checkRoleReachability has nothing to say
// about it and the two vocabulary errors below are the only findings, not a
// first-and-only.
func TestValidateBundleReportsEveryProblem(t *testing.T) {
	dir := writeBundle(t,
		`@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"noSuchAction", resource);
@id("nobody-may-write")
forbid(principal, action, resource) when { principal in NKE::Role::"noSuchRole" };`,
		shippedActions(t),
		oneReader)

	errs := ValidateBundle(dir)
	require.Len(t, errs, 2, "expected exactly the undeclared-action and undeclared-role findings: %v", errs)

	var sawAction, sawRole bool
	for _, err := range errs {
		switch {
		case strings.Contains(err.Error(), `NKE::Action::"noSuchAction"`):
			sawAction = true
		case strings.Contains(err.Error(), `NKE::Role::"noSuchRole"`):
			sawRole = true
		}
	}
	assert.True(t, sawAction, "expected an error naming the undeclared action, got: %v", errs)
	assert.True(t, sawRole, "expected an error naming the undeclared role, got: %v", errs)
}

// checkActionVocabulary/checkRoleVocabulary regex the policy source for
// role/action references; without comment-stripping (stripCedarComments),
// a comment like `// removed NKE::Role::"admin"` reads as a real reference
// to a role that no longer exists, and NewLocal runs these checks at
// startup -- so a comment-only edit could crash-loop a production
// controller. This is the false-positive case Codex named: the ONLY
// mention of the undeclared role/action is inside a comment, and the
// bundle must still validate clean.
func TestValidateBundleIgnoresReferencesInComments(t *testing.T) {
	dir := writeBundle(t,
		`// removed NKE::Role::"admin" and NKE::Action::"noSuchAction" during cleanup
@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);`,
		shippedActions(t),
		oneReader)

	errs := ValidateBundle(dir)
	assert.Empty(t, errs, "a reference that appears only in a comment must not be treated as real: %v", errs)
}

// The same case, spread across both Cedar comment forms (`//` and `/* */`),
// and with a real reference alongside the commented-out one, proving the
// stripper's false-positive fix does not swallow a live reference that
// happens to sit near a comment.
func TestValidateBundleIgnoresReferencesInBlockComments(t *testing.T) {
	dir := writeBundle(t,
		`/* removed NKE::Role::"admin"
   and NKE::Action::"noSuchAction" during cleanup */
@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);`,
		shippedActions(t),
		oneReader)

	errs := ValidateBundle(dir)
	assert.Empty(t, errs, "a reference inside a block comment must not be treated as real: %v", errs)
}

// The false-negative edge case stripCedarComments exists to avoid: Cedar
// string literals can legitimately contain "//" (a URL-shaped action id),
// and a naive comment-stripper would swallow the rest of the line,
// including a real reference that follows on the same line. This proves
// the string-literal-aware stripper does not do that: a role reference
// placed after a "//"-bearing string on the same line must still count.
func TestValidateBundleDoesNotTreatSlashSlashInStringAsComment(t *testing.T) {
	dir := writeBundle(t,
		`@id("url-shaped-action")
permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource) when { context.issuer == "http://example.com" && principal in NKE::Role::"noSuchRole" };`,
		shippedActions(t),
		oneReader)

	errs := ValidateBundle(dir)
	require.Len(t, errs, 1, "expected the real undeclared-role reference after the URL string to still be caught: %v", errs)
	assert.Contains(t, errs[0].Error(), `NKE::Role::"noSuchRole"`)
}

// checkEntityStore (local.go) returns on the first entity-store-class
// problem it finds, unlike the vocabulary/reachability checks above, which
// collect every finding. ValidateBundle's "reports every problem" guarantee
// is real for vocabulary/reachability class findings but narrower for
// entity-store class findings: only the first is reported when several
// exist simultaneously. This is the direct proof of that narrower,
// documented behavior -- see ValidateBundle's doc comment.
func TestValidateBundleReportsOnlyFirstEntityStoreProblem(t *testing.T) {
	// No roles.json entries and no roleMappings.json entries: both "no roles
	// declared" and "no mapping entries" are true simultaneously, but
	// checkEntityStore only ever reports the first problem it finds. The
	// policy names no role or action literal, so this stays isolated to the
	// entity-store checks -- no vocabulary finding rides along.
	dir := writeBundleWithMappings(t, `permit(principal, action, resource);`, shippedActions(t), `[]`, `[]`)

	errs := ValidateBundle(dir)
	require.Len(t, errs, 1, "checkEntityStore reports only the first entity-store problem, by design: %v", errs)
	assert.Contains(t, errs[0].Error(), rolesFile)
}

// roleMappings.json findings are collected with every other finding, one per
// bad entry, so the operator sees the whole list in one pass.
func TestValidateBundleReportsMappingProblemsAlongsideOthers(t *testing.T) {
	dir := writeBundleWithMappings(t,
		`@id("readers-may-read")
permit(principal in NKE::Role::"reader", action in NKE::Action::"noSuchAction", resource);`,
		shippedActions(t),
		oneReader,
		`[
 {"issuer":"k8s","claim":{"name":"aud","value":"https://nke"},"grants":[{"type":"NKE::Role","id":"reader"}]},
 {"issuer":"k8s","spiffe":{"trustDomain":"spiffe://nvidia.com/ns","expression":"true"},"grants":[{"type":"NKE::Role","id":"reader"}]},
 {"issuer":"k8s","serviceAccount":{"namespace":"ci"},"grants":[{"type":"NKE::Role","id":"raeder"}]}
]`)

	errs := ValidateBundle(dir)
	require.Len(t, errs, 4, "three mapping findings and one vocabulary finding: %v", errs)

	joined := make([]string, 0, len(errs))
	for _, err := range errs {
		joined = append(joined, err.Error())
	}
	all := strings.Join(joined, "\n")
	for _, want := range []string{
		roleMappingsFile + "[0]", `"aud"`,
		roleMappingsFile + "[1]", "bare trust domain",
		roleMappingsFile + "[2]", `"raeder"`,
		`NKE::Action::"noSuchAction"`,
	} {
		assert.Contains(t, all, want)
	}
}

// A strict-decoding failure is one finding and stops the mapping checks, but
// not the rest of the bundle's.
func TestValidateBundleReportsAStrictDecodeFailure(t *testing.T) {
	policies := aPolicy + `
permit(principal in NKE::Role::"reader", action == NKE::Action::"noSuchAction", resource);`
	dir := writeBundleWithMappings(t, policies, shippedActions(t), oneReader,
		`[{"issuer":"k8s","serviceAccount":{"namespace":"ci","name":"builder","NAME":null},"grants":[{"type":"NKE::Role","id":"reader"}]}]`)

	errs := ValidateBundle(dir)
	require.Len(t, errs, 2, "the decode failure and the independent vocabulary finding: %v", errs)
	assert.Contains(t, errs[0].Error(), roleMappingsFile)
	assert.Contains(t, errs[0].Error(), "NAME")
	assert.Contains(t, errs[1].Error(), `NKE::Action::"noSuchAction"`)
}

// The issuer check needs the authentication config, which ValidateBundle does
// not have. It runs where the Local is built (role_mappings_test.go) and at
// chart render.
func TestValidateBundleDoesNotCheckMappingIssuers(t *testing.T) {
	dir := shippedBundleWithMappings(t,
		`[{"issuer":"no-such-issuer","claim":{"name":"groups","value":"eng"},"grants":[{"type":"NKE::Role","id":"reader"}]}]`)
	assert.Empty(t, ValidateBundle(dir))
}
