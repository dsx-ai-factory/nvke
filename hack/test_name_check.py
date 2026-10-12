# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise the banned-name scan against isolated repository fixtures."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("name-check.py")
# A hook can export GIT_DIR and friends, which would point git at the outer
# repository instead of the fixture.
ENV = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
CLEAN = "linked symlinked chunked blanket Winkernel encryptionKey beginKeywords nats-io/nkeys h1:9Q3nKEdv4abcdef+/=\n"


class NameCheckTest(unittest.TestCase):
    def run_check(self, files, allowlist="", mode="fail", extra=()):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(["git", "init", "--quiet", str(root)], check=True, capture_output=True, env=ENV)
            (root / "hack").mkdir()
            shutil.copyfile(SCRIPT, root / "hack" / SCRIPT.name)
            (root / "hack" / "name-check-allowlist.txt").write_text(allowlist, encoding="utf-8")
            for name, content in files.items():
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                if isinstance(content, bytes):
                    path.write_bytes(content)
                else:
                    path.write_text(content, encoding="utf-8")
            subprocess.run(["git", "-C", str(root), "add", "-A"], check=True, capture_output=True, env=ENV)
            return subprocess.run(
                ["python3", "-B", "hack/name-check.py", "--mode", mode, *extra],
                cwd=root, capture_output=True, text=True, check=False, env=ENV,
            )

    def test_product_token_fails(self):
        result = self.run_check({"fixture.txt": "apt-get install nke-agent\n"})
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("fixture.txt:1: nke-agent", result.stdout)
        self.assertIn("1 hits outside the allowlist", result.stderr)

    def test_english_words_camel_case_nkeys_and_base64_pass(self):
        result = self.run_check({"fixture.txt": CLEAN})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("name-check: clean", result.stdout)

    def test_product_shapes_are_kept(self):
        for token in ("NKECluster", "nkectl", "Nkectl", "nke.nvidia.com/reboot", "/etc/nke", "NKE_AGENT", "nkeRBACPrefix", "urn:nvidia:nke:controller"):
            with self.subTest(token=token):
                result = self.run_check({"fixture.txt": f"x {token} y\n"})
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn(token, result.stdout)

    def test_report_mode_exits_zero_with_hits(self):
        result = self.run_check({"fixture.txt": "nke-agent nke-agent\nnkectl\n"}, mode="report")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("hits: 3 in 1 files", result.stdout)
        self.assertIn("distinct tokens: 2", result.stdout)
        self.assertIn("2  nke-agent", result.stdout)
        self.assertIn("report only, exit 0", result.stdout)

    def test_allowlisted_token_passes(self):
        result = self.run_check({"fixture.txt": 'magic := "NKEAuth"\n'}, allowlist="token:^NKEAuth$\n")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("1 allowed by the allowlist", result.stdout)

    def test_allowlisted_path_passes_but_other_hits_still_fail(self):
        files = {"fixtures/a.txt": "nke-agent\n", "fixtures/nke-b.txt": "clean\n"}
        result = self.run_check(files, allowlist="path:fixtures/*\n")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        files["other.txt"] = "nke-agent\n"
        result = self.run_check(files, allowlist="path:fixtures/*\n")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("other.txt:1: nke-agent", result.stdout)
        self.assertNotIn("fixtures/", result.stdout.split("hits:", 1)[1].split("\n", 1)[1])

    def test_dead_allowlist_entry_fails_in_both_modes(self):
        for mode in ("fail", "report"):
            with self.subTest(mode=mode):
                result = self.run_check({"fixture.txt": "clean\n"}, allowlist="token:^never-nke-matches$\n", mode=mode)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn("DEAD", result.stderr)
                self.assertIn("token:^never-nke-matches$", result.stderr)

    def test_malformed_allowlist_is_a_usage_error(self):
        result = self.run_check({"fixture.txt": "clean\n"}, allowlist="nke-agent\n")
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn("expected 'path:<glob>' or 'token:<regex>'", result.stderr)

    def test_excluded_paths_are_not_scanned(self):
        files = {
            "go.sum": "github.com/x/nke-agent v1 h1:abc=\n",
            "vendor/x.txt": "nke-agent\n",
            "a/vendor/y.txt": "nke-agent\n",
            "a/generated/z.txt": "nke-agent\n",
            "CHANGELOG.md": "nke-agent\n",
            "poetry.lock": "nke-agent\n",
        }
        result = self.run_check(files)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        # The six fixtures plus the check's own script and allowlist, which the
        # fixture repository tracks too.
        self.assertIn(f"{len(files) + 2} excluded by path", result.stdout)

    def test_path_carrying_the_name_is_a_hit(self):
        result = self.run_check({"nke-thing/readme.txt": "clean\n"})
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("nke-thing/readme.txt (path): nke-thing", result.stdout)
        self.assertIn("1 paths carry the name", result.stdout)

    def test_binary_files_are_skipped(self):
        result = self.run_check({"blob.bin": b"\x00\x01nke-agent\x00"})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("1 binary skipped", result.stdout)

    def test_name_is_a_parameter(self):
        result = self.run_check({"fixture.txt": "foo-bar nke-agent\n"}, extra=("--name", "foo"))
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("fixture.txt:1: foo-bar", result.stdout)
        self.assertNotIn("nke-agent", result.stdout)


if __name__ == "__main__":
    unittest.main()
