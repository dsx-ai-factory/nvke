#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Coverage collection across the workspace: the registry of which modules
# collect, the check that the registry still matches the tree, and the report
# that turns the profiles into something a person reads.
#
# READ docs/coverage-policy.md FIRST. The short version, because it is the part
# people skip: a coverage percentage says which lines a test binary executed. It
# says nothing about whether anything was asserted about them. It is therefore
# not a measure of test adequacy, and nothing here fails on a percentage. What
# this does fail on is a profile that is missing, malformed or empty, and a
# package whose tests stopped being run -- which are the states that make a
# green run mean less than it looks like it means.
#
# Three subcommands:
#
#   check    The registry against the tree. No tests run, no profiles needed, a
#            second or so. This is the part the merge gate calls.
#   report   Validate and summarise the profiles already in coverage/. Fails on
#            a missing, malformed or empty profile, and on a package that has
#            tests but appears in no profile.
#   run      `make test` in every collecting module, then report. What the root
#            `make coverage` target calls.
#
# No `mapfile` anywhere: it is a bash 4.4 builtin, and on macOS bash 3.2 it
# leaves the array empty and exits 0, so the caller checks nothing and passes.
# Same reason as hack/shell-scripts.sh.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

coverage_dir="${COVERAGE_DIR:-${repo_root}/coverage}"

# ---------------------------------------------------------------------------
# The registry
# ---------------------------------------------------------------------------
#
# Every module in go.work is in exactly one of these two lists, and `check`
# fails when one is in neither -- the same shape as WORK_MODULES and
# PRIVATE_MODULES in the root Makefile, and for the same reason. A new module
# that nobody classified would otherwise collect nothing and never be noticed,
# which is the state six of the nine that have tests were in.

# Modules that have tests and therefore write a profile. Each one's Makefile
# includes make/coverage.mk and appends $(GO_COVER_FLAGS) to its `go test`.
COVERAGE_MODULES=(
	agent
	auth
	cli
	cluster-api-provider-provisioned
	controller
	controller/api
	controller/deploy/charts
	controller/nico-sim
	controller/test/e2e
	management-engine
	os-image
	release
)

# Modules with no test files at all, so there is nothing to collect. `check`
# fails when one of these gains a _test.go, because a module that starts
# testing and does not start collecting is exactly the drift this guards.
#
#   controller/hack/capd-reboot-controller      a development helper for the
#                                               CAPD backend, run by hand.
NO_TEST_MODULES=(
	controller/hack/capd-reboot-controller
)

# Test suites a build tag keeps out of the default build, so a coverage run
# never compiles them and no profile can mention them. Each needs a reason, and
# `report` fails on a tag-excluded test package that is not here -- that is the
# "a build tag quietly excluded a suite" case, which otherwise leaves CI green
# and is the single thing hardest to notice by reading a diff.
#
# A row here is a waiver, not a decision to stop testing. Every one of these
# does run somewhere: in a self-hosted lane, against a live service, or in a
# compile-only `go vet -tags ...` step in controller-test.yml.
#
# Format: <repo-relative package dir><tab><reason>. Tab separated so a reason
# can contain spaces.
untested_packages() {
	cat <<-'EOF'
		cli/e2e/bootstrap	behind `-tags e2e`; driven by e2e/bootstrap/run.sh against a built nvkectl
		controller/e2e/gofail	behind `-tags gofail`; runs in gofail-e2e.yml against a fault-injection build
		controller/e2e/tracing	behind `-tags gofail`; runs with the gofail suite in gofail-e2e.yml
		controller/test/kubeadm	behind `-tags kubeadm_bringup`; needs two QEMU VMs on a self-hosted runner
		controller/test/kubeadm/internal/cloudinit	behind `-tags kubeadm_bringup`, with the suite above
		controller/test/oidctest/envtest/cases	behind `-tags oidc_integration`; runs with pinned Kubernetes envtest assets via `make -C controller test-oidc-reference`
		controller/nico-sim/test/integration/libvirt	behind `-tags integration`; needs libvirt and a node image
		controller/nico-sim/test/integration/qemu	behind `-tags integration`; needs QEMU and a node image
	EOF
}

# ---------------------------------------------------------------------------
# Reporting helpers
# ---------------------------------------------------------------------------

fail=0

# GitHub renders `::error::` as an annotation. Locally that is noise, so print
# the plain form there. Same shape as hack/docs-check.sh.
error() {
	if [[ "${GITHUB_ACTIONS:-false}" == "true" ]]; then
		echo "::error::$*" >&2
	else
		echo "ERROR: $*" >&2
	fi
	fail=1
}

note() { echo "  $*"; }

in_list() {
	local needle="$1"
	shift
	local item
	for item in "$@"; do
		[[ "${item}" == "${needle}" ]] && return 0
	done
	return 1
}

# Is <needle> one of the lines of <haystack>?
#
# Pure bash on purpose. The obvious spelling, `printf '%s\n' "${haystack}" |
# grep -qx -- "${needle}"`, is a SIGPIPE trap: `grep -q` exits at the first
# match, and with `set -o pipefail` the writer's 141 becomes the pipeline's
# status and `set -e` kills the script. It only fires when the writer is still
# writing when grep leaves, so it passes locally on a short list and fails in
# CI on a long one. Nothing in this script may pipe into a reader that exits
# early -- see cmd_check for the `head -1` that taught us this.
has_line() {
	case $'\n'"$1"$'\n' in
	*$'\n'"$2"$'\n'*) return 0 ;;
	esac
	return 1
}

# The module a repo-relative path belongs to: the longest module path that is a
# prefix of it. Longest wins, or controller/nico-sim/internal would resolve to
# controller -- the same deepest-first rule PREFLIGHT_MODULES uses.
owning_module() {
	local path="$1" best="" module
	while IFS= read -r module; do
		case "${path}/" in
		"${module}/"*) [[ ${#module} -gt ${#best} ]] && best="${module}" ;;
		esac
	done < <(./hack/workspace-modules.sh)
	printf '%s' "${best}"
}

# make/coverage.mk flattens the separators; keep one definition of that rule.
profile_name() { printf '%s' "${1//\//-}"; }

# Every profile belonging to a module: <name>.out, plus <name>.<suffix>.out for
# management-engine, whose API and envtest suites are separate invocations.
#
# The suffix separator is a dot, and it has to be: COVERAGE_NAME already spends
# the dash on the path separators, so `controller-*.out` would also match
# controller-nico-sim.out, controller-deploy-charts.out and
# controller-test-e2e.out, and the controller row would silently report four
# modules' statements as its own. See make/coverage.mk.
module_profiles() {
	local name
	name="$(profile_name "$1")"
	find "${coverage_dir}" -maxdepth 1 -type f \
		\( -name "${name}.out" -o -name "${name}.*.out" \) 2>/dev/null | LC_ALL=C sort
}

# Tracked test files, as the directories that hold them.
test_package_dirs() {
	git ls-files -- '*_test.go' ':(exclude)vendor/*' |
		sed 's|/[^/]*$||' | LC_ALL=C sort -u
}

# ---------------------------------------------------------------------------
# check: the registry against the tree
# ---------------------------------------------------------------------------

cmd_check() {
	local module modules
	modules="$(./hack/workspace-modules.sh)"

	# 1. Every workspace module is classified, and classified once.
	while IFS= read -r module; do
		local collects=0 untested=0
		in_list "${module}" "${COVERAGE_MODULES[@]}" && collects=1
		in_list "${module}" "${NO_TEST_MODULES[@]}" && untested=1
		if ((collects == 0 && untested == 0)); then
			error "${module} is in go.work but in neither COVERAGE_MODULES nor NO_TEST_MODULES in hack/coverage.sh — it would collect no coverage and nothing would say so"
		elif ((collects == 1 && untested == 1)); then
			error "${module} is in both COVERAGE_MODULES and NO_TEST_MODULES in hack/coverage.sh"
		fi
	done <<<"${modules}"

	# 2. The lists are not stale in the other direction.
	for module in "${COVERAGE_MODULES[@]}" "${NO_TEST_MODULES[@]}"; do
		has_line "${modules}" "${module}" ||
			error "hack/coverage.sh lists ${module}, which is not a module in go.work"
	done

	# 3. A module that claims to have no tests really has none.
	for module in "${NO_TEST_MODULES[@]}"; do
		local found
		found="$(git ls-files -- "${module}/*_test.go")"
		found="${found%%$'\n'*}"
		[[ -z "${found}" ]] ||
			error "${module} is in NO_TEST_MODULES but now has tests (${found}) — include make/coverage.mk in its Makefile, add \$(GO_COVER_FLAGS) to its test target, and move it to COVERAGE_MODULES"
	done

	# 4. And a module that claims to collect has something to collect from,
	#    and still asks for the flags. Deleting -coverprofile from a test
	#    recipe is a one-character-looking change that silently stops
	#    collection; this is what notices.
	for module in "${COVERAGE_MODULES[@]}"; do
		local found makefile
		found="$(git ls-files -- "${module}/*_test.go")"
		found="${found%%$'\n'*}"
		[[ -n "${found}" ]] ||
			error "${module} is in COVERAGE_MODULES but has no test files — move it to NO_TEST_MODULES with a reason"

		makefile="${module}/Makefile"
		if [[ ! -f "${makefile}" ]]; then
			error "${module} is in COVERAGE_MODULES but has no Makefile for \`make test\` to collect through"
			continue
		fi
		grep -q 'include .*make/coverage\.mk' "${makefile}" ||
			error "${makefile} does not include make/coverage.mk, so it has no profile path to write to"
		grep -qE '\$\(GO_COVER_FLAGS\)|\$\(call coverprofile' "${makefile}" ||
			error "${makefile} never uses \$(GO_COVER_FLAGS), so \`make -C ${module} test\` writes no profile"
	done

	if ((fail != 0)); then
		echo >&2
		echo "The coverage registry in hack/coverage.sh no longer matches the repository." >&2
		return 1
	fi

	note "${#COVERAGE_MODULES[@]} modules collect coverage, ${#NO_TEST_MODULES[@]} have no tests to collect from"
	note "the coverage registry agrees with go.work and the tree"
}

# ---------------------------------------------------------------------------
# report: validate the profiles and summarise them
# ---------------------------------------------------------------------------

# Sum a module's profiles into "<covered> <total> <packages>".
#
# Blocks are keyed and deduplicated with the highest count seen, because
# management-engine's two suites both reach ./apis/... and double-counting
# their shared blocks would inflate the denominator as well as the numerator.
sum_profiles() {
	awk '
		# The header, once per file. Not a block.
		/^mode:/ { next }
		{
			# <file>:<l>.<c>,<l>.<c> <numStmt> <count>
			key = $1
			if (!(key in stmts)) { stmts[key] = $2; order[++n] = key }
			if ($3 + 0 > count[key]) count[key] = $3 + 0
			pkg = substr($1, 1, index($1, ":") - 1)
			sub(/\/[^\/]*$/, "", pkg)
			pkgs[pkg] = 1
		}
		END {
			for (i = 1; i <= n; i++) {
				key = order[i]
				total += stmts[key]
				if (count[key] > 0) covered += stmts[key]
			}
			np = 0
			for (p in pkgs) np++
			printf "%d %d %d\n", covered + 0, total + 0, np
		}
	' "$@"
}

# A profile go tool cover would reject, or one that holds no measurement. Both
# pass as "coverage was collected" to anything that only tests for existence.
validate_profile() {
	local profile="$1" header blocks bad

	header="$(head -1 "${profile}")"
	case "${header}" in
	"mode: set" | "mode: count" | "mode: atomic") ;;
	*)
		error "${profile#"${repo_root}"/}: first line is \"${header}\", not a \`mode:\` header — go tool cover cannot read this"
		return 1
		;;
	esac

	blocks="$(tail -n +2 "${profile}" | grep -c . || true)"
	if [[ "${blocks}" -eq 0 ]]; then
		error "${profile#"${repo_root}"/}: a mode header and no coverage blocks — the suite compiled and measured nothing"
		return 1
	fi

	# One malformed line is enough: `go tool cover` fails on the whole file,
	# and a truncated profile (a killed run, a full disk) looks like this.
	bad="$(tail -n +2 "${profile}" |
		grep -vcE '^[^ ]+\.go:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+ [0-9]+ [0-9]+$' || true)"
	if [[ "${bad}" -ne 0 ]]; then
		error "${profile#"${repo_root}"/}: ${bad} malformed block line(s) — the profile is truncated or not a coverage profile"
		return 1
	fi
}

cmd_report() {
	local module profiles profile rows="" repo_covered=0 repo_total=0
	local -a files

	[[ -d "${coverage_dir}" ]] || {
		error "no ${coverage_dir#"${repo_root}"/} directory — run \`make coverage\` (or \`make test\` in each module) before reporting"
		return 1
	}

	for module in "${COVERAGE_MODULES[@]}"; do
		profiles="$(module_profiles "${module}")"
		if [[ -z "${profiles}" ]]; then
			error "${module}: no coverage profile in ${coverage_dir#"${repo_root}"/} — \`make -C ${module} test\` did not run, or it ran without \$(GO_COVER_FLAGS)"
			rows+="${module}|-|-|no profile"$'\n'
			continue
		fi

		# An array, built by hand: `mapfile` is a bash 4.4 builtin and on
		# macOS bash 3.2 it leaves the array empty and exits 0, which here
		# would summarise no files and report every module as 0%.
		local ok=1
		files=()
		while IFS= read -r profile; do
			files+=("${profile}")
			validate_profile "${profile}" || ok=0
		done <<<"${profiles}"
		if ((ok == 0)); then
			rows+="${module}|-|-|unreadable profile"$'\n'
			continue
		fi

		local covered total packages pct
		read -r covered total packages < <(sum_profiles "${files[@]}")

		if [[ "${total}" -eq 0 ]]; then
			error "${module}: the profile holds 0 statements — every package was excluded, or the suite is empty"
			rows+="${module}|-|-|no statements"$'\n'
			continue
		fi

		repo_covered=$((repo_covered + covered))
		repo_total=$((repo_total + total))
		pct="$(awk -v c="${covered}" -v t="${total}" 'BEGIN { printf "%.1f", 100 * c / t }')"
		rows+="${module}|${pct}%|${packages}|${covered}/${total}"$'\n'
	done

	check_every_test_package_is_measured

	local repo_pct="n/a"
	[[ "${repo_total}" -gt 0 ]] &&
		repo_pct="$(awk -v c="${repo_covered}" -v t="${repo_total}" 'BEGIN { printf "%.1f", 100 * c / t }')%"

	emit_text_report "${rows}" "${repo_pct}" "${repo_covered}" "${repo_total}"
	if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
		emit_markdown_report "${rows}" "${repo_pct}" "${repo_covered}" "${repo_total}" >>"${GITHUB_STEP_SUMMARY}"
	fi

	if ((fail != 0)); then
		echo >&2
		echo "Coverage was not collected as declared. A run that cannot say what it measured has not measured anything." >&2
		return 1
	fi
}

# Every package the default build sees in a collecting module, as
# "<repo-relative dir> <non-test .go files> <test files that compile>".
#
# Two counts, because they answer two different questions.
#
# The test-file count is "do this package's tests compile without a build tag".
# A directory absent from this list entirely, or present with a count of zero,
# has no test that the default build runs -- which is the state a `//go:build`
# line produces, and the state that leaves CI green while a suite is gone.
#
# The .go-file count separates "has nothing to measure" from "was not measured".
# A package of nothing but _test.go files -- controller/test/e2e itself,
# os-image/files/test, controller/hack/capd-kindest-node -- contributes no
# statements by construction, so it can never appear in a profile however
# thoroughly it runs.
buildable_packages() {
	local module
	for module in "${COVERAGE_MODULES[@]}"; do
		(cd "${module}" &&
			go list -f '{{.Dir}} {{len .GoFiles}} {{len .TestGoFiles}} {{len .XTestGoFiles}}' ./...) |
			awk -v root="${repo_root}/" '
				# A literal prefix strip, not sub(): the checkout path is a
				# regex to sub(), and a worktree directory named
				# "ci+coverage-all-modules" then matches nothing, every lookup
				# below misses, and every package reports as excluded.
				{
					if (substr($1, 1, length(root)) == root) $1 = substr($1, length(root) + 1)
					print $1, $2, $3 + $4
				}'
	done
}

# The silently-unrun-tests check, and the reason this script exists rather than
# a line of make. A test file that stops being compiled -- a build tag, a
# renamed directory, a package dropped from a `go test` argument list -- takes
# its failures with it and leaves CI green.
#
# Three states for a directory that holds _test.go files, and only the third is
# "measured":
#
#   its tests do not compile     a build tag excludes them -- either the whole
#                                package, or every _test.go file in it.
#                                Legitimate here, but it has to be declared in
#                                untested_packages() so the exclusion is a
#                                decision someone made rather than an accident
#                                nobody saw.
#   they compile, no .go files   a test-only package. Coverage cannot represent
#                                it: there are no statements of its own to
#                                attribute. It still runs, and still fails the
#                                suite when it breaks. Exempt from the profile
#                                check, and from nothing else.
#   they compile, with .go files it must appear in the module's profile. Absent
#                                means the test target never reached it.
check_every_test_package_is_measured() {
	local measured listed row dir module modpath relative importpath excluded reason
	local gofiles testfiles

	measured="$(cat "${coverage_dir}"/*.out 2>/dev/null |
		grep -v '^mode:' | cut -d: -f1 | sed 's|/[^/]*$||' | LC_ALL=C sort -u || true)"
	excluded="$(untested_packages | cut -f1)"
	listed="$(buildable_packages)"

	while IFS= read -r dir; do
		module="$(owning_module "${dir}")"
		if [[ -z "${module}" ]]; then
			error "${dir} holds tests but is in no go.work module"
			continue
		fi
		# cmd_check owns the modules that collect nothing on purpose.
		in_list "${module}" "${COVERAGE_MODULES[@]}" || continue

		# No `exit` in the awk: leaving early would SIGPIPE the printf
		# feeding it, the same way `grep -q` does. See has_line.
		row="$(printf '%s\n' "${listed}" | awk -v d="${dir}" '$1 == d && !seen { print; seen = 1 }')"
		gofiles="$(printf '%s' "${row}" | awk '{print $2}')"
		testfiles="$(printf '%s' "${row}" | awk '{print $3}')"

		# No row at all, or a row with no compiling test file: a `//go:build`
		# line is keeping the suite out of this build.
		if [[ -z "${row}" || "${testfiles}" == "0" ]]; then
			has_line "${excluded}" "${dir}" ||
				error "${dir} holds tests that the default build does not compile — a build tag excludes them and nothing runs them here. Declare it in untested_packages() in hack/coverage.sh with the reason, or drop the tag."
			continue
		fi

		# Compiling and waived: the tag went away and the waiver did not.
		has_line "${excluded}" "${dir}" &&
			error "untested_packages() in hack/coverage.sh waives ${dir}, but the default build compiles its tests now — drop the row"

		# A test-only package has no statements of its own to attribute.
		[[ "${gofiles}" == "0" ]] && continue

		modpath="$(awk '/^module /{print $2; exit}' "${module}/go.mod")"
		relative="${dir#"${module}"}"
		relative="${relative#/}"
		importpath="${modpath}${relative:+/${relative}}"

		has_line "${measured}" "${importpath}" ||
			error "${dir} holds tests and compiles, but reaches no coverage profile — \`make -C ${module} test\` never ran it. Add it to that target's package arguments."
	done < <(test_package_dirs)

	# A waiver for a directory that no longer exists keeps a suite excluded
	# after whatever justified it is gone, and reads as deliberate forever.
	while IFS=$'\t' read -r dir reason; do
		[[ -n "${reason}" ]] ||
			error "untested_packages() in hack/coverage.sh waives ${dir} with no reason — give one or drop the row"
		[[ -d "${dir}" ]] ||
			error "untested_packages() in hack/coverage.sh waives ${dir}, which no longer exists — drop the row"
	done < <(untested_packages)
}

emit_text_report() {
	local rows="$1" repo_pct="$2" covered="$3" total="$4"
	echo
	printf '  %-36s %8s %9s %s\n' MODULE COVERED PACKAGES STATEMENTS
	printf '%s' "${rows}" | while IFS='|' read -r m p k s; do
		[[ -n "${m}" ]] && printf '  %-36s %8s %9s %s\n' "${m}" "${p}" "${k}" "${s}"
	done
	printf '  %-36s %8s %9s %s\n' TOTAL "${repo_pct}" '' "${covered}/${total}"
	echo
	echo "  A percentage is not test adequacy. See docs/coverage-policy.md."
}

emit_markdown_report() {
	local rows="$1" repo_pct="$2" covered="$3" total="$4"
	echo "### Coverage"
	echo
	echo "| Module | Covered | Packages | Statements |"
	echo "|---|---:|---:|---:|"
	printf '%s' "${rows}" | while IFS='|' read -r m p k s; do
		[[ -n "${m}" ]] && echo "| \`${m}\` | ${p} | ${k} | ${s} |"
	done
	echo "| **Total** | **${repo_pct}** | | ${covered}/${total} |"
	echo
	echo "No threshold gates on these numbers, by policy: a percentage measures which"
	echo "lines ran, not whether anything was asserted about them. What does fail is a"
	echo "missing, malformed or empty profile, and a package with tests that reaches no"
	echo "profile. The profiles are attached to this run as the \`coverage-profiles\`"
	echo "artifact; \`go tool cover -html=coverage/<module>.out\` opens one."
}

# ---------------------------------------------------------------------------
# run: collect, then report
# ---------------------------------------------------------------------------

cmd_run() {
	local module
	rm -rf "${coverage_dir}"
	for module in "${COVERAGE_MODULES[@]}"; do
		echo "- collecting ${module}"
		"${MAKE:-make}" --no-print-directory -C "${module}" test
	done
	cmd_report
}

case "${1:-run}" in
check) cmd_check ;;
report) cmd_report ;;
run) cmd_run ;;
*)
	echo "usage: hack/coverage.sh [check|report|run]" >&2
	exit 2
	;;
esac
