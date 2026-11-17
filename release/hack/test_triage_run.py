# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise triage-run.sh with a stub gh and a stub triage, so nothing is fetched."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("triage-run.sh")

# gh api <path> [--jq ...]: the jobs listing prints what its jq would have
# printed, one tab-separated line per failed job, from $STUB_JOBS; a log is the
# file $STUB_LOGS/<job id>.log, and a job without one fails the way a missing
# artifact does.
STUB_GH = """#!/usr/bin/env bash
args="$*"
case "${args}" in
  *"/jobs?"*) cat "${STUB_JOBS}" ;;
  *"/jobs/"*"/logs"*)
    id="${args#*/jobs/}"
    id="${id%%/logs*}"
    cat "${STUB_LOGS}/${id}.log"
    ;;
  *) exit 1 ;;
esac
"""

# triage classify ...: prints the JSON in $STUB_RESULT whatever the log says,
# and fails the way an unreadable log does when the log says CLASSIFY-FAILS.
STUB_TRIAGE = """#!/usr/bin/env bash
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  if [[ "${args[i]}" == --log ]]; then log="${args[i + 1]}"; fi
done
if grep -q CLASSIFY-FAILS "${log}"; then
  echo "triage: reading the job log: bufio.Scanner: token too long" >&2
  exit 1
fi
cat "${STUB_RESULT}"
"""


class TriageRunTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.logs = self.root / "logs"
        self.logs.mkdir()
        self.script("gh", STUB_GH)
        self.script("triage", STUB_TRIAGE)
        self.jobs = self.root / "jobs.tsv"
        self.result = self.root / "result.json"
        self.result.write_text('{"class": "c", "action": "none", "evidence": "e", "note": "n"}')

    def script(self, name, body):
        path = self.root / name
        path.write_text(body)
        path.chmod(0o755)

    def run_script(self, *args, **extra):
        env = dict(
            os.environ,
            PATH=f"{self.root}:{os.environ['PATH']}",
            GH_REPO="o/r",
            TRIAGE=str(self.root / "triage"),
            STUB_JOBS=str(self.jobs),
            STUB_LOGS=str(self.logs),
            STUB_RESULT=str(self.result),
            **extra,
        )
        return subprocess.run([str(SCRIPT), *args], capture_output=True, text=True, env=env)

    def test_rejects_a_run_id_that_is_not_a_number(self):
        for bad in ["", "abc", "1; rm -rf /", "12 34", "$(id)"]:
            r = self.run_script(bad)
            self.assertEqual(r.returncode, 2, bad)
            self.assertEqual(r.stdout, "", bad)

    def test_a_run_with_no_failed_job_says_so(self):
        self.jobs.write_text("")
        r = self.run_script("1")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("No job of this run failed.", r.stdout)
        self.assertNotIn("| Job |", r.stdout)

    def test_a_failed_job_is_classified(self):
        self.jobs.write_text("11\t1\tRun the e2e suite\tforge-gb200-amd64\n")
        (self.logs / "11.log").write_text("some log\n")
        self.result.write_text(
            '{"class": "forge-capacity", "action": "wait-then-rerun", "evidence": "need 2, unusedUsable=1", "note": "n"}'
        )
        r = self.run_script("1")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("Diagnosis only: nothing was re-run, posted or deleted.", r.stdout)
        self.assertIn("| forge-gb200-amd64 | 1 | Run the e2e suite | forge-capacity | wait-then-rerun | `need 2, unusedUsable=1` |", r.stdout)

    def test_every_attempt_is_listed(self):
        self.jobs.write_text("11\t1\tPlan\tjob-a\n12\t2\tPlan\tjob-a\n")
        for i in ("11", "12"):
            (self.logs / f"{i}.log").write_text("log\n")
        r = self.run_script("1")
        rows = [line for line in r.stdout.splitlines() if line.startswith("| job-a")]
        self.assertEqual(len(rows), 2, r.stdout)

    def test_an_expired_log_is_listed_not_dropped(self):
        self.jobs.write_text("11\t1\tRun the e2e suite\tjob-a\n12\t1\tPlan\tjob-b\n")
        # job 11 has no log file: gh fails. Job 12's log is the expired-blob error.
        (self.logs / "12.log").write_text("<Error><Code>BlobNotFound</Code></Error>\n")
        r = self.run_script("1")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("| job-a | 1 | Run the e2e suite | log expired or unavailable | none | |", r.stdout)
        self.assertIn("| job-b | 1 | Plan | log expired or unavailable | none | |", r.stdout)

    def test_a_job_that_cannot_be_classified_does_not_stop_the_others(self):
        self.jobs.write_text("11\t1\tRun the e2e suite\tjob-a\n12\t1\tPlan\tjob-b\n")
        (self.logs / "11.log").write_text("CLASSIFY-FAILS\n")
        (self.logs / "12.log").write_text("fine\n")
        r = self.run_script("1")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("| job-a | 1 | Run the e2e suite | log expired or unavailable | none | |", r.stdout)
        self.assertIn("| job-b | 1 | Plan | c | none | `e` |", r.stdout)
        self.assertIn("could not classify job 11", r.stderr)

    def test_a_job_that_cannot_be_classified_is_not_posted_either(self):
        self.jobs.write_text("11\t1\tRun the e2e suite\tjob-a\n12\t1\tPlan\tjob-b\n")
        (self.logs / "11.log").write_text("CLASSIFY-FAILS\n")
        (self.logs / "12.log").write_text("fine\n")
        r = self.run_script("1", FORMAT="jsonl")
        self.assertEqual(r.returncode, 0, r.stderr)
        rows = [json.loads(line) for line in r.stdout.splitlines() if line.strip()]
        self.assertEqual([row["class"] for row in rows], ["log-unavailable", "c"])

    def test_log_text_cannot_break_the_table_or_inject_markup(self):
        self.jobs.write_text("11\t1\tRun the e2e suite\tjob-a\n")
        (self.logs / "11.log").write_text("log\n")
        self.result.write_text(
            '{"class": "c", "action": "none", '
            '"evidence": "a | b `code` <script>alert(1)</script>\\nsecond line", "note": "n"}'
        )
        r = self.run_script("1")
        row = next(line for line in r.stdout.splitlines() if line.startswith("| job-a"))
        # Six cells, whatever the evidence held.
        self.assertEqual(row.count(" | "), 5, row)
        self.assertNotIn("<", row)
        self.assertNotIn(">", row)
        self.assertEqual(row.count("`"), 2, row)
        self.assertNotIn("\n", row)


if __name__ == "__main__":
    unittest.main()
