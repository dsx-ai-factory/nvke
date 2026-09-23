#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Writes the CI dashboard's data.json: one row per workflow, one cell per run.
#
# The window is a run count, not a time span. A lane on a twice-daily cron would
# show two cells in 24 hours, which tells you nothing; RUNS_PER_WORKFLOW cells
# show it over whatever span that lane takes to produce them.
#
# Jobs are fetched only for runs that did not succeed. A successful run has no
# failed job by definition, and non-success is about 3% of runs on main, so this
# keeps a generation under a hundred API calls instead of a few thousand.

set -euo pipefail

REPO="${REPO:-dsx-ai-factory/nke}"
RUNS_PER_WORKFLOW="${RUNS_PER_WORKFLOW:-30}"
# How many recent runs to read per view before grouping. Enough that an active
# workflow still reaches RUNS_PER_WORKFLOW cells.
FETCH_RUNS="${FETCH_RUNS:-1500}"
OUT="${OUT:-site/data.json}"

fail() { echo "generate.sh: $*" >&2; exit 1; }

command -v gh >/dev/null || fail "gh is required"
command -v jq >/dev/null || fail "jq is required"

page_size=100
pages=$(( (FETCH_RUNS + page_size - 1) / page_size ))

# fetch_runs <query> -> newline-delimited run objects, newest first
#
# Errors are NOT swallowed. A 403 here would otherwise yield zero runs, leave
# the job green, and publish a blank grid over a good one. A failed job keeps
# the last deployment, which is the outcome we want.
# Returns non-zero rather than calling fail: an exit inside a command
# substitution only ends that subshell, and bash does not apply set -e to an
# assignment whose value came from one. Every caller checks the status.
fetch_runs() {
  local query=$1 page
  for ((page = 1; page <= pages; page++)); do
    gh api "repos/${REPO}/actions/runs?per_page=${page_size}&page=${page}&${query}" \
      --jq '.workflow_runs[]' || return 1
  done
}

# Group into rows of at most RUNS_PER_WORKFLOW runs, newest first. The name is
# the row key rather than the workflow id, because a renamed workflow keeps its
# id and two rows titled the same would be unreadable.
to_rows() {
  jq -s --argjson keep "$RUNS_PER_WORKFLOW" --arg repo "$REPO" '
    # Per-run keys are short and only what the page cannot derive. The run URL
    # is built from the repo and the id in the browser rather than repeated on
    # every cell.
    map({
      _wf:  .name,
      _wid: .workflow_id,
      id:   .id,
      c:    (.conclusion // .status),
      sha:  (.head_sha[0:7]),
      t:    .created_at,
      # Seconds spent queued before the run started. This repo serialises runs
      # per commit through a concurrency group, so a run can wait an hour with
      # nothing wrong; without this the wait is invisible.
      q:    (((.run_started_at // .created_at) | fromdateiso8601)
             - (.created_at | fromdateiso8601)),
      d:    ((.updated_at | fromdateiso8601)
             - ((.run_started_at // .created_at) | fromdateiso8601)),
      ev:   .event,
      br:   .head_branch,
      # The commit subject is already in this payload, and it is usually the
      # answer to "what broke it", so it costs nothing to carry.
      msg:  ((.head_commit.message // "") | split("\n")[0] | .[0:120]),
      actor: (.actor.login // null)
    }
    # copy-pr-bot mirrors a pull request onto pull-request/<number> and most
    # checks run as a push to that branch, so the number is in the branch name
    # when the runs payload has no pull_requests entry.
    #
    # Written as an if rather than with capture and //: capture emits NOTHING
    # when it does not match, and `a // b` with an empty b is also empty, which
    # deletes the whole run object instead of leaving the key off.
    + (if (.pull_requests[0].number // null) != null then
         {pr: .pull_requests[0].number}
       elif ((.head_branch // "") | test("^pull-request/[0-9]+$")) then
         {pr: ((.head_branch | ltrimstr("pull-request/")) | tonumber)}
       else {} end))
    | group_by(._wf)
    | map({
        workflow: .[0]._wf,
        workflow_url: "https://github.com/\($repo)/actions/workflows/\(.[0]._wid)",
        runs: (sort_by(.t) | reverse | .[0:$keep] | map(del(._wf, ._wid)))
      })
    | sort_by(.workflow)
  '
}

# Annotate non-success cells with the jobs that actually failed, so a red square
# says which leg broke without opening the run.
annotate_failures() {
  local rows=$1 ids id jobs
  ids=$(jq -r '.[].runs[] | select(.c != "success" and .c != null) | .id' <<<"$rows" | sort -u)
  [ -z "$ids" ] && { printf '%s' "$rows"; return; }

  local map='{}'
  while read -r id; do
    [ -n "$id" ] || continue
    jobs=$(gh api "repos/${REPO}/actions/runs/${id}/jobs?per_page=100" \
      --jq '[.jobs[] | select(.conclusion != null and .conclusion != "success"
                              and .conclusion != "skipped"
                              and .conclusion != "cancelled") | .name]' \
      2>/dev/null || true)
    # gh can exit 0 and print nothing, which --argjson rejects.
    [ -n "$jobs" ] || jobs='[]'
    map=$(jq --arg id "$id" --argjson jobs "$jobs" '. + {($id): $jobs}' <<<"$map")
  done <<<"$ids"

  jq --argjson map "$map" '
    map(.runs |= map(
      . + (($map[(.id|tostring)] // []) | if length > 0 then {failed: .} else {} end)
    ))
  ' <<<"$rows"
}

# build_view <query> [jq-select] -> rows, annotated
#
# The fetch is its own statement rather than the head of a pipeline. A `fail`
# inside a pipeline only exits that subshell, and set -e does not reliably carry
# it out through a command substitution in a function, so a 404 used to print
# its message and then publish an empty grid anyway.
build_view() {
  local query=$1 keep=${2:-.} raw rows
  if ! raw=$(fetch_runs "$query"); then
    echo "generate.sh: reading runs failed (${query}); refusing to publish a partial dashboard" >&2
    return 1
  fi
  rows=$(printf '%s\n' "$raw" | jq -c "select(${keep})" | to_rows)
  # An empty view is legitimate (a repo with no PR runs yet), but an empty
  # string is not JSON and --argjson would reject it below.
  [ -n "$rows" ] || rows='[]'
  annotate_failures "$rows"
}

echo "generate.sh: reading up to ${FETCH_RUNS} runs per view from ${REPO}" >&2

# Everything that ran on main: the scheduled lanes and the post-merge runs.
if ! main_rows=$(build_view "branch=main"); then fail "could not build the main view"; fi

# Pull request runs come in two shapes here, and the mirror is the bigger one.
# copy-pr-bot pushes a pull request onto pull-request/<number> because the
# self-hosted runners do not pick up the pull_request event, so most checks --
# every e2e lane, CodeQL, the image build, the notices gate -- run as a push to
# that branch and appear under neither `branch=main` nor `event=pull_request`.
# It was 101 of the last 300 runs, the single largest shape.
if ! pr_rows=$(build_view "event=pull_request"); then fail "could not build the pull_request view"; fi
if ! pr_mirror_rows=$(build_view "event=push" '(.head_branch // "") | startswith("pull-request/")'); then
  fail "could not build the mirror-branch view"
fi

mkdir -p "$(dirname "$OUT")"
jq -n \
  --arg repo "$REPO" \
  --arg generated "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson keep "$RUNS_PER_WORKFLOW" \
  --argjson main "$main_rows" \
  --argjson pr "$pr_rows" \
  --argjson prt "$pr_mirror_rows" \
  '{
     repo: $repo,
     generated: $generated,
     runs_per_workflow: $keep,
     views: {
       main: $main,
       pr: (($pr + $prt)
            | group_by(.workflow)
            | map({
                workflow: .[0].workflow,
                workflow_url: .[0].workflow_url,
                # sort_by(.t), not .created: the per-run key is t. A wrong key
                # makes every element null, the stable sort keeps input order,
                # and reverse then hands back the oldest runs.
                runs: (map(.runs) | add | sort_by(.t) | reverse | .[0:$keep])
              })
            | sort_by(.workflow))
     }
   }' > "$OUT"

echo "generate.sh: wrote $OUT ($(wc -c <"$OUT" | tr -d ' ') bytes)" >&2
echo "  main rows: $(jq '.views.main | length' <"$OUT")" >&2
echo "  pr rows:   $(jq '.views.pr | length' <"$OUT")" >&2
