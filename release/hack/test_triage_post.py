# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise triage-post.sh with a stub notify, so nothing is sent."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("triage-post.sh")

# notify trigger ...: records its arguments and the data file it was given.
STUB_NOTIFY = """#!/usr/bin/env bash
data=""
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  if [[ "${args[i]}" == --data-file ]]; then data="${args[i + 1]}"; fi
done
printf '%s\\n' "$*" >>"${STUB_CALLS}"
cat "${data}" >>"${STUB_DATA}"
printf '\\n' >>"${STUB_DATA}"
"""


def row(job="forge-gb200-amd64", cls="tenant-control-plane-join", **extra):
    r = {
        "job": job,
        "attempt": "1",
        "step": "Run the e2e suite",
        "class": cls,
        "action": "rerun-failed",
        "evidence": "has not progressed",
        "note": "n",
    }
    r.update(extra)
    return json.dumps(r)


class TriagePostTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        stub = self.root / "notify"
        stub.write_text(STUB_NOTIFY)
        stub.chmod(0o755)
        self.calls = self.root / "calls"
        self.data = self.root / "data"

    def run_script(self, stdin, **env):
        full = dict(
            os.environ,
            RUN_URL="https://example.com/run/1",
            NOTIFY=str(self.root / "notify"),
            STATE_DIR=str(self.root / "state"),
            STUB_CALLS=str(self.calls),
            STUB_DATA=str(self.data),
        )
        full.update(env)
        return subprocess.run([str(SCRIPT)], input=stdin, capture_output=True, text=True, env=full)

    def sent(self):
        if not self.calls.exists():
            return [], []
        calls = self.calls.read_text().splitlines()
        data = [json.loads(line) for line in self.data.read_text().splitlines() if line.strip()]
        return calls, data

    def test_requires_the_failed_runs_url(self):
        r = subprocess.run([str(SCRIPT)], input="", capture_output=True, text=True, env={**os.environ, "RUN_URL": ""})
        self.assertEqual(r.returncode, 2)

    def test_it_will_not_guess_which_notify_to_run(self):
        env = {k: v for k, v in os.environ.items() if k != "NOTIFY"}
        r = subprocess.run([str(SCRIPT)], input="", capture_output=True, text=True, env={**env, "RUN_URL": "https://example.com/run/1"})
        self.assertEqual(r.returncode, 2)
        self.assertIn("NOTIFY", r.stderr)

    def test_rejects_a_bad_dry_run_or_max(self):
        self.assertEqual(self.run_script("", DRY_RUN="maybe").returncode, 2)
        self.assertEqual(self.run_script("", MAX="lots").returncode, 2)

    def test_it_only_replies_and_is_a_dry_run_unless_told_otherwise(self):
        r = self.run_script(row() + "\n")
        self.assertEqual(r.returncode, 0, r.stderr)
        calls, _ = self.sent()
        self.assertEqual(len(calls), 1)
        self.assertIn("trigger qualification:triage", calls[0])
        self.assertIn("--reply-only", calls[0])
        self.assertIn("--dry-run", calls[0])

    def test_posting_for_real_drops_the_dry_run_but_keeps_reply_only(self):
        self.run_script(row() + "\n", DRY_RUN="false")
        calls, _ = self.sent()
        self.assertNotIn("--dry-run", calls[0])
        self.assertIn("--reply-only", calls[0])

    def test_the_data_carries_what_the_template_reads(self):
        self.run_script(row() + "\n")
        _, data = self.sent()
        self.assertEqual(
            data[0],
            {
                "stream": "forge-gb200-amd64",
                "step": "Run the e2e suite",
                "attempt": "1",
                "class": "tenant-control-plane-join",
                "action": "rerun-failed",
                "note": "n",
                "evidence": "has not progressed",
                "run_url": "https://example.com/run/1",
            },
        )

    def test_a_job_whose_log_is_gone_is_not_posted(self):
        self.run_script(row(cls="log-unavailable") + "\n" + row(job="b") + "\n")
        _, data = self.sent()
        self.assertEqual([d["stream"] for d in data], ["b"])

    def test_it_posts_at_most_max_and_says_what_it_left(self):
        rows = "".join(row(job=f"job-{i}") + "\n" for i in range(6))
        r = self.run_script(rows, MAX="2")
        _, data = self.sent()
        self.assertEqual([d["stream"] for d in data], ["job-0", "job-1"])
        self.assertIn("4 more failed job(s) not posted", r.stderr)

    def test_a_value_cannot_change_the_shape_of_the_data(self):
        nasty = 'x", "run_url": "https://evil.example", "y": "\n$(id) `id`'
        self.run_script(row(evidence=nasty) + "\n")
        _, data = self.sent()
        self.assertEqual(data[0]["run_url"], "https://example.com/run/1")
        self.assertNotIn("y", data[0])
        self.assertEqual(data[0]["evidence"], nasty)

    def test_no_failed_job_sends_nothing(self):
        r = self.run_script("")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.sent(), ([], []))


if __name__ == "__main__":
    unittest.main()
