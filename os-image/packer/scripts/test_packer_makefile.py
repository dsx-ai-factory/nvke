# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Behavioral contract tests for the OS-image Packer Makefile."""

import os
from pathlib import Path
import shutil
import subprocess
import unittest


PACKER_DIR = Path(__file__).resolve().parent.parent
REPO_ROOT = PACKER_DIR.parents[1]


class PackerMakefileTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.make = shutil.which("gmake") or shutil.which("make")
        if cls.make is None:
            raise RuntimeError("GNU Make is required to test the Packer Makefile")

        version = subprocess.run(
            [cls.make, "--version"],
            check=False,
            capture_output=True,
            text=True,
        )
        if version.returncode != 0 or "GNU Make" not in version.stdout:
            raise RuntimeError("GNU Make is required to test the Packer Makefile")

    def exported_packer_cache_dir(
        self,
        environment_override=None,
        command_line_override=None,
    ):
        env = os.environ.copy()
        for name in ("MAKEFLAGS", "MFLAGS", "GNUMAKEFLAGS"):
            env.pop(name, None)

        if environment_override is None:
            env.pop("PACKER_CACHE_DIR", None)
        else:
            env["PACKER_CACHE_DIR"] = str(environment_override)

        command = [
            self.make,
            "--silent",
            "--file",
            "Makefile",
            "--eval=print-packer-cache: ; @printenv PACKER_CACHE_DIR",
            "print-packer-cache",
        ]
        if command_line_override is not None:
            command.append(f"PACKER_CACHE_DIR={command_line_override}")

        result = subprocess.run(
            command,
            cwd=PACKER_DIR,
            env=env,
            check=True,
            capture_output=True,
            text=True,
        )
        return result.stdout.strip()

    def test_default_packer_cache_is_exported_under_ignored_iso_cache(self):
        expected = PACKER_DIR / "iso-cache" / "packer-cache"
        self.assertEqual(self.exported_packer_cache_dir(), str(expected))

        ignored_probe = expected / "cache-entry"
        ignored = subprocess.run(
            [
                "git",
                "check-ignore",
                "--quiet",
                str(ignored_probe.relative_to(REPO_ROOT)),
            ],
            cwd=REPO_ROOT,
            capture_output=True,
            text=True,
        )
        self.assertEqual(
            ignored.returncode,
            0,
            f"default Packer cache is not ignored: {ignored.stderr}",
        )

    def test_packer_cache_environment_override_is_preserved_and_exported(self):
        override = PACKER_DIR.parent / "caller-owned-packer-cache"
        self.assertEqual(self.exported_packer_cache_dir(override), str(override))

    def test_empty_environment_packer_cache_override_uses_safe_default(self):
        expected = PACKER_DIR / "iso-cache" / "packer-cache"
        self.assertEqual(
            self.exported_packer_cache_dir(""),
            str(expected),
        )

    def test_packer_cache_command_line_override_semantics(self):
        safe_default = PACKER_DIR / "iso-cache" / "packer-cache"
        command_line_cache = PACKER_DIR.parent / "command-line-packer-cache"
        environment_cache = PACKER_DIR.parent / "environment-packer-cache"
        cases = (
            ("empty uses safe default", "", safe_default),
            ("non-empty wins over environment", command_line_cache, command_line_cache),
        )

        for name, command_line_override, expected in cases:
            with self.subTest(name=name):
                self.assertEqual(
                    self.exported_packer_cache_dir(
                        environment_override=environment_cache,
                        command_line_override=command_line_override,
                    ),
                    str(expected),
                )


if __name__ == "__main__":
    unittest.main()
