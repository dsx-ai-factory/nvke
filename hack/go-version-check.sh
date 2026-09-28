#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# go.work owns the Go version. Everything that can derive it already does:
# the root Makefile and controller/Makefile compute GOTOOLCHAIN from it, and
# make/tool-versions.mk derives GOVULNCHECK_GO_VERSION from it.
#
# The copies below cannot derive. A Dockerfile FROM cannot call make, and the
# Flox manifest is static. So they are hand-kept, and until this check existed
# nothing noticed when one drifted. govulncheck.yml says "Keep this in step
# with the Dockerfiles", which was a comment asking a human to remember.
#
# Dependabot bumps `golang` image tags on its own schedule, so drift arrives
# without anyone deciding to cause it. That is the case this catches.
#
# The `go` directive in each member's go.mod is checked here too. Those can
# derive, in the sense that nothing stops them -- but they had drifted across
# three values (1.26.0, 1.26.5, 1.26.6) under one workspace, so a contributor's
# `go build` in one module and CI could resolve different compilers. `--fix`
# rewrites them, because a Go patch bump otherwise means editing eleven files
# by hand and the cheap thing has to be the correct thing.
#
# The module list is not enumerated here. The Makefile's WORK_MODULES and
# PRIVATE_MODULES are what the rest of the repository builds and syncs from,
# and a second copy of that list here would be one more thing
# to drift -- which is the failure this script exists to prevent. `make`
# passes both lists in; run this through `make go-version-check`, not
# directly. Anything tracked and not on either list is an error, so a new
# module cannot slip past unchecked.
#
# See docs/go-workspace.md, "The Go version", for the policy this enforces.

set -euo pipefail

usage() {
	cat >&2 <<'EOF'
usage: go-version-check.sh --modules "<list>" [--private "<list>"] [--fix]

Run it through make, which supplies the module lists:
    make go-version-check    # report drift
    make go-version-sync     # rewrite the go.mod directives
EOF
	exit 2
}

fix=0
modules=
private=
modules_set=0
while (( $# )); do
	case "$1" in
		--fix) fix=1; shift ;;
		--modules) [[ $# -ge 2 ]] || usage; modules="$2"; modules_set=1; shift 2 ;;
		--private) [[ $# -ge 2 ]] || usage; private="$2"; shift 2 ;;
		*) echo "unknown argument: $1" >&2; usage ;;
	esac
done
(( modules_set )) || usage

cd "$(dirname "$0")/.."

want="$(awk '/^go /{print $2; exit}' go.work)"
[[ -n "${want}" ]] || { echo "ERROR: no go directive in go.work" >&2; exit 2; }

fail=0
# `drift` is the subset of `fail` that is a version mismatch, as opposed to a
# module that is classified nowhere. Only the former has a fix to point at.
drift=0
report() {
	echo "ERROR: $1 declares Go $2, but go.work says ${want}" >&2
	fail=1
	drift=1
}

# Rewrites are staged through a temporary file rather than `sed -i`: the
# in-place flag takes an argument on BSD sed and not on GNU, so the portable
# form is this one. An interrupt between the write and the move would
# otherwise leave the staging file behind.
tmp=
cleanup() { [[ -n "${tmp}" && -f "${tmp}" ]] && rm -f "${tmp}"; return 0; }
trap cleanup EXIT

# Rewrite the first `go ` directive of a go.mod-shaped file in place.
set_directive() {
	tmp="$1.gover.tmp"
	awk -v want="${want}" '!seen && /^go /{print "go " want; seen=1; next} {print}' \
		"$1" > "${tmp}"
	mv "${tmp}" "$1"
	tmp=
}

# Every `golang:<version>` in a Dockerfile, pinned or not, with or without a
# registry variable or a digest suffix.
while IFS=: read -r file line rest; do
	ver="$(printf '%s' "${rest}" | sed -E 's|.*golang:([0-9]+\.[0-9]+(\.[0-9]+)?).*|\1|')"
	[[ "${ver}" == "${want}" ]] || report "${file}:${line}" "${ver}"
done < <(git grep -nE 'golang:[0-9]+\.[0-9]+' -- '*Dockerfile*' || true)

# The Flox manifest: `go = { pkg-path = "go", version = "X.Y.Z", ... }`
if [[ -f .flox/env/manifest.toml ]]; then
	flox="$(sed -nE 's/^go = .*version = "([^"]+)".*/\1/p' .flox/env/manifest.toml)"
	[[ -z "${flox}" || "${flox}" == "${want}" ]] || report ".flox/env/manifest.toml" "${flox}"
fi
hand_kept_drift=${drift}

# Every tracked go.mod is either a workspace member or deliberately private.
# One that is neither would be built by nothing and checked by nothing.
known=" $(for m in ${modules} ${private}; do printf '%s/go.mod ' "${m#./}"; done)"
while IFS= read -r modfile; do
	[[ "${known}" == *" ${modfile} "* ]] && continue
	echo "ERROR: ${modfile} is in neither WORK_MODULES nor PRIVATE_MODULES." >&2
	echo "       Add it to one of those lists in the Makefile." >&2
	fail=1
done < <(git ls-files -- '*go.mod' ':(exclude,glob)**/vendor/**' ':(exclude)vendor/*')

# Every member module's `go` directive. go.work already requires ${want} to
# build the workspace at all, so a member claiming less is claiming something
# the repository does not test. PRIVATE_MODULES are left alone: they are
# outside the workspace, so go.work makes no claim about them.
fixed=0
for m in ${modules}; do
	modfile="${m#./}/go.mod"
	[[ -f "${modfile}" ]] || { echo "ERROR: ${modfile} does not exist" >&2; fail=1; continue; }
	ver="$(awk '/^go /{print $2; exit}' "${modfile}")"
	[[ -n "${ver}" ]] || { echo "ERROR: no go directive in ${modfile}" >&2; fail=1; continue; }
	[[ "${ver}" == "${want}" ]] && continue
	if (( fix )); then
		set_directive "${modfile}"
		echo "  ${modfile}: ${ver} -> ${want}"
		fixed=$((fixed + 1))
	else
		report "${modfile}" "${ver}"
	fi
done

if (( fix )); then
	if (( fixed )); then
		echo "Rewrote ${fixed} go.mod directives to ${want}. Review and commit."
	else
		echo "Every go.mod already declares Go ${want}."
	fi
	# The Dockerfile and Flox copies cannot be rewritten from here, so a
	# --fix run still has to fail when one of them has drifted -- but it
	# should not tell you to run the thing you just ran.
	if (( hand_kept_drift )); then
		echo >&2
		echo "The copies above are hand-kept and were not rewritten." >&2
		echo "Edit them to ${want} by hand: see docs/go-workspace.md, \"Moving it\"." >&2
		exit 1
	fi
	if (( fail )); then exit 1; fi
	exit 0
fi

if (( fail )); then
	if (( drift )); then
		echo >&2
		echo "go.work is the single owner. Update the copies above to ${want}." >&2
		echo "For the go.mod directives:  make go-version-sync" >&2
	fi
	exit 1
fi

echo "Go ${want} from go.work, and every hand-kept copy agrees."
