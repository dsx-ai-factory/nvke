#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

# Says what the triage rules make of each failed job of a workflow run. It only
# reads: nothing is re-run, posted or deleted.
# usage: release/hack/triage-run.sh <RUN_ID>
#
# By default the jobs of every attempt are read, because a re-run that passed
# hides the attempt that failed. A job whose log has expired is listed as such.
#
# Environment:
#   TRIAGE   the built triage binary (default: triage on PATH)
#   RULES    the rules file (default: release/triage.yaml)
#   GH_REPO  owner/name of the repository (default: GITHUB_REPOSITORY)
#   GH_TOKEN a token that can read Actions, for gh
#   ATTEMPT  only the jobs of this attempt (default: every attempt)
#   FORMAT   markdown (default), a table for a run summary, or jsonl, one JSON
#            object per failed job with job, attempt, step, class, action,
#            evidence and note, for a caller that posts them

run="${1:-}"
if [[ ! "${run}" =~ ^[0-9]+$ ]]; then
    echo "usage: $0 <RUN_ID>   (a number, got '${run}')" >&2
    exit 2
fi

attempt="${ATTEMPT:-}"
if [[ -n "${attempt}" && ! "${attempt}" =~ ^[0-9]+$ ]]; then
    echo "ATTEMPT must be a number, got '${attempt}'" >&2
    exit 2
fi

format="${FORMAT:-markdown}"
if [[ "${format}" != markdown && "${format}" != jsonl ]]; then
    echo "FORMAT must be markdown or jsonl, got '${format}'" >&2
    exit 2
fi

triage="${TRIAGE:-triage}"
rules="${RULES:-release/triage.yaml}"
repo="${GH_REPO:-${GITHUB_REPOSITORY:-}}"
if [[ -z "${repo}" ]]; then
    echo "set GH_REPO to owner/name" >&2
    exit 2
fi

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# Only a number reaches the filter: it was checked above.
only="true"
if [[ -n "${attempt}" ]]; then
    only=".run_attempt == ${attempt}"
fi

# One line per failed job: id, attempt, the failed step, the job name. @tsv
# escapes tabs and newlines in a value, so a name cannot split a line.
gh api --paginate "repos/${repo}/actions/runs/${run}/jobs?filter=all&per_page=100" \
    --jq ".jobs[] | select(.conclusion == \"failure\" and (${only}))
          | [.id, .run_attempt, ([.steps[]? | select(.conclusion == \"failure\") | .name] | first // \"\"), .name]
          | @tsv" >"${work}/jobs.tsv"

if [[ "${format}" == markdown ]]; then
    echo "## Triage of run ${run}"
    echo
    echo "Diagnosis only: nothing was re-run, posted or deleted."
    echo

    if [[ ! -s "${work}/jobs.tsv" ]]; then
        echo "No job of this run failed."
        exit 0
    fi

    echo "| Job | Attempt | Failed step | Class | Action | Evidence |"
    echo "| --- | --- | --- | --- | --- | --- |"
fi

# cell makes a value safe inside a table cell: no pipe, newline or backtick, and
# no markup. Evidence comes from a log, which anyone who can push a branch can
# influence.
cell() {
    # -s reads the whole value as one string, so a newline inside it is
    # replaced and does not end the row.
    jq -Rrs 'gsub("[|`\r\n]"; " ") | gsub("[<>]"; "") | sub("\\s+$"; "")' <<<"$1"
}

while IFS=$'\t' read -r id attempt_of step name; do
    log="${work}/${id}.log"

    expired=false
    if ! gh api "repos/${repo}/actions/jobs/${id}/logs" --allow-escape-sequences >"${log}" 2>/dev/null \
        || grep -q "<Code>BlobNotFound</Code>" "${log}"; then
        expired=true
    fi

    if "${expired}"; then
        result='{"class": "log-unavailable", "action": "none", "evidence": "", "note": "The job log expired or could not be read."}'
    else
        result="$("${triage}" classify --rules "${rules}" --step "${step}" --log "${log}")"
    fi

    if [[ "${format}" == jsonl ]]; then
        # jq builds the object, so nothing in a value can alter its shape.
        jq -nc --arg job "${name}" --arg attempt "${attempt_of}" --arg step "${step}" --argjson r "${result}" \
            '{job: $job, attempt: $attempt, step: $step, class: $r.class, action: $r.action,
              evidence: (($r.evidence // "") | gsub("[`\r\n]"; " ")), note: $r.note}'

        continue
    fi

    if "${expired}"; then
        printf '| %s | %s | %s | log expired or unavailable | none | |\n' \
            "$(cell "${name}")" "${attempt_of}" "$(cell "${step}")"
        continue
    fi

    # The backticks are Markdown code, not a command substitution.
    # shellcheck disable=SC2016
    printf '| %s | %s | %s | %s | %s | `%s` |\n' \
        "$(cell "${name}")" "${attempt_of}" "$(cell "${step:-none}")" \
        "$(cell "$(jq -r .class <<<"${result}")")" \
        "$(cell "$(jq -r .action <<<"${result}")")" \
        "$(cell "$(jq -r '.evidence // ""' <<<"${result}")")"
done <"${work}/jobs.tsv"

if [[ "${format}" == markdown ]]; then
    echo
    echo "What each class means is in release/triage.yaml."
fi
