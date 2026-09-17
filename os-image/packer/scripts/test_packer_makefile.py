# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Behavioral contract tests for the OS-image Packer Makefile."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
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

    def dry_run(self, target):
        env = os.environ.copy()
        for name in ("MAKEFLAGS", "MFLAGS", "GNUMAKEFLAGS"):
            env.pop(name, None)
        return subprocess.run(
            [
                self.make,
                "--dry-run",
                "--file",
                "Makefile",
                target,
                "PACKER=/tmp/nkd-packer",
                "PYTHON=/tmp/nkd-python",
            ],
            cwd=PACKER_DIR,
            env=env,
            check=True,
            capture_output=True,
            text=True,
        ).stdout

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

    def test_test_target_does_not_write_python_bytecode_into_source(self):
        result = subprocess.run(
            [
                self.make,
                "--dry-run",
                "--file",
                "Makefile",
                "test",
                "PYTHON=/tmp/nkd-python",
            ],
            cwd=PACKER_DIR,
            check=True,
            capture_output=True,
            text=True,
        )
        self.assertIn(
            "/tmp/nkd-python -B -m unittest discover -s scripts -p 'test_*.py'",
            result.stdout,
        )

    def test_source_preflight_runs_before_any_packer_command(self):
        cases = (
            ("os-image-fde", "nke-node-fde.pkr.hcl"),
            ("os-image-non-fde", "nke-node-non-fde.pkr.hcl"),
        )
        for target, template in cases:
            with self.subTest(target=target):
                output = self.dry_run(target)
                source_check = output.index("finalize-qcow2.py check-source")
                packer_init = output.index("/tmp/nkd-packer init ")
                packer_build = output.index("/tmp/nkd-packer build")

                self.assertIn(template, output[packer_init:].splitlines()[0])
                self.assertLess(source_check, packer_init)
                self.assertLess(source_check, packer_build)

    def test_failed_source_preflight_blocks_parallel_packer_init(self):
        with tempfile.TemporaryDirectory() as tmp:
            tools = Path(tmp)
            python = tools / "python"
            python.write_text(
                "#!/bin/sh\n"
                "if [ \"$1\" = \"-c\" ]; then exit 0; fi\n"
                "echo 'source checkout is dirty' >&2\n"
                "exit 42\n",
                encoding="utf-8",
            )
            python.chmod(0o755)
            qemu_img = tools / "qemu-img"
            qemu_img.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            qemu_img.chmod(0o755)
            packer_log = tools / "packer.log"
            packer = tools / "packer"
            packer.write_text(
                f"#!/bin/sh\nprintf '%s\\n' \"$*\" >> {packer_log}\n",
                encoding="utf-8",
            )
            packer.chmod(0o755)
            env = {
                **os.environ,
                "PATH": f"{tools}{os.pathsep}{os.environ['PATH']}",
            }
            for target in ("init-fde", "init-non-fde"):
                with self.subTest(target=target):
                    packer_log.unlink(missing_ok=True)
                    result = subprocess.run(
                        [
                            self.make,
                            "--jobs=4",
                            "--file",
                            "Makefile",
                            target,
                            f"PACKER={packer}",
                            f"PYTHON={python}",
                        ],
                        cwd=PACKER_DIR,
                        env=env,
                        check=False,
                        capture_output=True,
                        text=True,
                    )

                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("source checkout is dirty", result.stderr)
                    self.assertFalse(packer_log.exists())


if __name__ == "__main__":
    unittest.main()
