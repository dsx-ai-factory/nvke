#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Delete old os-image container versions from GHCR.
#
#   os-image/prune-ghcr-versions.sh            # say what it would delete
#   os-image/prune-ghcr-versions.sh --delete   # actually delete
#
# Every build publishes a sha-<commit>-<arch> tag and a qcow2 is gigabytes, so
# without this the registry grows without limit.
#
# A version goes only when all three hold:
#
#   1. Every tag on it starts with "sha-", or it has none. GHCR puts a release
#      tag and its sha- tag on the SAME version, so requiring EVERY tag to be a
#      sha- tag is what keeps a release, and any moving tag, alive.
#   2. It is not among the newest KEEP_MIN of its package.
#   3. It is older than KEEP_DAYS.
#
# Needs gh with delete:packages, or GITHUB_TOKEN with packages: write.
set -euo pipefail

ORG="${ORG:-dsx-ai-factory}"
PACKAGES="${PACKAGES:-nke/os-image-fde nke/os-image-non-fde}"
KEEP_DAYS="${KEEP_DAYS:-7}"
KEEP_MIN="${KEEP_MIN:-3}"
DELETE=0

case "${1:-}" in
    -h|--help) awk 'NR>1 && /^#/ {sub(/^# ?/,""); print; next} NR>1 {exit}' "${BASH_SOURCE[0]}"; exit 0 ;;
    --delete) DELETE=1 ;;
    "") ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
esac

command -v gh >/dev/null || { echo "gh is required" >&2; exit 1; }
cutoff=$(( $(date +%s) - KEEP_DAYS * 86400 ))
total_deleted=0

for pkg in $PACKAGES; do
    enc="${pkg//\//%2F}"
    echo "== ${pkg}: keeping named tags, the newest ${KEEP_MIN}, and anything under ${KEEP_DAYS} days"

    # One "<verdict> <id> <created> <why>" line per version, newest first.
    verdicts="$(gh api --paginate \
        "orgs/${ORG}/packages/container/${enc}/versions?per_page=100" \
        --jq '.[]|{id, created_at, tags: (.metadata.container.tags // [])}' 2>/dev/null |
        KEEP_MIN="$KEEP_MIN" CUTOFF="$cutoff" python3 -c '
import json, os, sys, time
cutoff, keep_min = int(os.environ["CUTOFF"]), int(os.environ["KEEP_MIN"])
versions = [json.loads(line) for line in sys.stdin if line.strip()]
versions.sort(key=lambda v: v["created_at"], reverse=True)
kept = 0
for v in versions:
    stamp = time.mktime(time.strptime(v["created_at"], "%Y-%m-%dT%H:%M:%SZ"))
    tags, vid, at = v["tags"], v["id"], v["created_at"]
    kind = "sha-only" if tags else "untagged"
    named = ",".join(tags)
    if tags and not all(t.startswith("sha-") for t in tags):
        print(f"KEEP {vid} {at} named tag: {named}")
    elif kept < keep_min:
        kept += 1
        print(f"KEEP {vid} {at} {kind}, in the newest {keep_min}")
    elif stamp >= cutoff:
        print(f"KEEP {vid} {at} {kind}, under the age limit")
    else:
        print(f"DELETE {vid} {at} {kind}, over the age limit")
')"

    [[ -n "$verdicts" ]] || { echo "   no versions"; continue; }
    while read -r verdict vid created why; do
        if [[ "$verdict" == KEEP ]]; then
            printf '   keep   %-12s %s  %s\n' "$vid" "$created" "$why"
            continue
        fi
        if (( DELETE )); then
            gh api --method DELETE \
                "orgs/${ORG}/packages/container/${enc}/versions/${vid}" >/dev/null
            printf '   DELETED %-11s %s  %s\n' "$vid" "$created" "$why"
        else
            printf '   would delete %-6s %s  %s\n' "$vid" "$created" "$why"
        fi
        total_deleted=$(( total_deleted + 1 ))
    done <<< "$verdicts"
done

if (( DELETE )); then
    echo "deleted ${total_deleted} version(s)"
else
    echo "would delete ${total_deleted} version(s); re-run with --delete"
fi
