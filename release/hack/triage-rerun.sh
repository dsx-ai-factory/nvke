#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

# Re-runs the failed jobs of a qualification run once, when the triage rules say
# a re-run is what has fixed every one of its failures before. It reads the
# one-object-per-line output of `FORMAT=jsonl release/hack/triage-run.sh` on
# standard input, says what it decided as Markdown, and tells the run's Slack
# thread when it re-ran.
# usage: ATTEMPT=1 FORMAT=jsonl release/hack/triage-run.sh <RUN_ID> | release/hack/triage-rerun.sh
#
# It re-runs only when all of these hold, and says which one did not:
#   - the run is a push of a version tag, finished, and failed;
#   - the attempt that was triaged is the first, and still the latest: a re-run
#     is the one automatic retry, so a second failure is left to a person, and a
#     run someone already re-run is not re-run again;
#   - every failed job's rule names the action rerun-failed. A job the rules do
#     not know, or want a person to read, keeps the whole run from being
#     re-run, because re-running the failed jobs re-runs it too.
#
# Environment:
#   RUN_ID    the run (required)
#   ATTEMPT   the attempt that was triaged (required)
#   GH_REPO   owner/name of the repository (default: GITHUB_REPOSITORY)
#   GH_TOKEN  a token that can write Actions, for gh; not read on a dry run
#   RUN_URL   the run's URL, which the message links to (required)
#   DRY_RUN   true says what it would do and re-runs nothing (default: true)
#   NOTIFY    the notify command (required, as for triage-post.sh)
#   NOTIFY_CONFIG, STATE_DIR, NOTIFY_STATE_KEY, SLACK_BOT_TOKEN
#             as for triage-post.sh; the message is sent as a dry run unless
#             NOTIFY_DRY_RUN is false (default: true)

run="${RUN_ID:-}"
if [[ ! "${run}" =~ ^[0-9]+$ ]]; then
    echo "set RUN_ID to the run's number, got '${run}'" >&2
    exit 2
fi

attempt="${ATTEMPT:-}"
if [[ ! "${attempt}" =~ ^[0-9]+$ ]]; then
    echo "set ATTEMPT to the attempt that was triaged, got '${attempt}'" >&2
    exit 2
fi

repo="${GH_REPO:-${GITHUB_REPOSITORY:-}}"
if [[ -z "${repo}" ]]; then
    echo "set GH_REPO to owner/name" >&2
    exit 2
fi

if [[ -z "${RUN_URL:-}" ]]; then
    echo "set RUN_URL to the run's URL" >&2
    exit 2
fi

if [[ -z "${NOTIFY:-}" ]]; then
    echo "set NOTIFY to the notify command" >&2
    exit 2
fi

dry="${DRY_RUN:-true}"
notify_dry="${NOTIFY_DRY_RUN:-true}"
for v in "${dry}" "${notify_dry}"; do
    if [[ "${v}" != true && "${v}" != false ]]; then
        echo "DRY_RUN and NOTIFY_DRY_RUN must be true or false, got '${v}'" >&2
        exit 2
    fi
done

read -r -a notify <<<"${NOTIFY}"
state="${STATE_DIR:-.notify/cache}"
config="${NOTIFY_CONFIG:-release/notify.yml}"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

echo "## Automatic re-run of run ${run}"
echo

# decline says why nothing was re-run, and stops. It is not a failure: most
# failures are left to a person.
decline() {
    echo "Not re-run: $1"
    exit 0
}

cat >"${work}/rows.jsonl"

if [[ ! -s "${work}/rows.jsonl" ]]; then
    decline "no job of attempt ${attempt} failed."
fi

# The run itself, from the API and not from anything the caller says about it.
# The conclusion is last because a run that has not finished has none, and read
# collapses adjacent tabs, so an empty field anywhere else would shift the rest.
IFS=$'\t' read -r status event latest head conclusion < <(
    gh api "repos/${repo}/actions/runs/${run}" \
        --jq '[.status, .event, .run_attempt, .head_branch, .conclusion // ""] | @tsv'
)

if [[ "${event}" != push || ! "${head}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
    decline "the run is not a push of a version tag."
fi

if [[ "${status}" != completed || "${conclusion}" != failure ]]; then
    decline "the run is ${status}${conclusion:+ (${conclusion})}, not a finished failure."
fi

if [[ "${attempt}" != 1 ]]; then
    decline "attempt ${attempt} was triaged; the automatic re-run is for the first attempt only."
fi

if [[ "${latest}" != "${attempt}" ]]; then
    decline "the run is already at attempt ${latest}; someone re-ran it."
fi

# Every failed job must be one a re-run has fixed before. Only the class and the
# action are read from the rows, and the action only to compare it with one
# word; the evidence, which is log text, is never used.
if jq -es 'any(.[]; .action != "rerun-failed")' "${work}/rows.jsonl" >/dev/null; then
    echo "Not re-run: a failed job is not one a re-run has fixed before, and re-running the failed jobs re-runs it too."
    echo
    # A job's name comes from the workflow and may hold anything, so it goes
    # through jq to one line with no backtick.
    jq -r 'select(.action != "rerun-failed")
           | "- `\(.job | gsub("[`\r\n]"; " "))`: \(.class | gsub("[`\r\n]"; " ")), left to a person"' "${work}/rows.jsonl"
    exit 0
fi

classes="$(jq -rs '[.[].class] | unique | join(", ")' "${work}/rows.jsonl")"
next=$((attempt + 1))

if [[ "${dry}" == true ]]; then
    echo "Would re-run the failed jobs once (attempt ${next}): ${classes}. Dry run: nothing was re-run."
    exit 0
fi

gh api -X POST "repos/${repo}/actions/runs/${run}/rerun-failed-jobs" >/dev/null
echo "Re-ran the failed jobs once (attempt ${next}): ${classes}."

# The thread is told after the re-run, not before: a message about a re-run that
# did not start would be wrong. A message that cannot be sent does not undo it.
args=(trigger qualification:triage-rerun --config "${config}" --state-dir "${state}" --reply-only)
if [[ "${notify_dry}" == true ]]; then
    args+=(--dry-run)
fi

jq -nc --arg next "${next}" --arg classes "${classes}" --arg url "${RUN_URL}" \
    '{next_attempt: $next, classes: $classes, run_url: $url}' >"${work}/data.json"

"${notify[@]}" "${args[@]}" --data-file "${work}/data.json" ||
    echo "The thread could not be told; the re-run started." >&2
