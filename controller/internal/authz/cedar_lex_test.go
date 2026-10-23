// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// These exercise stripCedarComments directly, independent of loadBundle/
// ValidateBundle -- validate_test.go's TestValidateBundleIgnoresReferencesIn*
// tests cover the same lexer indirectly, through a full bundle load; these
// are the fast, no-fixture-directory unit tests for the lexer itself.

func TestStripCedarCommentsRemovesLineComments(t *testing.T) {
	source := "permit(principal, action, resource); // a trailing comment\n" +
		"// removed NKE::Role::\"admin\"\n" +
		"forbid(principal, action, resource);"

	got := string(stripCedarComments([]byte(source)))

	assert.NotContains(t, got, "trailing comment")
	assert.NotContains(t, got, "NKE::Role::\"admin\"")
	assert.Contains(t, got, "permit(principal, action, resource);")
	assert.Contains(t, got, "forbid(principal, action, resource);")
}

func TestStripCedarCommentsRemovesBlockComments(t *testing.T) {
	source := "permit(principal, action, resource) /* NKE::Role::\"admin\"\n" +
		"spans multiple lines */ when { true };"

	got := string(stripCedarComments([]byte(source)))

	assert.NotContains(t, got, "NKE::Role::\"admin\"")
	assert.NotContains(t, got, "spans multiple lines")
	assert.Contains(t, got, "permit(principal, action, resource)")
	assert.Contains(t, got, "when { true };")
}

func TestStripCedarCommentsPreservesUnterminatedBlockComment(t *testing.T) {
	// No closing "*/": the loop must not run past the end of source.
	source := "permit(principal, action, resource); /* NKE::Role::\"admin\""

	got := string(stripCedarComments([]byte(source)))

	assert.Contains(t, got, "permit(principal, action, resource);")
}

func TestStripCedarCommentsDoesNotTreatSlashSlashInStringAsComment(t *testing.T) {
	source := `permit(principal, action == NKE::Action::"http://example", resource);`

	got := string(stripCedarComments([]byte(source)))

	assert.Equal(t, source, got, "a \"//\" inside a string literal is data, not a comment opener")
}

func TestStripCedarCommentsRespectsBackslashEscapedQuotes(t *testing.T) {
	// An escaped quote inside the string must not end the string early --
	// otherwise the trailing `// real comment` would wrongly be treated as
	// code rather than comment, or the `*/`-shaped text after it would be
	// misread as ending a block comment that never started.
	source := `permit(principal, action == NKE::Action::"say \"hi\" // not a comment", resource); // real comment`

	got := string(stripCedarComments([]byte(source)))

	assert.Contains(t, got, `NKE::Action::"say \"hi\" // not a comment"`)
	assert.NotContains(t, got, "real comment")
}

func TestStripCedarCommentsLeavesOrdinaryPolicySourceUntouched(t *testing.T) {
	source := `permit(principal in NKE::Role::"reader", action in NKE::Action::"read", resource);`

	got := string(stripCedarComments([]byte(source)))

	assert.Equal(t, source, got)
}
