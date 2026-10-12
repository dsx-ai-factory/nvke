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

REPO="${REPO:-dsx-ai-factory/nvke}"
RUNS_PER_WORKFLOW="${RUNS_PER_WORKFLOW:-30}"
# How many recent runs to read per view before grouping. Enough that an active
# workflow still reaches RUNS_PER_WORKFLOW cells.
FETCH_RUNS="${FETCH_RUNS:-1500}"
OUT="${OUT:-site/data.json}"

fail() { echo "generate.sh: $*" >&2; exit 1; }

command -v gh >/dev/null || fail "gh is required"
command -v jq >/dev/null || fail "jq is required"

page_size=100
# Runs are read one day at a time. A day with more runs than the listing can
# page through (event=push on a busy day) is split in half until it fits.
window=86400
cap=900

# fetch_window <query> <from> <to> -> the runs created in [from, to] (epoch seconds)
#
# Reads every page of the window and accepts it only when the distinct ids
# match its total_count; otherwise reads it again, three times, then fails.
fetch_window() {
  local query=$1 from=$2 to=$3 range page1 total pages page runs got
  range="$(jq -rn "$from|todate")..$(jq -rn "$to|todate")"
  for _ in 1 2 3; do
    page1=$(gh api "repos/${REPO}/actions/runs?per_page=${page_size}&page=1&${query}&created=${range}") || continue
    total=$(jq '.total_count' <<<"$page1") || continue
    if (( total > cap )); then
      fetch_window "$query" "$from" $(( (from + to) / 2 )) &&
        fetch_window "$query" $(( (from + to) / 2 + 1 )) "$to"
      return
    fi
    runs=$(jq -c '.workflow_runs[]' <<<"$page1")
    pages=$(( (total + page_size - 1) / page_size ))
    for ((page = 2; page <= pages; page++)); do
      runs+=$'\n'$(gh api "repos/${REPO}/actions/runs?per_page=${page_size}&page=${page}&${query}&created=${range}" \
        --jq '.workflow_runs[]') || continue 2
    done
    runs=$(jq -c -s 'unique_by(.id)[]' <<<"$runs")
    got=$(grep -c . <<<"$runs" || true)
    if (( got == total )); then
      [ -z "$runs" ] || printf '%s\n' "$runs"
      return 0
    fi
    echo "generate.sh: ${query} ${range}: ${got} distinct runs of ${total}; reading it again" >&2
  done
  return 1
}

# fetch_runs <query> -> newline-delimited run objects, at least FETCH_RUNS of
# them when the view has that many
#
# The listing is a search index, and with a filter it is not stable: the same
# page comes back from a different point in the history from one call to the
# next (on 2026-10-05, 17 of 31 reads of page 1 for branch=main started days
# or weeks in the past), and pages past the first thousand results are empty.
# Paging it by count therefore drops runs and repeats others, and the sort in
# to_rows can only order what was fetched. Reading it in date windows and
# checking each window's count is what makes the result complete.
#
# Errors are NOT swallowed. A 403 here would otherwise yield zero runs, leave
# the job green, and publish a blank grid over a good one. A failed job keeps
# the last deployment, which is the outcome we want.
# Returns non-zero rather than calling fail: an exit inside a command
# substitution only ends that subshell, and bash does not apply set -e to an
# assignment whose value came from one. Every caller checks the status.
fetch_runs() {
  local query=$1 now w got=0 out
  now=$(date -u +%s)
  # Sixty windows is the stop for a view that never reaches FETCH_RUNS.
  for ((w = 0; w < 60 && got < FETCH_RUNS; w++)); do
    out=$(fetch_window "$query" $(( now - (w + 1) * window + 1 )) $(( now - w * window ))) || return 1
    [ -z "$out" ] || printf '%s\n' "$out"
    got=$(( got + $(grep -c . <<<"$out" || true) ))
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

  # Through a file for the same reason as the final assembly below: a large
  # argv entry fails with "Argument list too long".
  local mapfile
  mapfile=$(mktemp)
  printf '%s' "$map" > "$mapfile"
  jq --slurpfile m "$mapfile" '
    ($m[0]) as $map
    | map(.runs |= map(
        . + (($map[(.id|tostring)] // []) | if length > 0 then {failed: .} else {} end)
      ))
  ' <<<"$rows"
  rm -f "$mapfile"
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
# This workflow runs on main, so its own run belongs in that view. A listing
# without a run created minutes ago is stale, however complete its windows are.
if [ -n "${GITHUB_RUN_ID:-}" ] && [ "${GITHUB_REF_NAME:-main}" = main ] &&
   ! jq -e --argjson id "$GITHUB_RUN_ID" 'any(.[].runs[]; .id == $id)' <<<"$main_rows" >/dev/null; then
  fail "run ${GITHUB_RUN_ID} is missing from the main view: the listing is stale, keeping the last deployment"
fi

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

# Through files, not --argjson. Linux caps one argv entry at 128 KiB
# (MAX_ARG_STRLEN), and the main view alone passes that at the default window,
# so passing these on the command line fails with "Argument list too long".
# --slurpfile reads a file of JSON values into an array, hence the [0].
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT
printf '%s' "$main_rows" > "${tmp}/main.json"
printf '%s' "$pr_rows" > "${tmp}/pr.json"
printf '%s' "$pr_mirror_rows" > "${tmp}/prt.json"

jq -n \
  --arg repo "$REPO" \
  --arg generated "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson keep "$RUNS_PER_WORKFLOW" \
  --slurpfile mainf "${tmp}/main.json" \
  --slurpfile prf "${tmp}/pr.json" \
  --slurpfile prtf "${tmp}/prt.json" \
  '{
     repo: $repo,
     generated: $generated,
     runs_per_workflow: $keep,
     views: {
       main: $mainf[0],
       pr: (($prf[0] + $prtf[0])
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
