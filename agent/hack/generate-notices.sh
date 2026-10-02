#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Regenerate THIRD_PARTY_NOTICES.md: an index and the full license text of every
# Go module linked into the released commands.
#
# Usage:  make notices
#
# Configuration is by environment variable so the same script serves every
# repository in the fleet:
#
#   PACKAGES   space-separated package patterns to analyse   (default "./cmd/...")
#   PLATFORMS  space-separated GOOS/GOARCH pairs             (default "linux/amd64 linux/arm64")
#   OUTPUT     file to write                                 (default THIRD_PARTY_NOTICES.md)
#   GO_LICENSES  path to the pinned binary                   (default ./bin/go-licenses)

set -euo pipefail

# Byte-order collation, so the file is identical on every machine. Without this
# `sort` follows the ambient locale: macOS collates case-insensitively and puts
# github.com/beorn7 before github.com/NVIDIA, while Linux CI uses byte order and
# puts uppercase first. The generated file is then valid on the machine that
# wrote it and "stale" everywhere else, which makes the CI check unusable.
export LC_ALL=C

# THIRD_PARTY_NOTICES.md describes exactly one module's dependency closure, so
# this must not run in workspace mode. With a go.work above it, `go list -m`
# returns every module in the workspace rather than one, and that value is used
# as the module name in the output, passed to --ignore, and used by the
# self-attribution guard below -- so a workspace turns all three wrong at once
# while the script still exits 0 on a tolerant awk. No-op where there is no
# go.work.
export GOWORK=off

OUTPUT="${OUTPUT:-THIRD_PARTY_NOTICES.md}"
GO_LICENSES="${GO_LICENSES:-./bin/go-licenses}"
read -r -a PACKAGES <<<"${PACKAGES:-./cmd/...}"

# Accept commas as well as spaces. Several of these Makefiles already use
# PLATFORMS for `docker buildx --platform`, which is comma-separated, and make
# exports a command-line variable into every recipe. Without this, a single
# `make notices PLATFORMS=linux/amd64,linux/arm64` would parse as one platform
# with GOARCH "amd64,linux/arm64".
PLATFORMS_IN="${PLATFORMS:-linux/amd64 linux/arm64}"
read -r -a PLATFORMS <<<"${PLATFORMS_IN//,/ }"

die() { echo "ERROR: $*" >&2; exit 1; }
log() { echo "  $*" >&2; }

# A fenced block must use a fence longer than any backtick run inside it, or a
# license containing a code sample would terminate the block early.
fence_for() {
    local longest
    longest=$(grep -aoE '`+' "$1" 2>/dev/null | awk '{ if (length($0) > n) n = length($0) } END { print n+0 }')
    printf '%*s' $(( longest < 3 ? 3 : longest + 1 )) '' | tr ' ' '`'
}

[[ -x "${GO_LICENSES}" ]] || die "${GO_LICENSES} not found. Run 'make bin/go-licenses'."
command -v go >/dev/null || die "go is not on PATH."

LOCAL_MODULE="$(go list -m)"
[[ -n "${LOCAL_MODULE}" ]] || die "could not determine the local module path."

# go-licenses resolves everything as part of the local module when a vendor/
# directory is present, so every licence URL points back at this repository
# instead of upstream. Every package in the closure is rewritten that way, and
# the file then looks entirely plausible while asserting we are the licence
# source for other people's code -- the one failure THIRD_PARTY_NOTICES.md
# exists to prevent.
#
# GOFLAGS=-mod=mod makes go-licenses read the module graph instead. It has to go
# in the environment: go-licenses is a cobra program and would parse -mod=mod as
# short flags. Applied only when vendor/ exists, because -mod=mod is also the
# mode that lets the go tool rewrite go.mod, and in readonly mode an untidy
# module fails loudly here -- a signal worth keeping.
# `|| true` because shasum exits non-zero when go.sum is absent, and under
# `set -e` with pipefail that kills the script with no message at all. A module
# with no go.sum still hashes its go.mod, which is what the guard needs.
mod_digest() { shasum go.mod go.sum 2>/dev/null | shasum | cut -d" " -f1 || true; }

MOD_ENV=()
MOD_BEFORE=""
if [[ -d vendor ]]; then
    log "vendor/ present - reading the module graph rather than vendor/"
    MOD_ENV=(GOFLAGS=-mod=mod)
    MOD_BEFORE="$(mod_digest)"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
SAVE_ROOT="${WORK}/save"
LICENSES_DIR="${WORK}/licenses"
CSV="${WORK}/licenses.csv"
ERRLOG="${WORK}/go-licenses.err"
mkdir -p "${SAVE_ROOT}" "${LICENSES_DIR}"
: >"${CSV}"

# go-licenses is noisy on stderr for benign reasons -- packages that carry no
# licence file of their own, directories it cannot inspect -- so its diagnostics
# are held back rather than shown. Held back, not discarded: discarding them
# once hid a fatal argument error behind a bare non-zero exit, and the run then
# looked like an empty dependency tree rather than a broken invocation.
run_go_licenses() {
    : >"${ERRLOG}"
    env ${MOD_ENV[@]+"${MOD_ENV[@]}"} GOOS="${goos}" GOARCH="${goarch}" \
        "${GO_LICENSES}" "$@" 2>>"${ERRLOG}" && return 0
    cat "${ERRLOG}" >&2
    die "go-licenses ${1} failed for ${goos}/${goarch}."
}

# Collect per platform and merge. A dependency can be reachable on one GOARCH
# and not another, so a single-platform graph under-reports.
#
# Only the local module goes to --ignore. go-licenses already omits the standard
# library, and --ignore matches on plain string prefixes rather than path
# segments, so a short generic prefix silently drops unrelated dependencies —
# "go" would remove golang.org/x/*, google.golang.org/* and gopkg.in/* at once.
# Keep this to the local module.
for platform in "${PLATFORMS[@]}"; do
    goos="${platform%/*}"; goarch="${platform#*/}"
    log "collecting ${goos}/${goarch}"
    save_dir="${SAVE_ROOT}/${goos}_${goarch}"

    run_go_licenses save "${PACKAGES[@]}" \
        --save_path="${save_dir}" --force --ignore="${LOCAL_MODULE}"

    run_go_licenses csv "${PACKAGES[@]}" \
        --ignore="${LOCAL_MODULE}" >>"${CSV}"

    if [[ -d "${save_dir}" ]]; then
        (cd "${save_dir}" && find . -type f -print0) | while IFS= read -r -d '' f; do
            mkdir -p "${LICENSES_DIR}/$(dirname "${f}")"
            cp -n "${save_dir}/${f}" "${LICENSES_DIR}/${f}" 2>/dev/null || true
        done
    fi
done

[[ -s "${CSV}" ]] || die "go-licenses produced no rows for ${PACKAGES[*]}."

# Settle the major-version directory from the committed file, not the network.
#
# For a module in a repository subdirectory with a major version suffix --
# go.etcd.io/etcd/client/v3, tagged client/v3.7.1 -- the directory may be
# client/v3 or client, and etcd has both kinds: api/v3 lives in api/. The repair
# above cannot help, because the tag carries the subdirectory either way.
# go-licenses decides with a HEAD request for <dir>/vN/go.mod on github.com and,
# in its own words, "on any failure, assume that the right directory is the one
# without the version" (pkgsite's source.adjustVersionedModuleDirectory).
# GitHub-hosted runners are throttled often enough that the URL flips between
# runs, and notices-check cannot tell a flip from a change, since it compares
# the file against a regeneration of itself.
#
# So when the committed file already has a URL for the package that differs
# from the regenerated one only by that /vN segment, the committed URL stands:
# a human verified it once, and the probe has no information the file lacks.
# Anything else -- a new package, a new version, a different licence file --
# still shows up as a diff, and a new package keeps whatever go-licenses chose,
# to be verified when it is first committed. The root-module repair below runs
# after this step on purpose: a committed URL may carry the spurious /vN segment
# from before that repair existed, and the rule, not the file, decides for roots.
if [[ -f "${OUTPUT}" ]]; then
    SETTLED="${WORK}/licenses.settled.csv"
    awk -F, 'BEGIN { OFS = "," }
        # First file: the committed index rows, "| [pkg](url) | licence | module |".
        FNR == NR {
            if (match($0, /^\| \[[^]]+\]\([^)]+\)/)) {
                row = substr($0, RSTART, RLENGTH)
                pkg = substr(row, 4, index(row, "](") - 4)
                url = substr(row, index(row, "](") + 2)
                sub(/\)$/, "", url)
                committed[pkg] = url
            }
            next
        }
        # Second file: the regenerated CSV.
        {
            if (($1 in committed) && committed[$1] != $2) {
                a = $2; b = committed[$1]
                # Drop the first /vN/ segment from each; equal means they differ
                # only in that segment. The tag "client/v3.7.1" does not match:
                # a dot follows its digits, not a slash.
                a2 = a; sub(/\/v[0-9]+\//, "/", a2)
                b2 = b; sub(/\/v[0-9]+\//, "/", b2)
                if (a2 == b || b2 == a) { $2 = b }
            }
            print
        }' "${OUTPUT}" "${CSV}" >"${SETTLED}"
    SETTLED_N=$(diff <(cut -d, -f2 "${CSV}") <(cut -d, -f2 "${SETTLED}") | grep -c '^>' || true)
    if [[ "${SETTLED_N}" -gt 0 ]]; then
        log "kept ${SETTLED_N} committed major-version licence URL(s) over go-licenses' network-dependent choice"
    fi
    mv "${SETTLED}" "${CSV}"
fi

# Repair the licence URL of a major-version module.
#
# For a module whose path ends in /vN, go-licenses treats that suffix as a
# directory inside the repository and writes it into the URL:
#
#   https://github.com/blang/semver/blob/v4.0.0/v4/LICENSE      404
#   https://github.com/blang/semver/blob/v4.0.0/LICENSE         200
#
# The reference decides which is right, and it does so without guessing. Go
# requires a module in a subdirectory to be tagged <subdir>/vX.Y.Z, so a plain
# vX.Y.Z tag means the module sits at the repository root -- and a root module
# cannot have a directory prefix on its licence path. When the tag carries no
# slash and the next segment is exactly the major-version suffix, that segment
# is spurious.
#
# A module genuinely in a vN/ subdirectory is tagged vN/vX.Y.Z, does not match,
# and is left alone. Pseudo-versions and commit refs do not match either.
#
# This mattered beyond the dead links: go-licenses picks the two forms
# inconsistently between runs on identical input, so notices-check would pass on
# one run and fail on the next. notices-check cannot detect the wrong form on
# its own -- it compares the file against a regeneration of itself, so a wrong
# URL matches a wrong URL.
REPAIRED="${WORK}/licenses.repaired.csv"
awk -F, 'BEGIN { OFS = "," }
    {
        url = $2
        # https://<host>/<owner>/<repo>/blob/<ref>/<path>
        if (match(url, /\/blob\/[^\/]+\//)) {
            ref  = substr(url, RSTART + 6, RLENGTH - 7)
            rest = substr(url, RSTART + RLENGTH)
            # A plain semantic-version tag: the module is at the repository root.
            if (ref ~ /^v[0-9]+\.[0-9]+\.[0-9]+/ && rest ~ /^v[0-9]+\//) {
                major = substr(rest, 1, index(rest, "/") - 1)
                # Only when it matches this module major, e.g. .../json-patch/v5.
                if ($1 ~ ("/" major "$") || $1 ~ ("/" major "/")) {
                    $2 = substr(url, 1, RSTART + RLENGTH - 1) substr(rest, length(major) + 2)
                }
            }
        }
        print
    }' "${CSV}" >"${REPAIRED}"

REPAIRS=$(diff <(cut -d, -f2 "${CSV}") <(cut -d, -f2 "${REPAIRED}") | grep -c '^>' || true)
if [[ "${REPAIRS}" -gt 0 ]]; then
    log "repaired ${REPAIRS} major-version licence URL(s)"
fi
mv "${REPAIRED}" "${CSV}"

# -mod=mod is allowed to rewrite go.mod. Generating a documentation file must
# not quietly change the dependency set. Checked before the URL scan below, so
# that a run which both rewrote go.mod and self-attributed reports the rewrite
# rather than hiding it behind the other message.
if [[ -n "${MOD_BEFORE}" ]]; then
    [[ "$(mod_digest)" == "${MOD_BEFORE}" ]] \
        || die "go.mod or go.sum changed while generating notices. Run 'go mod tidy' and commit that separately."
fi

# Backstop, in case go-licenses still self-attributes. Anchored on the module
# path trimmed to its repository root, which is what go-licenses actually builds
# URLs from -- a module in a subdirectory (github.com/dsx-ai-factory/nke/agent) still
# yields https://github.com/dsx-ai-factory/nke/blob/..., so anchoring on the full module
# path would miss every row. Not taken from the git remote either: that varies
# by checkout (a fork remote would disable the check silently) while the URL
# does not.
#
# A package that genuinely lives in this repository is allowed to have a URL
# here: these are multi-module repositories, and a sibling module really is
# licensed by us. What must never happen is a THIRD-PARTY package carrying our
# URL, so the package path decides, not the URL alone.
SELF_ROOT="$(printf '%s\n' "${LOCAL_MODULE}" | cut -d/ -f1-3)"
SELF_REF="$(awk -F, -v root="${SELF_ROOT}" '
    BEGIN { esc = root; gsub(/\./, "\\.", esc); re = "^https://" esc "(/|$)" }
    $2 ~ re && $1 != root && index($1, root "/") != 1 { print $1 }
' "${CSV}" | sort -u)"
if [[ -n "${SELF_REF}" ]]; then
    log "third-party packages carrying a ${SELF_ROOT} licence URL:"
    while IFS= read -r pkg; do log "  ${pkg}"; done <<<"${SELF_REF}"
    die "$(wc -l <<<"${SELF_REF}" | tr -d ' ') package(s) above are not ours. go-licenses is attributing upstream code to this repository."
fi

# package -> module@version, so a reader can pin what was actually linked.
MODMAP="${WORK}/modmap"
for platform in "${PLATFORMS[@]}"; do
    goos="${platform%/*}"; goarch="${platform#*/}"
    GOOS="${goos}" GOARCH="${goarch}" go list -deps -buildvcs=false \
        -f '{{if .Module}}{{.ImportPath}} {{.Module.Path}}@{{.Module.Version}}{{end}}' \
        "${PACKAGES[@]}" 2>/dev/null
done | sort -u >"${MODMAP}"

# Join every licence seen for a package rather than taking the first: a package
# may be dual-licensed, and dropping one would misstate the terms.
INDEX="${WORK}/index"
sort -u "${CSV}" | awk -F, 'NF>=3 {
    pkg=$1; url=$2; lic=$3
    if (!(pkg in seen)) { order[++n]=pkg }
    if (index(seen[pkg], lic) == 0) { seen[pkg] = (seen[pkg]=="" ? lic : seen[pkg]" / "lic) }
    link[pkg]=url
} END { for (i=1;i<=n;i++) { p=order[i]; print p "\t" seen[p] "\t" link[p] } }' >"${INDEX}"

TMP="${WORK}/out.md"
{
    cat <<EOF
<!--
  Generated by hack/generate-notices.sh. Do not edit by hand.
  Regenerate with 'make notices'; 'make notices-check' verifies it in CI.
-->

# Third-Party Notices

This project links the Go modules listed below. Their licenses are reproduced in
full so that binaries and container images built from this repository ship with
the attribution those licenses require.

## Scope

Generated from the import closure of \`${PACKAGES[*]}\` in module
\`${LOCAL_MODULE}\`, for $(printf '%s ' "${PLATFORMS[@]}" | sed 's/ $//').

The Go standard library is excluded, as are packages belonging to this module.
Test-only and build-time dependencies that are not reachable from the released
commands are not listed, because they are not distributed.

## Dependency Index

| Package | License | Module |
|---|---|---|
EOF
    while IFS=$'\t' read -r pkg lic url; do
        mod=$(awk -v p="${pkg}" '$1==p {print $2; exit}' "${MODMAP}")
        printf '| [%s](%s) | %s | %s |\n' "${pkg}" "${url}" "${lic}" "${mod:-—}"
    done <"${INDEX}"

    echo
    echo "## License Texts"
    echo

    # Every attribution file for the package, not just the first. go-licenses
    # saves LICENSE and NOTICE side by side, and this loop used to take
    # `sort | head -1` — which under LC_ALL=C always kept LICENSE and always
    # discarded NOTICE. Apache-2.0 section 4(d) requires the NOTICE text to
    # travel with the distribution, and this file is how it travels.
    #
    # The licence file itself comes from the URL go-licenses reported, not from
    # a name pattern. Guessing is how the first attempt at this fix went wrong:
    # go-licenses accepts UNLICENSE and README as licence files (its
    # licenseRegexp is `^(?i)((UN)?LICEN(S|C)E|COPYING|README|NOTICE).*$`), and
    # a narrower allowlist drops such a dependency's entire licence text without
    # a word. The URL's last segment is the file it actually classified;
    # googlesource spells that "<tag>:LICENSE", hence the second strip.
    PKGFILES="${WORK}/pkgfiles"
    while IFS=$'\t' read -r pkg lic url; do
        : >"${PKGFILES}"
        classified="${url##*/}"; classified="${classified##*:}"
        if [[ -n "${classified}" && -f "${LICENSES_DIR}/${pkg}/${classified}" ]]; then
            printf '%s\n' "${LICENSES_DIR}/${pkg}/${classified}" >>"${PKGFILES}"
        fi

        # Plus any NOTICE. go-licenses saves it but never reports it, so it has
        # to be found by name. This is the file the old code discarded.
        find "${LICENSES_DIR}/${pkg}" -maxdepth 1 -type f -iname 'NOTICE*' \
            2>/dev/null >>"${PKGFILES}"

        # Fallback for a reported URL that names no saved file. Mirrors
        # go-licenses' own regexp rather than a narrower guess.
        if [[ ! -s "${PKGFILES}" ]]; then
            find "${LICENSES_DIR}/${pkg}" -maxdepth 1 -type f \
                \( -iname 'LICEN[SC]E*' -o -iname 'UNLICEN[SC]E*' \
                   -o -iname 'COPYING*' -o -iname 'README*' \) \
                2>/dev/null >>"${PKGFILES}"
        fi

        sort -u -o "${PKGFILES}" "${PKGFILES}"
        count=$(wc -l <"${PKGFILES}" | tr -d ' ')
        # Silence here would mean shipping a dependency with no attribution at
        # all, which is the one outcome this file exists to prevent.
        [[ "${count}" -gt 0 ]] || die "no attribution file for ${pkg} (go-licenses reported '${classified}')"

        printf '### %s\n\n' "${pkg}"
        printf '_%s_\n\n' "${lic}"
        while IFS= read -r f; do
            [[ -n "${f}" ]] || continue
            # Name the file only when there is more than one, so the common
            # single-licence case reads exactly as it did before.
            if [[ "${count}" -gt 1 ]]; then printf '**%s**\n\n' "$(basename "${f}")"; fi
            fence=$(fence_for "${f}")
            printf '%s\n' "${fence}"
            cat "${f}"
            printf '\n%s\n\n' "${fence}"
        done <"${PKGFILES}"
    done <"${INDEX}"
} >"${TMP}"

mv "${TMP}" "${OUTPUT}"
log "wrote ${OUTPUT} ($(wc -l <"${OUTPUT}" | tr -d ' ') lines, $(wc -l <"${INDEX}" | tr -d ' ') packages)"
