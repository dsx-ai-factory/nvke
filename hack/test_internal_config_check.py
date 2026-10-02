# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise internal identifier detection against isolated repository fixtures."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("internal-config-check.sh")
ORG_ID = "12345678" + "90123456"
# A hook can export GIT_DIR and friends, which would point git at the outer
# repository instead of the fixture.
ENV = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}


class InternalConfigCheckTest(unittest.TestCase):
    def scan(self, content):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(
                ["git", "init", "--quiet", str(root)], check=True, capture_output=True, env=ENV
            )
            (root / "hack").mkdir()
            script = root / "hack" / SCRIPT.name
            shutil.copyfile(SCRIPT, script)
            (root / "fixture.txt").write_text(content, encoding="utf-8")
            return subprocess.run(
                ["bash", str(script)], capture_output=True, text=True, check=False, env=ENV
            )

    def test_hex_digest_is_not_an_org_id(self):
        checksum = f"de0f{ORG_ID}" + "ab" * 22
        for content in (
            f"TOOL_SHA256_AMD64 ?= {checksum}",
            f"export TOOL_SHA256={checksum}",
            f'sha256: "{checksum.upper()}"',
            f'{{"digest": "sha256:{checksum}"}}',
        ):
            with self.subTest(content=content):
                result = self.scan(content)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_org_ids_are_rejected_without_printing_values(self):
        for content in (
            ORG_ID,
            f'org: "{ORG_ID}"',
            f"org={ORG_ID}, next",
            f"org_{ORG_ID}",
            f"ORG{ORG_ID}x",
            f"sha256 of {ORG_ID}",
        ):
            with self.subTest(content=content):
                result = self.scan(content)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn("sixteen-digit NGC or Forge org id", result.stdout)
                self.assertNotIn(ORG_ID, result.stdout + result.stderr)

    def test_longer_digit_runs_and_placeholder_pass(self):
        for content in (f"9{ORG_ID}9", "0" * 16):
            with self.subTest(content=content):
                result = self.scan(content)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
