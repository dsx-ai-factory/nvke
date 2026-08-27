#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Print every tracked shell script, NUL separated, for `xargs -0`.
#
# Found by extension and by shebang, because `*.sh` alone missed 11 tracked
# scripts, one of which a Go test runs. This was inline in shellcheck.yml, where
# a developer could not run it. It is a script, not a recipe, because make runs
# recipes under /bin/sh and this needs `read -d ''`.
#
# No `mapfile`. That is a bash 4.4 builtin, and on macOS bash 3.2 it fails,
# leaves the array empty and exits 0, so a caller lints zero files and passes.
set -euo pipefail

{
	git ls-files -z -- '*.sh' ':(exclude)vendor/*'

	# Shebang-bearing files with no extension. Match narrowly: siblings start
	# `#!/usr/bin/env python3` and `#!ipxe`. Take the word after `env` when
	# present, so `#!/bin/bash -e` and `#!/usr/bin/env bash` both resolve.
	while IFS= read -r -d '' path; do
		case "${path##*/}" in *.*) continue ;; esac
		[ -f "${path}" ] || continue
		interp=$(
			sed -n '1s/^#![[:space:]]*//p' "${path}" |
				awk 'NR == 1 { if ($1 ~ /\/env$/) print $2; else print $1 }'
		)
		case "${interp##*/}" in
		sh | bash | dash | ksh) printf '%s\0' "${path}" ;;
		esac
	done < <(git ls-files -z -- ':(exclude)vendor/*')
} | sort -zu
