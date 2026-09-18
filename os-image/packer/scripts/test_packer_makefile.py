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

    def run_make(self, target, *, cwd, variables=(), extra_env=None):
        env = os.environ.copy()
        for name in ("MAKEFLAGS", "MFLAGS", "GNUMAKEFLAGS"):
            env.pop(name, None)
        env.update(extra_env or {})
        return subprocess.run(
            [
                self.make,
                "--no-print-directory",
                "--silent",
                "--file",
                str(PACKER_DIR / "Makefile"),
                target,
                *variables,
            ],
            cwd=cwd,
            env=env,
            check=False,
            capture_output=True,
            text=True,
        )

    def test_generic_dist_dir_is_not_exported_to_build_subprocesses(self):
        env = os.environ.copy()
        for name in ("MAKEFLAGS", "MFLAGS", "GNUMAKEFLAGS", "DIST_DIR"):
            env.pop(name, None)

        result = subprocess.run(
            [
                self.make,
                "--silent",
                "--file",
                "Makefile",
                "--eval=probe-dist-env: ; @if printenv DIST_DIR >/dev/null 2>&1; "
                "then echo 'DIST_DIR leaked into build subprocess' >&2; exit 1; fi",
                "probe-dist-env",
            ],
            cwd=PACKER_DIR,
            env=env,
            check=False,
            capture_output=True,
            text=True,
        )

        self.assertEqual(result.returncode, 0, result.stderr)

    @staticmethod
    def create_source_repo(path):
        path.mkdir()
        (path / "tracked.txt").write_text("tracked\n", encoding="utf-8")
        (path / ".gitignore").write_text("ignored/\n", encoding="utf-8")
        subprocess.run(["git", "init", "-q"], cwd=path, check=True)
        subprocess.run(
            ["git", "add", "tracked.txt", ".gitignore"], cwd=path, check=True
        )
        subprocess.run(
            [
                "git",
                "-c",
                "user.name=Packer Makefile Test",
                "-c",
                "user.email=packer-makefile@example.invalid",
                "-c",
                "commit.gpgsign=false",
                "commit",
                "-qm",
                "fixture",
            ],
            cwd=path,
            check=True,
        )
        return subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=path,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()

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
                source_check = output.index('git -C "$SOURCE_ROOT" rev-parse')
                packer_init = output.index("/tmp/nkd-packer init ")
                packer_build = output.index("/tmp/nkd-packer build")

                self.assertIn(template, output[packer_init:].splitlines()[0])
                self.assertLess(source_check, packer_init)
                self.assertLess(source_check, packer_build)

    def test_failed_source_preflight_blocks_parallel_packer_init(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source = root / "source"
            self.create_source_repo(source)
            (source / "untracked.txt").write_text("dirty\n", encoding="utf-8")
            tools = root / "tools"
            tools.mkdir()
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
                            f"SOURCE_ROOT={source}",
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

    def test_source_preflight_accepts_only_the_expected_clean_commit(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source = root / "source"
            workspace = root / "workspace"
            workspace.mkdir()
            commit = self.create_source_repo(source)
            variables = (f"SOURCE_ROOT={source}", f"EXPECTED_SOURCE_COMMIT={commit}")

            clean = self.run_make("check-source", cwd=workspace, variables=variables)
            self.assertEqual(clean.returncode, 0, clean.stderr)
            self.assertEqual(clean.stdout, "")

            ignored = source / "ignored" / "cache-entry"
            ignored.parent.mkdir()
            ignored.write_text("ignored\n", encoding="utf-8")
            still_clean = self.run_make(
                "check-source", cwd=workspace, variables=variables
            )
            self.assertEqual(still_clean.returncode, 0, still_clean.stderr)

            wrong_commit = self.run_make(
                "check-source",
                cwd=workspace,
                variables=(f"SOURCE_ROOT={source}", "EXPECTED_SOURCE_COMMIT=wrong"),
            )
            self.assertNotEqual(wrong_commit.returncode, 0)
            self.assertIn("does not match expected", wrong_commit.stderr)

            (source / "untracked.txt").write_text("dirty\n", encoding="utf-8")
            dirty = self.run_make("check-source", cwd=workspace, variables=variables)
            self.assertNotEqual(dirty.returncode, 0)
            self.assertIn("source checkout is dirty", dirty.stderr)

    def test_collection_copies_exact_bytes_and_prints_one_basename(self):
        cases = (
            ("fde", "amd64"),
            ("non-fde", "arm64"),
        )
        for variant, arch in cases:
            with self.subTest(variant=variant, arch=arch), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                source = root / "source"
                workspace = root / "workspace"
                workspace.mkdir()
                commit = self.create_source_repo(source)
                output = workspace / f"output-{variant}"
                output.mkdir()
                name = f"nke-node-{variant}-ubuntu-26.04.1-{arch}.qcow2"
                payload = f"{variant}-{arch}-bytes".encode()
                (output / name).write_bytes(payload)
                dist = workspace / "dist"

                result = self.run_make(
                    f"collect-qcow2-{variant}",
                    cwd=workspace,
                    variables=(
                        f"SOURCE_ROOT={source}",
                        f"EXPECTED_SOURCE_COMMIT={commit}",
                        f"QCOW2_DIST_DIR={dist}",
                        f"ARCH={arch}",
                    ),
                )

                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, name + "\n")
                self.assertEqual((output / name).read_bytes(), payload)
                self.assertEqual((dist / name).read_bytes(), payload)

    def test_collection_rejects_ambiguous_wrong_or_non_regular_outputs(self):
        cases = ("missing", "multiple", "wrong-name", "symlink")
        for case in cases:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                source = root / "source"
                workspace = root / "workspace"
                workspace.mkdir()
                commit = self.create_source_repo(source)
                output = workspace / "output-fde"
                output.mkdir()
                expected = output / "nke-node-fde-ubuntu-26.04.1-amd64.qcow2"
                if case == "multiple":
                    expected.write_bytes(b"one")
                    (output / "extra.qcow2").write_bytes(b"two")
                elif case == "wrong-name":
                    (output / "nke-node-non-fde-ubuntu-26.04.1-amd64.qcow2").write_bytes(b"wrong")
                elif case == "symlink":
                    target = output / "image.data"
                    target.write_bytes(b"target")
                    expected.symlink_to(target.name)
                dist = workspace / "dist"

                result = self.run_make(
                    "collect-qcow2-fde",
                    cwd=workspace,
                    variables=(
                        f"SOURCE_ROOT={source}",
                        f"EXPECTED_SOURCE_COMMIT={commit}",
                        f"QCOW2_DIST_DIR={dist}",
                    ),
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(dist.exists())

    def test_collection_rechecks_source_and_requires_destination(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source = root / "source"
            workspace = root / "workspace"
            workspace.mkdir()
            commit = self.create_source_repo(source)
            output = workspace / "output-fde"
            output.mkdir()
            name = "nke-node-fde-ubuntu-26.04.1-amd64.qcow2"
            (output / name).write_bytes(b"bytes")

            no_destination = self.run_make(
                "collect-qcow2-fde",
                cwd=workspace,
                variables=(
                    f"SOURCE_ROOT={source}",
                    f"EXPECTED_SOURCE_COMMIT={commit}",
                ),
            )
            self.assertNotEqual(no_destination.returncode, 0)
            self.assertIn("QCOW2_DIST_DIR is required", no_destination.stderr)

            (source / "untracked.txt").write_text("dirty\n", encoding="utf-8")
            dirty = self.run_make(
                "collect-qcow2-fde",
                cwd=workspace,
                variables=(
                    f"SOURCE_ROOT={source}",
                    f"EXPECTED_SOURCE_COMMIT={commit}",
                    f"QCOW2_DIST_DIR={workspace / 'dist'}",
                ),
            )
            self.assertNotEqual(dirty.returncode, 0)
            self.assertIn("source checkout is dirty", dirty.stderr)
            self.assertFalse((workspace / "dist").exists())

    def test_collection_removes_destination_when_byte_comparison_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source = root / "source"
            workspace = root / "workspace"
            workspace.mkdir()
            commit = self.create_source_repo(source)
            output = workspace / "output-fde"
            output.mkdir()
            name = "nke-node-fde-ubuntu-26.04.1-amd64.qcow2"
            (output / name).write_bytes(b"source-bytes")
            dist = workspace / "dist"
            tools = root / "tools"
            tools.mkdir()
            cmp = tools / "cmp"
            cmp.write_text("#!/bin/sh\nexit 1\n", encoding="utf-8")
            cmp.chmod(0o755)

            result = self.run_make(
                "collect-qcow2-fde",
                cwd=workspace,
                variables=(
                    f"SOURCE_ROOT={source}",
                    f"EXPECTED_SOURCE_COMMIT={commit}",
                    f"QCOW2_DIST_DIR={dist}",
                ),
                extra_env={"PATH": f"{tools}{os.pathsep}{os.environ['PATH']}"},
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.stdout, "")
            self.assertIn("differs from source", result.stderr)
            self.assertFalse((dist / name).exists())
            self.assertEqual((output / name).read_bytes(), b"source-bytes")


if __name__ == "__main__":
    unittest.main()
