#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Decide whether a pull request's mirror push runs an e2e lane.

WHY THIS EXISTS. copy-pr-bot mirrors a vetted pull request to
`pull-request/<n>`, and the e2e lanes start from that push. A mirror push also
carries the base branch's new commits, so a lane's `on.push.paths` match files
the pull request never changed: a docs-only pull request started CAPD, NICo,
Kubeadm and the bootstrap lane on self-hosted runners. The pull request's own
file list is the authority, so .github/workflows/e2e-pr-path-filter.yml asks
this script whether that list touches the lane.

ONE SOURCE OF TRUTH. `decide` reads the calling lane's own `on.push.paths`, the
same list GitHub applies to a push on `main`, and matches the pull request's
files against it with GitHub's filter-pattern rules: `*` stops at `/`, `**`
crosses it, `?` and `+` act on the character before them, and a `!` pattern
excludes what earlier patterns matched, in order. A lane never keeps a second,
hand-copied list that can drift from its triggers.

NEVER A LANE INPUT. Before matching, `decide` drops files that cannot change
what an e2e lane does: anything under a `docs/` directory, Markdown, and unit
tests (`*_test.go` and `testdata/`) outside an `e2e/` or `test/` directory,
which is where the e2e and integration suites keep theirs.

`check` is the repository rule `make lint-workflows` enforces: a workflow that
runs on mirror pushes, has push paths and uses a self-hosted runner gates every
job on the filter, and its push paths list every local action and reusable
workflow it reaches. A lane's push paths decide its pull request runs, so an
input missing from them is a change that never runs the lane.

Needs python3 and yq (preinstalled on GitHub-hosted Ubuntu runners).
"""

import argparse
import json
from pathlib import Path
import re
import subprocess
import sys

MIRROR_BRANCH = "pull-request/[0-9]+"
FILTER_WORKFLOW = "./.github/workflows/e2e-pr-path-filter.yml"
# The filter workflow runs this script, so a lane gated on it depends on both.
FILTER_SCRIPT = "hack/e2e-path-filter.py"
WORKFLOWS = Path(".github/workflows")

# A runner label that names the self-hosted pools: arc-* (RelEng EKS),
# linux-* (NV-managed) and the generic self-hosted label. GitHub-hosted labels
# are ubuntu-*, macos-* and windows-*.
SELF_HOSTED = re.compile(r"^(?:arc-|linux-|self-hosted$)", re.IGNORECASE)
# Status functions that let a job run after the job it needs was skipped.
STATUS_OVERRIDE = re.compile(r"\b(?:always|failure|cancelled)\s*\(")

DOCS = re.compile(r"(^|/)docs/|\.mdx?$")
UNIT_TEST = re.compile(r"_test\.go$|(^|/)testdata/")
SUITE_DIR = re.compile(r"(^|/)(e2e|test)/")


def is_lane_input(path):
    """Whether a changed file can change what an e2e lane does."""
    if DOCS.search(path):
        return False
    if UNIT_TEST.search(path) and not SUITE_DIR.search(path):
        return False
    return True


def glob_to_regex(pattern):
    """Translate one GitHub filter pattern (without a leading !) to a regex."""
    out, i = [], 0
    while i < len(pattern):
        if pattern.startswith("**/", i):
            out.append("(?:.*/)?")
            i += 3
        elif pattern.startswith("**", i):
            out.append(".*")
            i += 2
        elif pattern[i] == "*":
            out.append("[^/]*")
            i += 1
        elif pattern[i] in "?+":
            out.append(pattern[i])
            i += 1
        elif pattern[i] == "\\" and i + 1 < len(pattern):
            out.append(re.escape(pattern[i + 1]))
            i += 2
        elif pattern[i] == "[" and "]" in pattern[i + 1:]:
            end = pattern.index("]", i + 1)
            out.append(pattern[i:end + 1])
            i = end + 1
        else:
            out.append(re.escape(pattern[i]))
            i += 1
    return re.compile("^" + "".join(out) + "$")


def matches(patterns, path):
    """Whether GitHub's paths filter selects path: the last pattern that matches decides."""
    selected = False
    for pattern in patterns:
        negated = pattern.startswith("!")
        if glob_to_regex(pattern[1:] if negated else pattern).match(path):
            selected = not negated
    return selected


def lane_inputs(patterns, changed):
    """The changed files that are inputs of a lane with these push paths."""
    return [path for path in changed if is_lane_input(path) and matches(patterns, path)]


def load_workflow(path):
    out = subprocess.run(["yq", "-o=json", ".", str(path)], capture_output=True, text=True, check=True)
    return json.loads(out.stdout) or {}


def push_trigger(workflow):
    on = workflow.get("on") or {}
    return (on.get("push") or {}) if isinstance(on, dict) else {}


def decide(args):
    push = push_trigger(load_workflow(args.workflow))
    patterns = push.get("paths") or []
    changed = [line.strip() for line in Path(args.changed_files).read_text(encoding="utf-8").splitlines() if line.strip()]
    pr = f"pull request {args.pull_request}" if args.pull_request else "the pull request"
    if not patterns:
        print(f"{args.workflow} has no push paths: running it for {pr}.", file=sys.stderr)
        print("true")
        return 0
    if not changed:
        print(f"No file list for {pr}: running {args.workflow}.", file=sys.stderr)
        print("true")
        return 0
    hits = lane_inputs(patterns, changed)
    if hits:
        shown = ", ".join(hits[:5]) + (f" and {len(hits) - 5} more" if len(hits) > 5 else "")
        print(f"{pr} changes this lane's inputs ({shown}): running {args.workflow}.", file=sys.stderr)
        print("true")
    else:
        print(f"Skipping {args.workflow} for {pr}: none of its {len(changed)} changed files is an input of this lane.", file=sys.stderr)
        print("false")
    return 0


def needs_of(job):
    needs = job.get("needs") or []
    return [needs] if isinstance(needs, str) else list(needs)


def runner_labels(node, key=None):
    """Every value under a runs-on or runner key, steps excluded: runs-on
    scalars and lists, matrix runner lists and include entries, and the runner
    input a job passes to a called workflow."""
    if isinstance(node, dict):
        for k, v in node.items():
            if k != "steps":
                yield from runner_labels(v, k)
    elif isinstance(node, list):
        for v in node:
            yield from runner_labels(v, key)
    elif isinstance(node, str) and key in ("runs-on", "runner"):
        yield node


def step_uses(doc):
    """The uses: values of a parsed workflow (job-level and step-level) or of a
    composite action's steps. Text inside run: scripts and with: inputs is data,
    so it is never read as a reference."""
    refs = []
    for job in (doc.get("jobs") or {}).values():
        if isinstance(job, dict):
            refs.append(job.get("uses"))
            refs += [step.get("uses") for step in job.get("steps") or [] if isinstance(step, dict)]
    runs = doc.get("runs")
    if isinstance(runs, dict):
        refs += [step.get("uses") for step in runs.get("steps") or [] if isinstance(step, dict)]
    return [ref for ref in refs if isinstance(ref, str) and ref.startswith("./")]


def local_uses(root, relpath, seen):
    """The local actions and reusable workflows a workflow or action reaches,
    directly or through another one: (reference, the files it consists of)."""
    if relpath in seen or not (root / relpath).is_file():
        return
    seen.add(relpath)
    for ref in step_uses(load_workflow(root / relpath)):
        ref = ref[2:].split("@")[0].rstrip("/")
        if ref.endswith((".yml", ".yaml")):
            yield ref, [ref, FILTER_SCRIPT] if f"./{ref}" == FILTER_WORKFLOW else [ref]
            yield from local_uses(root, ref, seen)
            continue
        metadata = next((f"{ref}/{n}" for n in ("action.yml", "action.yaml") if (root / ref / n).is_file()), f"{ref}/action.yml")
        # An action can run its own scripts, so every file in it is an input.
        files = sorted(p.relative_to(root).as_posix() for p in (root / ref).rglob("*") if p.is_file()) if (root / ref).is_dir() else []
        yield ref, files or [metadata]
        yield from local_uses(root, metadata, seen)


def check(args):
    problems = []
    workflows = Path(args.workflows)
    root = workflows.resolve().parent.parent
    for path in sorted(workflows.glob("*.y*ml")):
        workflow = load_workflow(path)
        push = push_trigger(workflow)
        if MIRROR_BRANCH not in (push.get("branches") or []) or not push.get("paths"):
            continue
        jobs = workflow.get("jobs") or {}
        if not any(SELF_HOSTED.match(label) for label in runner_labels(jobs)):
            continue
        gates = [name for name, job in jobs.items() if job.get("uses") == FILTER_WORKFLOW]
        if not gates:
            problems.append(f"{path}: runs on mirror pushes with push paths and a self-hosted runner, but no job uses {FILTER_WORKFLOW}.")
            continue
        if len(gates) > 1:
            problems.append(f"{path}: jobs {', '.join(sorted(gates))} all use {FILTER_WORKFLOW}; one filter job per lane is supported.")
            continue
        gate = gates[0]

        def reaches_gate(name, seen=()):
            return any(n == gate or (n not in seen and reaches_gate(n, seen + (name,))) for n in needs_of(jobs.get(name, {})))

        gated = re.compile(rf"needs\.{re.escape(gate)}\.outputs\.run_e2e\s*==\s*'true'")
        for name, job in jobs.items():
            if name == gate:
                continue
            condition = str(job.get("if", ""))
            if not reaches_gate(name):
                problems.append(f"{path}: job {name} does not need {gate}, so it runs whatever the pull request changed.")
            elif gate in needs_of(job) and not gated.search(condition):
                problems.append(f"{path}: job {name} needs {gate} but its if: does not test needs.{gate}.outputs.run_e2e == 'true'.")
            elif STATUS_OVERRIDE.search(condition):
                problems.append(f"{path}: job {name} can run after a skipped job ({condition.strip()}), so the filter cannot stop it.")

        patterns = push["paths"]
        missing = {}
        for ref, files in local_uses(root, path.resolve().relative_to(root).as_posix(), set()):
            unlisted = [f for f in files if not matches(patterns, f)]
            if unlisted:
                missing[ref] = unlisted
        for ref, unlisted in sorted(missing.items()):
            problems.append(f"{path}: uses ./{ref}, but its push paths do not list {', '.join(unlisted)}, so a pull request that changes only that skips this lane.")
    for problem in problems:
        print(f"ERROR: {problem}", file=sys.stderr)
    return 1 if problems else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)
    d = sub.add_parser("decide", help="Print true when the pull request changes an input of the lane, else false.")
    d.add_argument("--workflow", required=True, help="The calling lane's workflow file.")
    d.add_argument("--changed-files", required=True, help="File with the pull request's changed paths, one per line.")
    d.add_argument("--pull-request", default="", help="Pull request number, for the log line.")
    d.set_defaults(func=decide)
    c = sub.add_parser("check", help="Fail when a self-hosted mirror lane with push paths is not gated on the filter.")
    c.add_argument("--workflows", default=str(WORKFLOWS))
    c.set_defaults(func=check)
    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
