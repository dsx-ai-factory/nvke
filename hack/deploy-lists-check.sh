#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Fail when an nvke-deploy.yaml item has no directory; deploy/nvke-deploy.sh dies there.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
status=0
lists=0
items=0
while IFS= read -r list; do
	lists=$((lists + 1))
	deploy_path="$(dirname "${list}")"
	parsed="$(python3 deploy/lib/nvke-deploy-items.py items "${list}")"
	if [[ -z "${parsed}" ]]; then
		echo "ERROR: ${list}: no items parsed" >&2
		status=1
		continue
	fi
	while IFS=$'\t' read -r name type; do
		[[ -n "${name}" ]] || continue
		items=$((items + 1))
		if [[ ! -d "${deploy_path}/${name}" ]]; then
			echo "ERROR: ${list}: item ${name} (${type}) has no directory at ${deploy_path}/${name}" >&2
			status=1
		fi
	done <<<"${parsed}"
done < <(git ls-files -- '*/nvke-deploy.yaml')

if [[ ${lists} -eq 0 ]]; then
	echo "ERROR: no nvke-deploy.yaml found; the check matched nothing" >&2
	exit 1
fi
[[ ${status} -ne 0 ]] || echo "deploy lists: ${items} item(s) in ${lists} list(s), each with its directory"
exit "${status}"
