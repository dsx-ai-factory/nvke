# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise triage-rerun.sh with a stub gh and a stub notify, so nothing is re-run or sent."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("triage-rerun.sh")

# gh api <run>: runs the script's own --jq over the run in $STUB_RUN, as gh
# would. gh api -X POST <run>/rerun-failed-jobs: records the call, and fails when
# $STUB_POST_FAILS is set. Anything else fails.
STUB_GH = """#!/usr/bin/env bash
args="$*"
case "${args}" in
  *"-X POST"*"/rerun-failed-jobs"*)
    if [[ -n "${STUB_POST_FAILS:-}" ]]; then exit 1; fi
    printf '%s\\n' "${args}" >>"${STUB_CALLS}"
    ;;
  *"/actions/runs/"*)
    argv=("$@")
    for ((i = 0; i < ${#argv[@]}; i++)); do
      if [[ "${argv[i]}" == --jq ]]; then query="${argv[i + 1]}"; fi
    done
    jq -r "${query}" "${STUB_RUN}"
    ;;
  *) exit 1 ;;
esac
"""

# notify trigger ...: records its arguments and the data file it was given.
STUB_NOTIFY = """#!/usr/bin/env bash
data=""
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  if [[ "${args[i]}" == --data-file ]]; then data="${args[i + 1]}"; fi
done
printf '%s\\n' "$*" >>"${STUB_NOTIFY_CALLS}"
cat "${data}" >>"${STUB_NOTIFY_DATA}"
printf '\\n' >>"${STUB_NOTIFY_DATA}"
if [[ -n "${STUB_NOTIFY_FAILS:-}" ]]; then exit 1; fi
"""


def row(job="forge-gb200-amd64", cls="tenant-control-plane-join", action="rerun-failed", **extra):
    r = {
        "job": job,
        "attempt": "1",
        "step": "Run the e2e suite",
        "class": cls,
        "action": action,
        "evidence": "has not progressed",
        "note": "n",
    }
    r.update(extra)
    return json.dumps(r)


def run_json(**extra):
    r = {
        "status": "completed",
        "conclusion": "failure",
        "event": "push",
        "run_attempt": 1,
        "head_branch": "v0.24.0-beta.14",
    }
    r.update(extra)
    return json.dumps(r)


class TriageRerunTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)

        for name, body in (("gh", STUB_GH), ("notify", STUB_NOTIFY)):
            f = self.root / name
            f.write_text(body)
            f.chmod(0o755)

        self.run_file = self.root / "run.json"
        self.run_file.write_text(run_json())
        self.calls = self.root / "calls"
        self.notify_calls = self.root / "notify-calls"
        self.notify_data = self.root / "notify-data"

    def run_script(self, stdin, **env):
        full = dict(
            os.environ,
            PATH=f"{self.root}:{os.environ['PATH']}",
            GH_REPO="o/r",
            RUN_ID="42",
            ATTEMPT="1",
            RUN_URL="https://example.com/run/42",
            NOTIFY=str(self.root / "notify"),
            STATE_DIR=str(self.root / "state"),
            STUB_RUN=str(self.run_file),
            STUB_CALLS=str(self.calls),
            STUB_NOTIFY_CALLS=str(self.notify_calls),
            STUB_NOTIFY_DATA=str(self.notify_data),
        )
        full.update(env)
        return subprocess.run([str(SCRIPT)], input=stdin, capture_output=True, text=True, env=full)

    def rerun_calls(self):
        return self.calls.read_text().splitlines() if self.calls.exists() else []

    def told(self):
        if not self.notify_calls.exists():
            return [], []
        data = [json.loads(line) for line in self.notify_data.read_text().splitlines() if line.strip()]
        return self.notify_calls.read_text().splitlines(), data

    def test_it_needs_to_know_the_run_the_attempt_the_url_and_notify(self):
        for bad in ({"RUN_ID": "abc"}, {"RUN_ID": ""}, {"ATTEMPT": "x"}, {"RUN_URL": ""}, {"NOTIFY": ""}):
            r = self.run_script(row() + "\n", **bad)
            self.assertEqual(r.returncode, 2, bad)
            self.assertEqual(self.rerun_calls(), [], bad)

    def test_it_rejects_a_bad_dry_run(self):
        self.assertEqual(self.run_script(row() + "\n", DRY_RUN="maybe").returncode, 2)
        self.assertEqual(self.run_script(row() + "\n", NOTIFY_DRY_RUN="maybe").returncode, 2)

    def test_a_dry_run_says_what_it_would_do_and_does_nothing(self):
        r = self.run_script(row() + "\n")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("Would re-run the failed jobs once (attempt 2): tenant-control-plane-join", r.stdout)
        self.assertEqual(self.rerun_calls(), [])
        self.assertEqual(self.told(), ([], []))

    def test_it_re_runs_the_failed_jobs_once_and_tells_the_thread(self):
        r = self.run_script(row() + "\n" + row(job="b", cls="forge-node-unreachable") + "\n", DRY_RUN="false")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("Re-ran the failed jobs once (attempt 2): forge-node-unreachable, tenant-control-plane-join", r.stdout)
        calls = self.rerun_calls()
        self.assertEqual(len(calls), 1)
        self.assertIn("repos/o/r/actions/runs/42/rerun-failed-jobs", calls[0])

        notify_calls, data = self.told()
        self.assertIn("trigger qualification:triage-rerun", notify_calls[0])
        self.assertIn("--reply-only", notify_calls[0])
        self.assertIn("--dry-run", notify_calls[0])
        self.assertEqual(
            data[0],
            {
                "next_attempt": "2",
                "classes": "forge-node-unreachable, tenant-control-plane-join",
                "run_url": "https://example.com/run/42",
            },
        )

    def test_the_message_is_sent_for_real_only_when_told_to(self):
        self.run_script(row() + "\n", DRY_RUN="false", NOTIFY_DRY_RUN="false")
        notify_calls, _ = self.told()
        self.assertNotIn("--dry-run", notify_calls[0])
        self.assertIn("--reply-only", notify_calls[0])

    def test_one_job_a_person_should_read_keeps_the_whole_run_from_being_re_run(self):
        for blocker in (row(job="b", cls="unknown", action="none"), row(job="b", cls="log-unavailable", action="none"),
                        row(job="b", cls="os-image-not-published", action="rerun-image-build"),
                        row(job="b", cls="forge-capacity", action="wait-then-rerun")):
            r = self.run_script(row() + "\n" + blocker + "\n", DRY_RUN="false")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("Not re-run: a failed job is not one a re-run has fixed before", r.stdout)
            self.assertIn("`b`", r.stdout)
            self.assertEqual(self.rerun_calls(), [], blocker)

    def test_a_job_name_cannot_break_the_summary(self):
        r = self.run_script(row(job="x`\n## injected", cls="unknown", action="none") + "\n", DRY_RUN="false")
        self.assertIn("- `x  ## injected`: unknown", r.stdout)
        self.assertNotIn("\n## injected", r.stdout)

    def test_log_text_in_the_evidence_is_never_used(self):
        nasty = 'x", "action": "rerun-failed", "y": "\n$(id)'
        r = self.run_script(row(cls="unknown", action="none", evidence=nasty) + "\n", DRY_RUN="false")
        self.assertIn("Not re-run", r.stdout)
        self.assertEqual(self.rerun_calls(), [])

    def test_no_failed_job_re_runs_nothing(self):
        r = self.run_script("", DRY_RUN="false")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("Not re-run: no job of attempt 1 failed.", r.stdout)
        self.assertEqual(self.rerun_calls(), [])

    def test_a_second_attempt_is_left_to_a_person(self):
        self.run_file.write_text(run_json(run_attempt=2))
        r = self.run_script(row(attempt="2") + "\n", DRY_RUN="false", ATTEMPT="2")
        self.assertIn("first attempt only", r.stdout)
        self.assertEqual(self.rerun_calls(), [])

    def test_a_run_someone_already_re_ran_is_not_re_run_again(self):
        self.run_file.write_text(run_json(run_attempt=2))
        r = self.run_script(row() + "\n", DRY_RUN="false")
        self.assertIn("already at attempt 2", r.stdout)
        self.assertEqual(self.rerun_calls(), [])

    def test_only_a_finished_failure_of_a_tag_push_is_re_run(self):
        # Each case also says why, so a wrong reason fails the test: a run that
        # has not finished has no conclusion, which once shifted the fields.
        cases = {
            "not a push": (run_json(event="pull_request"), "not a push of a version tag"),
            "not a tag": (run_json(head_branch="main"), "not a push of a version tag"),
            "a branch named like a flag": (run_json(head_branch="--force"), "not a push of a version tag"),
            "still running": (run_json(status="in_progress", conclusion=None), "is in_progress, not a finished failure"),
            "queued": (run_json(status="queued", conclusion=None), "is queued, not a finished failure"),
            "cancelled": (run_json(conclusion="cancelled"), "is completed (cancelled), not a finished failure"),
            "passed": (run_json(conclusion="success"), "is completed (success), not a finished failure"),
        }
        for name, (body, why) in cases.items():
            self.run_file.write_text(body)
            r = self.run_script(row() + "\n", DRY_RUN="false")
            self.assertEqual(r.returncode, 0, f"{name}: {r.stderr}")
            self.assertIn("Not re-run", r.stdout, name)
            self.assertIn(why, r.stdout, name)
            self.assertEqual(self.rerun_calls(), [], name)

    def test_a_re_run_that_cannot_be_started_fails_loudly_and_tells_no_one(self):
        r = self.run_script(row() + "\n", DRY_RUN="false", STUB_POST_FAILS="1")
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(self.told(), ([], []))

    def test_a_message_that_cannot_be_sent_does_not_undo_the_re_run(self):
        r = self.run_script(row() + "\n", DRY_RUN="false", STUB_NOTIFY_FAILS="1")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(len(self.rerun_calls()), 1)
        self.assertIn("the re-run started", r.stderr)


if __name__ == "__main__":
    unittest.main()
