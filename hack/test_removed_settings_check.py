# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise the removed-settings scan against isolated repository fixtures."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("removed-settings-check.py")
# A hook can export GIT_DIR and friends, which would point git at the outer
# repository instead of the fixture.
ENV = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}


class RemovedSettingsCheckTest(unittest.TestCase):
    def run_check(self, files):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(["git", "init", "--quiet", str(root)], check=True, capture_output=True, env=ENV)
            (root / "hack").mkdir()
            shutil.copyfile(SCRIPT, root / "hack" / SCRIPT.name)
            for name, content in files.items():
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(content, encoding="utf-8")
            subprocess.run(["git", "-C", str(root), "add", "-A"], check=True, capture_output=True, env=ENV)
            return subprocess.run(
                ["python3", "-B", f"hack/{SCRIPT.name}"], cwd=root, capture_output=True, text=True, check=False, env=ENV,
            )

    def test_removed_names_fail(self):
        for line in (
            'port := os.Getenv("CWE_HTTP_PORT")',
            "NVKE_CONTROLLER_HTTP_PORT=${NVKE_CONTROLLER_HTTP_PORT:-${NKE_CONTROLLER_HTTP_PORT:-8080}}",
            "image: ${{ vars.CWE_NAMESPACE }}",
            "CWE_IMAGE ?= controller:latest",
            "export NKE_CONTROLLER_GOFAIL_E2E_WORKER_BINARY=bin/worker",
            "prefix = \"NKE_CONTROLLER_GOFAIL_E2E_\"",
            'cfg.get("cwe_image", "")',
            "LOCAL_CWE_IMAGE=controller:dev make test-e2e-capd-local",
            "--cwe-api) API=$2 ;;",
            "      cwe-name:",
            'export CWE_API="http://localhost:8080/api"',
        ):
            with self.subTest(line=line):
                result = self.run_check({"fixture.sh": line + "\n"})
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn("fixture.sh:1:", result.stdout)

    def test_current_names_pass(self):
        result = self.run_check({"fixture.sh": "\n".join((
            "NVKE_CONTROLLER_HTTP_PORT=${NVKE_CONTROLLER_HTTP_PORT:-8080}",
            "CONTROLLER_IMAGE ?= controller:latest",
            "LOCAL_NVKE_CONTROLLER_IMAGE=controller:dev",
            'cfg.get("nvke_controller_image", "")',
            "--controller-api) API=$2 ;;",
            "      controller-name:",
            "--cwe-api-url) URL=$2 ;;",
            "LOCAL_CWE_IMAGE_TAG=x MY_CWE_IMAGE=y CWE_IMAGES=z",
            "See CWE-79 and CWE_79.",
        )) + "\n"})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("removed-settings-check: clean", result.stdout)

    def test_allowed_token_passes_only_in_its_file(self):
        line = 'CWE_API_URL="${2%/}"\n'
        allowed = self.run_check({"controller/scripts/cwe-to-platform-crs.sh": line})
        self.assertEqual(allowed.returncode, 0, allowed.stdout + allowed.stderr)
        elsewhere = self.run_check({"controller/scripts/other.sh": line})
        self.assertEqual(elsewhere.returncode, 1, elsewhere.stdout + elsewhere.stderr)


if __name__ == "__main__":
    unittest.main()
