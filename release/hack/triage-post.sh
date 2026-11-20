#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

# Posts what the triage rules make of a run's failed jobs as replies in the
# run's Slack thread. It reads the one-object-per-line output of
# `FORMAT=jsonl release/hack/triage-run.sh` on standard input, and sends each as
# a qualification:triage event through the notify tool.
# usage: FORMAT=jsonl release/hack/triage-run.sh <RUN_ID> | release/hack/triage-post.sh
#
# It only ever replies. A run whose thread was never saved, or has expired,
# posts nothing, because it cannot start a thread of its own.
#
# Environment:
#   RUN_URL     the failed run's URL, which the message links to (required)
#   STATE_DIR   where the saved thread is (default: .notify/cache)
#   NOTIFY      the notify command, such as a built binary or
#               "go run ./release/cmd/notify" (required: a repository that
#               borrows this script must say which notify it means)
#   NOTIFY_CONFIG the notify config (default: release/notify.yml)
#   DRY_RUN     true prints what would be sent and needs no token (default: true)
#   MAX         the most findings to post per run (default: 4)
#   SLACK_BOT_TOKEN the bot token, read by notify; not needed for a dry run
#   NOTIFY_STATE_KEY the conversation, which for a tag is the tag (read by notify)

if [[ -z "${RUN_URL:-}" ]]; then
    echo "set RUN_URL to the failed run's URL" >&2
    exit 2
fi

dry="${DRY_RUN:-true}"
if [[ "${dry}" != true && "${dry}" != false ]]; then
    echo "DRY_RUN must be true or false, got '${dry}'" >&2
    exit 2
fi

max="${MAX:-4}"
if [[ ! "${max}" =~ ^[0-9]+$ ]]; then
    echo "MAX must be a number, got '${max}'" >&2
    exit 2
fi

if [[ -z "${NOTIFY:-}" ]]; then
    echo "set NOTIFY to the notify command" >&2
    exit 2
fi

read -r -a notify <<<"${NOTIFY}"
state="${STATE_DIR:-.notify/cache}"
config="${NOTIFY_CONFIG:-release/notify.yml}"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

args=(trigger qualification:triage --config "${config}" --state-dir "${state}" --reply-only)
if [[ "${dry}" == true ]]; then
    args+=(--dry-run)
fi

posted=0
skipped=0

while IFS= read -r row; do
    [[ -n "${row}" ]] || continue

    # Nothing helpful to say about a log that is gone.
    if [[ "$(jq -r .class <<<"${row}")" == log-unavailable ]]; then
        continue
    fi

    if ((posted >= max)); then
        skipped=$((skipped + 1))
        continue
    fi

    # The data file is built by jq from the row, so nothing in a value can
    # change its shape. The stream is the job's name.
    jq -c --arg url "${RUN_URL}" \
        '{stream: .job, step: .step, attempt: .attempt, class: .class, action: .action,
          note: .note, evidence: .evidence, run_url: $url}' <<<"${row}" >"${work}/data.json"

    "${notify[@]}" "${args[@]}" --data-file "${work}/data.json"
    posted=$((posted + 1))
done

if ((skipped > 0)); then
    echo "triage-post: ${skipped} more failed job(s) not posted; the run summary lists them" >&2
fi
