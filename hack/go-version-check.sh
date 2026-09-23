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

set -euo pipefail

cd "$(dirname "$0")/.."

want="$(awk '/^go /{print $2; exit}' go.work)"
[[ -n "${want}" ]] || { echo "ERROR: no go directive in go.work" >&2; exit 2; }

fail=0
report() {
	echo "ERROR: $1 declares Go $2, but go.work says ${want}" >&2
	fail=1
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

if (( fail )); then
	echo >&2
	echo "go.work is the single owner. Update the copies above to ${want}." >&2
	exit 1
fi

echo "Go ${want} from go.work, and every hand-kept copy agrees."
