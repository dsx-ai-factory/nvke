#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Enforce the rules .grype.yaml states in prose.
#
# The preamble of that file has always said three things: scope an entry to the
# narrowest thing that describes the root cause, say who can remove it, and
# re-check it on a date. Nothing checked any of them, so they held only for as
# long as everyone remembered to read a comment before adding an entry. This is
# what checks them.
#
# Three modes:
#
#   grype-ignore-check.sh [config]
#       Static validation. Reads files only, needs no network, no scanner and
#       no YAML tooling, so it runs in the merge gate and on a laptop.
#
#   grype-ignore-check.sh --count [config]
#       Print how many entries there are, so a caller can skip work when the
#       list is empty. Validates nothing.
#
#   grype-ignore-check.sh --applied <source|image> <grype-json>... [-- config]
#       Staleness. Given grype's JSON report, assert every entry declaring that
#       scope actually suppressed something. An entry whose upstream has shipped
#       a fix matches nothing, and a silent no-op entry is exactly what hides
#       the next real finding behind a green check.
#
# Why the owner and review date live inside `reason` rather than in keys of
# their own: grype defines the schema for an ignore rule, and a key it does not
# know is a key it is free to reject -- which would turn a policy annotation
# into a broken scan. `reason` is the one field it carries verbatim and never
# interprets, so the tokens cannot break grype, and they stay in the same file
# and the same entry as the rule they govern.
#
# The YAML parsing is deliberately literal and deliberately strict. This file is
# hand-written, short, and policy-controlled; anything this script cannot parse
# with certainty it rejects and says how to write instead, rather than guessing.
# A checker that silently skips an entry it did not understand is worse than no
# checker, because it reports success.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

mode=static
want_scope=""
reports=()

case "${1:-}" in
--count)
	mode=count
	shift
	;;
--applied)
	mode=applied
	shift
	[[ $# -ge 2 ]] || {
		echo "usage: $0 --applied <source|image> <grype-json>... [-- <config>]" >&2
		exit 2
	}
	want_scope="$1"
	shift
	case "${want_scope}" in
	source | image) ;;
	*)
		echo "ERROR: --applied scope must be 'source' or 'image', got '${want_scope}'" >&2
		exit 2
		;;
	esac
	while [[ $# -gt 0 && "$1" != "--" ]]; do
		reports+=("$1")
		shift
	done
	[[ ${#reports[@]} -gt 0 ]] || {
		echo "ERROR: --applied needs at least one grype JSON report" >&2
		exit 2
	}
	[[ "${1:-}" == "--" ]] && shift
	;;
esac

config="${1:-${repo_root}/.grype.yaml}"
rel_config="${config#"${repo_root}/"}"

[[ -f "${config}" ]] || {
	echo "ERROR: no such config: ${config}" >&2
	exit 2
}

error() {
	if [[ "${GITHUB_ACTIONS:-false}" == "true" ]]; then
		echo "::error file=${rel_config}::$*" >&2
	else
		echo "ERROR: ${rel_config}: $*" >&2
	fi
}

entries="$(mktemp)"
meta="$(mktemp)"
parse_errors="$(mktemp)"
applied="$(mktemp)"
trap 'rm -f "${entries}" "${meta}" "${parse_errors}" "${applied}"' EXIT

# Flatten the ignore list into `<index>\t<key>\t<value>` records. Only the keys
# the policy checks are emitted; the rest of the schema belongs to grype, and
# this script must not become a second, lagging copy of it.
#
# The parser demands the canonical layout: `ignore:` at column 0, entries at
# `  - `, entry keys at four spaces, `package:` sub-keys at six.
awk -v errfile="${parse_errors}" '
	function fail(msg) { print "line " NR ": " msg > errfile; bad = 1 }

	function strip(s) {
		gsub(/^[[:space:]]+|[[:space:]]+$/, "", s)
		if (s ~ /^".*"$/ || s ~ /^'"'"'.*'"'"'$/) { s = substr(s, 2, length(s) - 2) }
		return s
	}

	function emit(text,   key, val) {
		key = text; sub(/:.*$/, "", key)
		val = strip(substr(text, length(key) + 2))
		in_package = (key == "package" && val == "")
		if (!in_package) { print n "\t" key "\t" val }
	}

	{ line = $0 }
	line ~ /^[[:space:]]*#/ { next }
	line ~ /^[[:space:]]*$/ { next }

	!in_ignore {
		if (line ~ /^ignore:[[:space:]]*(\[\][[:space:]]*)?$/) {
			in_ignore = 1
			# `ignore: []` is the empty list: nothing follows it.
			if (line ~ /\[\]/) { empty = 1 }
		}
		next
	}

	# A list entry at the wrong indent. Caught here, before the rule below,
	# because an un-indented `- ` looks exactly like a new top-level key and
	# would otherwise end the list silently -- leaving every entry after it
	# unvalidated and the file reported as clean.
	line ~ /^[[:space:]]*- / && line !~ /^  - / {
		fail("list entry must be indented exactly two spaces")
		next
	}

	# A new top-level key ends the list.
	line ~ /^[^[:space:]]/ { in_ignore = 0; next }

	empty { fail("`ignore: []` is followed by list entries; drop the `[]`"); next }

	# Entry start.
	line ~ /^  - [^[:space:]]/ {
		n++
		emit(substr(line, 5))
		next
	}

	# Entry-level key.
	line ~ /^    [^[:space:]-]/ {
		if (n == 0) { fail("key outside any list entry"); next }
		emit(substr(line, 5))
		next
	}

	# Six spaces: a `package:` sub-key, or a continuation line of a multi-line
	# plain scalar. Continuations are only ever prose -- the policy tokens must
	# sit on the `reason:` line itself, so this script never has to reassemble
	# a folded scalar to find them.
	line ~ /^      [^[:space:]-]/ {
		if (!in_package) { next }
		key = substr(line, 7); sub(/:.*$/, "", key)
		print n "\tpackage." key "\t" strip(substr(line, 7 + length(key) + 1))
		next
	}

	line ~ /^      / { next }

	{ fail("cannot parse: " line) }

	END {
		print "COUNT\t" n "\t"
		if (bad) { print "PARSE_FAILED\t0\t" }
	}
' "${config}" >"${entries}"

if grep -q '^PARSE_FAILED' "${entries}"; then
	while IFS= read -r l; do error "${l}"; done <"${parse_errors}"
	error "see the format documented at the top of hack/grype-ignore-check.sh"
	exit 1
fi

count="$(awk -F'\t' '$1 == "COUNT" { print $2 }' "${entries}")"
count="${count:-0}"

if [[ "${mode}" == "count" ]]; then
	echo "${count}"
	exit 0
fi

field() { awk -F'\t' -v i="$1" -v k="$2" '$1 == i && $2 == k { print $3; exit }' "${entries}"; }
has_field() { awk -F'\t' -v i="$1" -v k="$2" '$1 == i && $2 == k { f = 1 } END { exit !f }' "${entries}"; }

# GNU date and BSD date disagree about everything except exit status, and BSD
# date does not even agree there: it accepts 2026-02-31 and silently reports
# back March 3rd. So the test is a round trip -- a date that normalises to
# something else was never a date.
valid_date() {
	local got
	got="$(date -u -d "$1" +%Y-%m-%d 2>/dev/null)" ||
		got="$(date -u -j -f %Y-%m-%d "$1" +%Y-%m-%d 2>/dev/null)" ||
		return 1
	[[ "${got}" == "$1" ]]
}

# Owners are GitHub handles or teams, so "who can remove this" resolves to
# someone who can actually be asked, and review routing can reach them. A
# free-text name goes stale the moment that person changes teams.
owner_re='^@[A-Za-z0-9][A-Za-z0-9-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)?$'

# CVE-, GHSA-, GO-, ALAS2-, RHSA- and friends all share this shape. The point is
# to reject a bare package name or a wildcard: an entry that names no single
# advisory suppresses the next, unrelated one in the same package too.
vuln_re='^[A-Z][A-Z0-9]*-[0-9A-Za-z][0-9A-Za-z.-]*$'

today="$(date -u +%Y-%m-%d)"
fail=0

for ((i = 1; i <= count; i++)); do
	vuln="$(field "${i}" vulnerability)"
	pkg="$(field "${i}" package.name)"
	reason="$(field "${i}" reason)"
	label="entry ${i}${vuln:+ (${vuln})}"

	# Rule 1: name the vulnerability and the artifact precisely.
	if [[ -z "${vuln}" ]]; then
		error "${label}: no 'vulnerability:' key; an entry naming no advisory suppresses every future one too"
		fail=1
	elif [[ ! "${vuln}" =~ ${vuln_re} ]]; then
		error "${label}: '${vuln}' is not an advisory id (CVE-..., GHSA-..., GO-...)"
		fail=1
	fi

	if ! has_field "${i}" package.name; then
		error "${label}: no 'package.name:'; scope the entry to the package that carries the root cause"
		fail=1
	elif [[ -z "${pkg}" || "${pkg}" == "*" ]]; then
		error "${label}: package.name '${pkg}' matches anything; scope it to one package"
		fail=1
	fi

	# Rules 2 and 3: ownership and a review date, both machine-checked.
	if [[ -z "${reason}" ]]; then
		error "${label}: no 'reason:'; it carries the owner, the review date and the scope"
		fail=1
		continue
	fi

	read -r t_owner t_review t_scope prose <<<"${reason}"

	if [[ "${t_owner}" != owner=* || "${t_review}" != review=* || "${t_scope}" != scope=* ]]; then
		error "${label}: reason must begin 'owner=<@handle> review=<YYYY-MM-DD> scope=<source|image|both>' followed by prose, got: ${reason}"
		fail=1
		continue
	fi

	owner="${t_owner#owner=}"
	review="${t_review#review=}"
	scope="${t_scope#scope=}"

	if [[ ! "${owner}" =~ ${owner_re} ]]; then
		error "${label}: owner '${owner}' is not a GitHub handle or team (@user, @org/team)"
		fail=1
	fi

	case "${scope}" in
	source | image | both) ;;
	*)
		error "${label}: scope '${scope}' must be source, image or both; it says which scan is entitled to call the entry stale"
		fail=1
		;;
	esac

	if [[ ! "${review}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || ! valid_date "${review}"; then
		error "${label}: review date '${review}' is not a real YYYY-MM-DD date"
		fail=1
	elif [[ "${today}" > "${review}" ]]; then
		# Unlike the govulncheck waiver review date, which only warns, this one
		# fails. That waiver has an independent un-waive check that catches rot
		# on its own, so the date there is a nudge. Here the date is the check,
		# and a warning nobody is forced to act on leaves the entry forever.
		error "${label}: review date ${review} has passed (owner: ${owner}). Re-check the finding, then renew the date or remove the entry."
		fail=1
	fi

	if [[ -z "${prose}" ]]; then
		error "${label}: the reason says who and when but not why; add the explanation after scope=${scope}"
		fail=1
	fi

	printf '%s\t%s\t%s\t%s\n' "${i}" "${vuln}" "${pkg}" "${scope}" >>"${meta}"
done

[[ ${fail} -eq 0 ]] || exit 1

if [[ "${mode}" == "static" ]]; then
	if [[ "${count}" -eq 0 ]]; then
		echo "grype ignore policy: no exceptions"
	else
		echo "grype ignore policy: ${count} exception(s), each owned, scoped and within its review date"
	fi
	exit 0
fi

# --- staleness -------------------------------------------------------------
#
# grype records, per suppressed match, which ignore rules applied to it. A rule
# that appears nowhere in that list suppressed nothing. The match is on advisory
# and package together, because the same advisory is routinely ignored once per
# affected package -- CVE-x in libssl3 and CVE-x in libcrypto3 are two entries,
# and matching on the advisory alone would let either one vouch for the other.
# The reports are unioned
# because one scan is not the whole picture: an entry for an OS package in the
# published image never matches the source-tree scan, which is why each entry
# declares the scope that is entitled to judge it.

command -v jq >/dev/null || {
	echo "ERROR: --applied needs jq" >&2
	exit 2
}

for r in "${reports[@]}"; do
	[[ -s "${r}" ]] || {
		echo "ERROR: grype report '${r}' is missing or empty; refusing to call entries stale on no evidence" >&2
		exit 2
	}
	jq -r '(.ignoredMatches // [])[] | (.appliedIgnoreRules // [])[] | select(.vulnerability) | "\(.vulnerability)\t\(.package.name // "")"' "${r}"
done | sort -u >"${applied}"

while IFS=$'\t' read -r i vuln pkg scope; do
	[[ "${scope}" == "both" || "${scope}" == "${want_scope}" ]] || continue
	if ! grep -qxF "${vuln}"$'\t'"${pkg}" "${applied}"; then
		error "entry ${i} (${vuln} in ${pkg}) suppressed nothing in the ${want_scope} scan. Either the finding is gone and the entry should go with it, or the entry no longer matches it."
		fail=1
	fi
done <"${meta}"

[[ ${fail} -eq 0 ]] || exit 1
echo "grype ignore policy: every ${want_scope}-scoped exception still suppresses a finding"
