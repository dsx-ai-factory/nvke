#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Check the documented facts that also live in the repository.
#
# Prose restated four facts the tree already held, and all four went stale:
# docs/architecture.md said two Go modules when go.work lists nine,
# docs/getting-started.md said "both modules", docs/README.md described a
# directory by one of the three files in it, and README.md counts the binaries
# `make build` links.
#
# Each check below reads the fact from the tree, never from a second copy of it.
#
# WHAT THIS DOES NOT CHECK. A green run means no counted fact and no link is
# wrong. It does not mean the prose is right.
#
#   - Whether a description is accurate, current or complete. "docs/README.md
#     described a directory by one of the three files in it" is the drift that
#     started this, and no check can see it. Check 4 only asks whether every
#     document in docs/ has a row.
#   - Make targets named in prose. Resolving one needs the `cd` or `-C` around
#     it, module help output comes in three different shapes, and
#     docs/go-workspace.md deliberately names `make vendor` to say it does not
#     exist. A scan would be wrong on its first run.
#   - A count with no noun after it, such as "all nine" in CONTRIBUTING.md.
#   - A count that spans a list item, a table row or a blank line. Blocks stop
#     at all three, so a number in one bullet never reads a noun in the next.
#   - Reference-style links, bare anchors, and whether an anchor exists.
#   - A binary that is renamed out of README.md and docs/go-workspace.md and
#     leaves the old name behind. Check 3 only asks whether every current name
#     is present.
#   - Every other document. Checks 2, 3 and 4 name the files they read.
#
# No `mapfile`: it is a bash 4.4 builtin, and on macOS bash 3.2 it leaves the
# array empty and exits 0, so the caller checks nothing and passes.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

fail=0

# GitHub renders `::error::` as an annotation. Locally that is noise, so print
# the plain form there. Same shape as hack/govulncheck-gate.sh.
error() {
	if [[ "${GITHUB_ACTIONS:-false}" == "true" ]]; then
		echo "::error::$*" >&2
	else
		echo "ERROR: $*" >&2
	fi
	fail=1
}

# Markdown this repository writes. vendor/ and generated/ are not ours to edit.
markdown_files() {
	git ls-files -z -- '*.md' ':(exclude)vendor/*' ':(exclude)generated/*'
}

# The same, minus the notices files. Their "156 modules" counts dependencies,
# not workspace members, so a module count there is a different fact.
prose_files() {
	git ls-files -z -- '*.md' \
		':(exclude)vendor/*' \
		':(exclude)generated/*' \
		':(exclude)*THIRD_PARTY_NOTICES.md'
}

# ---------------------------------------------------------------------------
# The facts, read from the tree
# ---------------------------------------------------------------------------

# go.work, through the parser license.yml and govulncheck.yml already use.
modules="$(./hack/workspace-modules.sh)"
module_count="$(printf '%s\n' "${modules}" | wc -l | tr -d ' ')"

# `make build` produces exactly the binaries VERSION_CMDS names, and
# `make version-check` fails when it does not. Read here instead of built,
# because building takes minutes and this check should take seconds.
binaries="$(
	awk '/^VERSION_CMDS[[:space:]]*:?=/, /[^\\]$/' Makefile |
		sed 's/^VERSION_CMDS[[:space:]]*:*=[[:space:]]*//; s/\\$//' |
		tr -s ' \t' '\n' | sed '/^$/d' | cut -d: -f1
)"
[[ -n "${binaries}" ]] || {
	echo "ERROR: no binaries parsed from VERSION_CMDS in Makefile" >&2
	exit 2
}
binary_count="$(printf '%s\n' "${binaries}" | wc -l | tr -d ' ')"

# The members of the required aggregate: the `needs:` list of its `gate` job.
members="$(
	awk '
		/^  gate:/ { in_gate = 1 }
		in_gate && /^    needs:$/ { in_needs = 1; next }
		in_needs && /^      - / { sub(/^      - /, ""); print; next }
		in_needs { exit }
	' .github/workflows/merge-gate.yml
)"
[[ -n "${members}" ]] || {
	echo "ERROR: no members parsed from gate.needs in .github/workflows/merge-gate.yml" >&2
	exit 2
}
member_count="$(printf '%s\n' "${members}" | wc -l | tr -d ' ')"

# ---------------------------------------------------------------------------
# Check 1: a count written in prose matches the count in the tree
# ---------------------------------------------------------------------------

# House style spells small numbers, so both spellings are accepted. "both" is
# in the list because "both modules" is how the module count drifted last time.
NUMBERS='both|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|[0-9]+'

# Up to three words may sit between the number and the noun, which is what
# lets "two independently deployable Go modules" be found.
#
# The match needs a word boundary in front of the number, or an architecture name
# supplies one. Without it, "amd64 Linux binaries" reads as a claim of 64
# binaries, and this repository writes amd64 and arm64 constantly.
GAP='([[:space:]]+[a-zA-Z-]+){0,3}[[:space:]]+'

number_word() {
	case "$1" in
	1) echo one ;;      2) echo two ;;       3) echo three ;;
	4) echo four ;;     5) echo five ;;      6) echo six ;;
	7) echo seven ;;    8) echo eight ;;     9) echo nine ;;
	10) echo ten ;;     11) echo eleven ;;   12) echo twelve ;;
	13) echo thirteen ;; 14) echo fourteen ;; 15) echo fifteen ;;
	16) echo sixteen ;; 17) echo seventeen ;; 18) echo eighteen ;;
	19) echo nineteen ;; 20) echo twenty ;;
	# Past twenty only the digits are accepted. Nothing here counts that high.
	*) echo "" ;;
	esac
}

# Markdown hard-wraps, so "two independently deployable Go modules" straddles a
# newline and a line-at-a-time grep cannot see it. Join each block of prose into
# one record, and keep where the block starts so a failure names a place. A
# list item, table row, heading, quote or fence starts a new block, so a number
# in one bullet cannot join a noun in the next.
#
# The location goes at the END of the record. At the front, its line number is a
# `[0-9]+` the pattern below would happily read as the count.
prose_blocks() {
	prose_files | xargs -0 awk '
		function flush() { if (block != "") print block "\t" file ":" first; block = "" }
		FNR == 1 { flush() }
		/^[[:space:]]*$/ { flush(); next }
		/^[[:space:]]*([-*+>|#`]|[0-9]+[.)])/ { flush() }
		{ if (block == "") { file = FILENAME; first = FNR; block = $0 } \
		  else block = block " " $0 }
		END { flush() }
	'
}

# check_count <what> <expected> <noun regex>
check_count() {
	local what="$1" expected="$2" noun="$3"
	local word record loc text phrase number
	word="$(number_word "${expected}")"
	while IFS= read -r record; do
		loc="${record##*$'\t'}"
		text="${record%$'\t'*}"
		while IFS= read -r phrase; do
			number="$(printf '%s' "${phrase%%[[:space:]]*}" | tr '[:upper:]' '[:lower:]')"
			[[ "${number}" == "${expected}" ]] && continue
			[[ -n "${word}" && "${number}" == "${word}" ]] && continue
			# "three other modules" counts a subset, so it is not restating the
			# repository's total and must not be measured against it. A word
			# that makes the number partial is the signal.
			case "$(printf '%s' "${phrase}" | awk '{print tolower($2)}')" in
			other | more | additional | remaining | further | extra) continue ;;
			esac
			error "${loc}: \"${phrase}\", but this repository has ${expected}${word:+ (${word})} ${what}"
		done < <(printf '%s' "${text}" | grep -oEi "\\b(${NUMBERS})${GAP}${noun}" || true)
	done < <(prose_blocks | grep -Ei "\\b(${NUMBERS})${GAP}${noun}" || true)
}

# One row per counted fact. To check another, add a row whose noun regex matches
# the words that follow the number.
check_count "workspace modules" "${module_count}" '(go[[:space:]]+)?modules\b'
check_count "released binaries" "${binary_count}" 'binaries\b'
check_count "merge-gate members" "${member_count}" 'members\b'

# ---------------------------------------------------------------------------
# Check 2: the module table in docs/go-workspace.md matches go.work
# ---------------------------------------------------------------------------

# Rows read: | `agent` | yes | builds from public dependencies |
# The pattern needs the whole first cell to be one backticked path, so the prose
# tables further down the same file do not match.
table="$(grep -oE '^\| `[^`]+` \|' docs/go-workspace.md | tr -d '|` ' | LC_ALL=C sort || true)"
if [[ "${table}" != "${modules}" ]]; then
	error "docs/go-workspace.md lists different modules from go.work"
	diff <(printf '%s\n' "${table}") <(printf '%s\n' "${modules}") |
		sed 's/^</         only in the document: /; s/^>/         only in go.work:      /' >&2 || true
fi

# ---------------------------------------------------------------------------
# Check 3: the two documents that enumerate the binaries name all of them
# ---------------------------------------------------------------------------

# Only these two enumerate. AGENTS.md gives a count and no list, so asking every
# document that mentions a binary to name them all would fail on it.
for doc in README.md docs/go-workspace.md; do
	while IFS= read -r binary; do
		grep -qF -- "${binary}" "${doc}" ||
			error "${doc} never names ${binary}, which \`make build\` produces"
	done <<<"${binaries}"
done

# ---------------------------------------------------------------------------
# Check 4: docs/README.md indexes every document in docs/
# ---------------------------------------------------------------------------

while IFS= read -r doc; do
	[[ "${doc}" == "docs/README.md" ]] && continue
	grep -qF -- "(${doc#docs/})" docs/README.md ||
		error "docs/README.md has no row for ${doc}"
done < <(git ls-files -- 'docs/*.md')

# ---------------------------------------------------------------------------
# Check 5: every relative link points at something that exists
# ---------------------------------------------------------------------------

# Inline links only. Reference-style links and bare anchors are not checked.
while IFS= read -r -d '' file; do
	dir="$(dirname "${file}")"
	while IFS= read -r target; do
		case "${target}" in
		http*|mailto:*|'#'*|'') continue ;;
		esac
		target="${target%%#*}" # drop the anchor; the file is the checkable part
		[[ -n "${target}" ]] || continue
		if [[ "${target}" == /* ]]; then
			path=".${target}" # root-relative, the way GitHub reads it
		else
			path="${dir}/${target}"
		fi
		[[ -e "${path}" ]] ||
			error "${file}: the link to ${target} resolves to ${path}, which does not exist"
	done < <(grep -oE '\]\([^)[:space:]]+\)' "${file}" | sed 's/^](//; s/)$//' || true)
done < <(markdown_files)

# ---------------------------------------------------------------------------
# Check 6: no internal tracker identifiers
# ---------------------------------------------------------------------------

# Internal ticket IDs are dead links to an external reader. CHANGELOG.md is
# exempt: it is generated from commit history, which is public anyway. This
# script exempts itself, since the pattern below must name what it forbids.
while IFS=: read -r file line match; do
	error "${file}:${line}: internal tracker reference '${match}' — replace it with a GitHub issue or the context it stood for"
done < <(git ls-files -z -- ':(exclude)vendor/*' ':(exclude)generated/*' \
	':(exclude)CHANGELOG.md' ':(exclude)hack/docs-check.sh' ':(exclude)go.sum' |
	xargs -0 grep -nHoE 'NKX-[0-9]+|DGXCINC-[0-9]+|jirasw\.nvidia\.com' || true)

# ---------------------------------------------------------------------------

if [[ ${fail} -ne 0 ]]; then
	echo >&2
	echo "The documentation contradicts the repository. Correct the prose, or correct the fact." >&2
	exit 1
fi

echo "  ${module_count} workspace modules, ${binary_count} binaries, ${member_count} merge-gate members"
echo "  the documented facts agree with the repository"
