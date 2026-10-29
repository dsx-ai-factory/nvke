// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clidocs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// newTestTree returns a command tree exercising every piece of metadata the
// generator renders, so the cases below do not depend on nvkectl happening to
// have a command of the right shape today.
func newTestTree() *cobra.Command {
	root := &cobra.Command{Use: "demo", Short: "Demo CLI"}
	root.PersistentFlags().Bool("verbose", false, "Enable debug logging")

	group := &cobra.Command{Use: "group", Short: "A group of commands"}

	leaf := &cobra.Command{
		Use:     "leaf <name>",
		Aliases: []string{"leaves", "lf"},
		Short:   "Do a thing",
		// The placeholder and the indented block are here so that the cases
		// walking the whole generated tree have something to catch.
		Long: "Do a thing, at length, to <name>:\n" +
			"\n" +
			"  demo group leaf <name>",
		Example: "  demo group leaf my-thing",
		Run:     func(_ *cobra.Command, _ []string) {},
	}
	leaf.Flags().String("required-one", "", "A flag that must be given")
	leaf.Flags().String("left", "", "One side")
	leaf.Flags().String("right", "", "The other side")
	leaf.Flags().String("cert", "", "A certificate")
	leaf.Flags().String("key", "", "Its key")
	_ = leaf.MarkFlagRequired("required-one")
	leaf.MarkFlagsMutuallyExclusive("left", "right")
	leaf.MarkFlagsOneRequired("left", "right")
	leaf.MarkFlagsRequiredTogether("cert", "key")

	hidden := &cobra.Command{
		Use:    "hidden",
		Short:  "Not part of the public CLI",
		Hidden: true,
		Run:    func(_ *cobra.Command, _ []string) {},
	}

	deprecated := &cobra.Command{
		Use:        "old",
		Short:      "The previous spelling",
		Deprecated: "use `demo group leaf` instead.",
		Run:        func(_ *cobra.Command, _ []string) {},
	}

	group.AddCommand(leaf, hidden, deprecated)
	root.AddCommand(group)
	return root
}

// generateInto runs the generator over a fresh test tree and returns the
// directory it wrote to.
func generateInto(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, Generate(newTestTree(), dir))
	return dir
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err, "page %s should exist", name)
	return string(data)
}

// TestFlagGroupAnnotationKeys pins the three annotation keys this package
// restates from cobra.
//
// cobra keeps them unexported, so they are copied into clidocs.go. This test
// drives cobra's public API and asserts the copies still find what it wrote:
// an upgrade that renames one fails here, instead of silently dropping the
// constraint sections from every page.
func TestFlagGroupAnnotationKeys(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{Use: "c", Run: func(_ *cobra.Command, _ []string) {}}
	cmd.Flags().String("a", "", "")
	cmd.Flags().String("b", "", "")
	cmd.MarkFlagsMutuallyExclusive("a", "b")
	cmd.MarkFlagsOneRequired("a", "b")
	cmd.MarkFlagsRequiredTogether("a", "b")

	for name, annotation := range map[string]string{
		"mutually exclusive": mutuallyExclusiveAnnotation,
		"one required":       oneRequiredAnnotation,
		"required together":  requiredTogetherAnnotation,
	} {
		require.Equal(t, [][]string{{"a", "b"}}, flagGroups(cmd, annotation),
			"the %q annotation key no longer matches what cobra writes", name)
	}
}

// TestHiddenCommandsAreExcluded is the exclusion rule the reference states:
// a hidden command gets no page and no index row.
func TestHiddenCommandsAreExcluded(t *testing.T) {
	t.Parallel()

	dir := generateInto(t)

	_, err := os.Stat(filepath.Join(dir, "demo_group_hidden.md"))
	require.ErrorIs(t, err, os.ErrNotExist, "a hidden command must get no page")

	// The link, not the word: the index prose explains the exclusion rule and
	// says "hidden" while doing so.
	index := read(t, dir, IndexFile)
	require.NotContains(t, index, "demo_group_hidden.md", "a hidden command must not be indexed")
	require.NotContains(t, index, "`demo group hidden`")

	// The parent's own page must not link to it either, or the reference
	// carries a link that resolves to nothing.
	require.NotContains(t, read(t, dir, "demo_group.md"), "demo_group_hidden.md")
}

// TestDeprecatedCommandIsDocumented covers the one place this departs from
// cobra's IsAvailableCommand: a deprecated command still runs, so it keeps its
// page, and the page says it is deprecated.
func TestDeprecatedCommandIsDocumented(t *testing.T) {
	t.Parallel()

	dir := generateInto(t)

	page := read(t, dir, "demo_group_old.md")
	require.Contains(t, page, "> **Deprecated.** use `demo group leaf` instead.")

	require.Contains(t, read(t, dir, IndexFile), "**(deprecated)**")
}

// TestAliasesAreRendered covers metadata cobra's Markdown generator drops.
func TestAliasesAreRendered(t *testing.T) {
	t.Parallel()

	page := read(t, generateInto(t), "demo_group_leaf.md")
	require.Contains(t, page, "### Aliases")
	require.Contains(t, page, "`leaves`, `lf`")
}

// TestFlagConstraintsAreRendered covers the rest of it: which options are
// required, and which may or must be used together.
func TestFlagConstraintsAreRendered(t *testing.T) {
	t.Parallel()

	page := read(t, generateInto(t), "demo_group_leaf.md")

	require.Contains(t, page, "### Required options")
	require.Contains(t, page, "`--required-one`")
	require.Contains(t, page, "At least one of `--left`, `--right` is required.")
	require.Contains(t, page, "`--left`, `--right` may not be used together.")
	require.Contains(t, page, "`--cert`, `--key` must be used together.")
}

// TestInheritedFlagsAreRendered checks a subcommand's page carries the
// persistent flags it inherits, not only its own.
func TestInheritedFlagsAreRendered(t *testing.T) {
	t.Parallel()

	page := read(t, generateInto(t), "demo_group_leaf.md")
	require.Contains(t, page, "### Options inherited from parent commands")
	require.Contains(t, page, "--verbose")
}

// TestNoGeneratedTimestamp is the determinism property that would otherwise
// make every page differ from one day to the next.
func TestNoGeneratedTimestamp(t *testing.T) {
	t.Parallel()

	dir := generateInto(t)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	for _, e := range entries {
		require.NotContains(t, read(t, dir, e.Name()), "Auto generated by spf13/cobra",
			"%s carries cobra's generated-on stamp", e.Name())
	}
}

// TestGroupCommandsGetNoHelpSubcommand is a regression test.
//
// cobra's doc package calls InitDefaultHelpCmd on every command it renders, so
// each group command gained a `help` child that the binary does not have --
// `demo group help` is not a command. Every group page then linked to a page
// that was never written.
func TestGroupCommandsGetNoHelpSubcommand(t *testing.T) {
	t.Parallel()

	dir := generateInto(t)

	_, err := os.Stat(filepath.Join(dir, "demo_group_help.md"))
	require.ErrorIs(t, err, os.ErrNotExist, "a group command must not gain a help subcommand")

	require.NotContains(t, read(t, dir, "demo_group.md"), "demo_group_help.md")
	require.NotContains(t, read(t, dir, IndexFile), "demo_group_help.md")
}

// TestObsoletePagesAreRemoved is the half of the freshness check that a
// content comparison cannot see: a command that was deleted leaves a page
// behind, and nothing in the new output mentions it.
func TestObsoletePagesAreRemoved(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stale := filepath.Join(dir, "demo_group_removed.md")
	require.NoError(t, os.WriteFile(stale, []byte("# gone\n"), 0o644))

	// Something that is not a page, to show the directory is not simply wiped.
	keep := filepath.Join(dir, "not-a-page.txt")
	require.NoError(t, os.WriteFile(keep, []byte("kept\n"), 0o644))

	require.NoError(t, Generate(newTestTree(), dir))

	_, err := os.Stat(stale)
	require.ErrorIs(t, err, os.ErrNotExist, "the page of a removed command must not survive")
	require.FileExists(t, keep)
}

// TestEveryLinkResolves walks the generated output the way hack/docs-check.sh
// does, so a broken cross-link fails here rather than in the merge gate.
func TestEveryLinkResolves(t *testing.T) {
	t.Parallel()

	dir := generateInto(t)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), mdExtension) {
			continue
		}
		for _, target := range relativeLinks(read(t, dir, e.Name())) {
			require.FileExists(t, filepath.Join(dir, target),
				"%s links to %s, which was not generated", e.Name(), target)
		}
	}
}

// relativeLinks returns the inline-link targets in page that point at another
// file in the same directory.
func relativeLinks(page string) []string {
	var out []string
	for _, part := range strings.Split(page, "](")[1:] {
		target, _, ok := strings.Cut(part, ")")
		if !ok {
			continue
		}
		// Only same-directory links are checkable here. The index also links
		// out of the directory, and hack/docs-check.sh resolves those against
		// the repository.
		if strings.ContainsAny(target, "/#") || target == "" {
			continue
		}
		out = append(out, target)
	}
	return out
}

// TestIndexListsEveryPage keeps the entry point complete: a command with a
// page the index does not mention is unreachable by browsing.
func TestIndexListsEveryPage(t *testing.T) {
	t.Parallel()

	dir := generateInto(t)
	index := read(t, dir, IndexFile)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		if e.Name() == IndexFile || !strings.HasSuffix(e.Name(), mdExtension) {
			continue
		}
		require.Contains(t, index, "("+e.Name()+")", "the index has no row for %s", e.Name())
	}
}

// TestLongTextRendersAsMarkdown covers the two ways terminal help text is
// misread by a Markdown renderer: a placeholder taken for an HTML tag, and an
// indented example block taken for more of the paragraph above it.
func TestLongTextRendersAsMarkdown(t *testing.T) {
	cmd := &cobra.Command{
		Use:   "leaf",
		Short: "Do a thing",
		Long: "Give it a document shaped like this, and nothing else:\n" +
			"\n" +
			"  metadata:\n" +
			"    name: <name>\n" +
			"\n" +
			"Naming <name> twice fails. Use a & b, or a > b.",
		Run: func(_ *cobra.Command, _ []string) {},
	}

	page, err := renderPage(cmd)
	require.NoError(t, err)

	require.Contains(t, page, "```\nmetadata:\n  name: <name>\n```",
		"the indented block should be fenced and dedented, so its structure and its placeholder both survive")
	require.Contains(t, page, "Naming &lt;name&gt; twice fails. Use a &amp; b, or a &gt; b.",
		"a placeholder in prose should be escaped rather than swallowed as a tag")
}

// TestNoPlaceholderIsSwallowed holds the whole generated reference to the rule
// the case above covers for one command: no angle-bracket placeholder outside
// a code span or a fenced block, where a Markdown renderer would drop it.
func TestNoPlaceholderIsSwallowed(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Generate(newTestTree(), dir))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	placeholder := regexp.MustCompile(`<[A-Za-z][A-Za-z0-9._-]*>`)
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		require.NoError(t, err)

		fenced := false
		for n, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				fenced = !fenced
				continue
			}
			if fenced || strings.HasPrefix(line, "<!--") || strings.HasPrefix(line, "GENERATED") {
				continue
			}
			bare := regexp.MustCompile("`[^`]*`").ReplaceAllString(line, "")
			require.NotRegexp(t, placeholder, bare,
				"%s:%d renders as nothing in Markdown; put it in a code span or let markdownText escape it", entry.Name(), n+1)
		}
		require.False(t, fenced, "%s leaves a code fence open", entry.Name())
	}
}

// TestGeneratedPagesAreMDXSafe covers the constructs the documentation site's
// MDX parser rejects but plain Markdown accepts. tools/check-docs-mdx is the
// gate; this keeps the generator from reaching it with a known-bad page.
//
// Tab-indented blocks matter most here. MDX does not read an indented block as
// code at all, and cobra's own completion help indents with tabs, so those
// lines have to be fenced rather than escaped as prose.
func TestGeneratedPagesAreMDXSafe(t *testing.T) {
	root := newTestTree()
	root.AddCommand(&cobra.Command{
		Use:   "braces",
		Short: "Use braces",
		Long:  "Write it to ${fpath[1]}, like this:\n\n\tcmd > \"${fpath[1]}/_x\"",
		Run:   func(_ *cobra.Command, _ []string) {},
	})

	dir := t.TempDir()
	require.NoError(t, Generate(root, dir))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		require.NoError(t, err)

		fenced := false
		for n, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				fenced = !fenced
				continue
			}
			if fenced {
				continue
			}
			where := fmt.Sprintf("%s:%d", entry.Name(), n+1)
			bare := regexp.MustCompile("`[^`]*`").ReplaceAllString(line, "")
			require.NotContains(t, line, "<!--", "%s: an HTML comment is not valid in MDX", where)
			require.NotRegexp(t, `(?:^|[^\\]){`, bare, "%s: an unescaped brace parses as a JSX expression", where)
			require.False(t, strings.HasPrefix(line, "\t"), "%s: MDX does not read a tab-indented block as code; it should be fenced", where)
		}
	}
}
