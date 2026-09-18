# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Behavioral contract tests for the OS-image Packer Makefile."""

import os
from pathlib import Path
import shutil
import subprocess
import sys
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

    def run_make(
        self,
        target,
        *,
        cwd,
        variables=(),
        extra_env=None,
        make_options=(),
        makefile=None,
    ):
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
                str(makefile or PACKER_DIR / "Makefile"),
                *make_options,
                target,
                *variables,
            ],
            cwd=cwd,
            env=env,
            check=False,
            capture_output=True,
            text=True,
        )

    @staticmethod
    def write_executable(path, contents):
        path.write_text(contents, encoding="utf-8")
        path.chmod(0o755)

    def create_image_build_fixture(self, root, *, unsafe_artifacts=False):
        root = root.resolve()
        workspace = root / "workspace"
        workspace.mkdir()
        shutil.copy2(PACKER_DIR / "Makefile", workspace / "Makefile")
        source = root / "source"
        self.create_source_repo(source)

        scripts = workspace / "scripts"
        scripts.mkdir()
        events = root / "events.log"
        artifacts = workspace / "artifacts"
        if unsafe_artifacts:
            caller_artifacts = root / "caller-artifacts"
            caller_artifacts.mkdir()
            (caller_artifacts / "keep").write_text("caller data\n", encoding="utf-8")
            artifacts.symlink_to(caller_artifacts, target_is_directory=True)
        else:
            artifacts.mkdir()
            (artifacts / "stale").write_text("stale\n", encoding="utf-8")

        self.write_executable(
            scripts / "fetch-artifacts.sh",
            """#!/bin/sh
set -eu
printf 'fetch|%s|%s|%s|%s|%s|%s|%s|%s\\n' \
  "${SOURCE_ROOT-}" "${ARTIFACTS_DIR-}" "${ARCH-}" \
  "${K8S_VERSIONS-}" "${CALICO_VERSIONS-}" "${OVN_STREAMS-}" \
  "${SPIRE_VERSION-}" "${TPM_PLUGIN_VERSION-}" >> "${EVENT_LOG:?}"
printf 'partial\n' > "${ARTIFACTS_DIR}/partial"
if [ "${FETCH_FAIL:-0}" = "1" ]; then
  exit 42
fi
touch "${ARTIFACTS_DIR}/ready"
""",
        )
        self.write_executable(
            scripts / "prepare-installer-iso.sh",
            "#!/bin/sh\nset -eu\n",
        )

        tools = root / "tools"
        tools.mkdir()
        packer = tools / "packer"
        self.write_executable(
            packer,
            """#!/bin/sh
set -eu
printf 'packer:%s\\n' "$1" >> "${EVENT_LOG:?}"
if [ "$1" = "build" ]; then
  test -f "${EXPECTED_ARTIFACTS_DIR:?}/ready"
  test ! -e "${EXPECTED_ARTIFACTS_DIR}/stale"
fi
""",
        )
        self.write_executable(tools / "go", "#!/bin/sh\nexit 0\n")
        self.write_executable(tools / "qemu-img", "#!/bin/sh\nexit 0\n")

        ssh_dir = workspace / ".ssh"
        ssh_dir.mkdir()
        (ssh_dir / "id_ed25519").write_text("test key\n", encoding="utf-8")
        for template in ("nke-node-fde.pkr.hcl", "nke-node-non-fde.pkr.hcl"):
            (workspace / template).write_text("# test fixture\n", encoding="utf-8")

        return workspace, source, tools, packer, events, artifacts

    def run_image_build(
        self,
        target,
        arch,
        root,
        *,
        artifacts_override=None,
        fetch_fails=False,
        make_options=(),
        material_inputs_from_environment=False,
        material_inputs=None,
        python_path=None,
        promotion_fails=False,
        unsafe_artifacts=False,
    ):
        workspace, source, tools, packer, events, artifacts = (
            self.create_image_build_fixture(
                root, unsafe_artifacts=unsafe_artifacts
            )
        )
        if material_inputs is None:
            material_inputs = (
                ("K8S_VERSIONS", "1.33.5 1.34.1"),
                ("CALICO_VERSIONS", "3.30.3 3.31.0"),
                ("OVN_STREAMS", "stable next"),
                ("SPIRE_VERSION", "1.13.1"),
                ("TPM_PLUGIN_VERSION", "0.13.1"),
            )
        variables = [
            f"PACKER={packer}",
            f"PYTHON={python_path or sys.executable}",
            f"SOURCE_ROOT={source}",
            f"ARCH={arch}",
        ]
        if not material_inputs_from_environment:
            variables.extend(f"{name}={value}" for name, value in material_inputs)
        if artifacts_override is not None:
            variables.append(f"ARTIFACTS={artifacts_override}")

        real_mv = shutil.which("mv")
        if real_mv is None:
            raise RuntimeError("mv is required to test artifact promotion")
        self.write_executable(
            tools / "mv",
            """#!/bin/sh
set -eu
source_path="$1"
destination_path="$2"
if [ "$source_path" = "--" ]; then
  source_path="$2"
  destination_path="$3"
fi
case "$source_path" in
  */.artifacts.tmp.*)
    if [ "${PROMOTION_FAIL:-0}" = "1" ] && [ "$destination_path" = "${EXPECTED_ARTIFACTS_DIR:?}" ]; then
      exit 73
    fi
    ;;
esac
exec "${REAL_MV:?}" "$@"
""",
        )

        result = self.run_make(
            target,
            cwd=workspace,
            variables=variables,
            extra_env={
                "PATH": f"{tools}{os.pathsep}{os.environ['PATH']}",
                "EVENT_LOG": str(events),
                "EXPECTED_ARTIFACTS_DIR": str(artifacts),
                "ARTIFACTS_DIR": str(root / "wrong-artifacts"),
                "SOURCE_ROOT": str(root / "wrong-source"),
                "ARCH": "wrong-arch",
                **{
                    name: value if material_inputs_from_environment else f"wrong-{name}"
                    for name, value in material_inputs
                },
                "FETCH_FAIL": "1" if fetch_fails else "0",
                "PROMOTION_FAIL": "1" if promotion_fails else "0",
                "REAL_MV": real_mv,
            },
            make_options=make_options,
            makefile=workspace / "Makefile",
        )
        recorded_events = (
            events.read_text(encoding="utf-8").splitlines()
            if events.exists()
            else []
        )
        expected_fetch_fields = (
            "fetch",
            str(source),
            arch,
            *(value for _, value in material_inputs),
        )
        return result, recorded_events, artifacts, expected_fetch_fields

    def assert_single_staging_fetch(self, events, artifacts, expected_fields):
        fetch_events = [event for event in events if event.startswith("fetch|")]
        self.assertEqual(len(fetch_events), 1)
        fields = fetch_events[0].split("|")
        self.assertEqual(
            (
                fields[0],
                fields[1],
                *fields[3:],
            ),
            expected_fields,
        )
        stage = Path(fields[2])
        self.assertEqual(stage.parent, artifacts.parent)
        self.assertTrue(stage.name.startswith(".artifacts.tmp."))
        return fetch_events[0], stage

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

    def test_artifact_staging_root_is_owned_by_the_loaded_makefile(self):
        result = subprocess.run(
            [
                self.make,
                "--no-print-directory",
                "--silent",
                "--file",
                str(PACKER_DIR / "Makefile"),
                "--eval=print-artifacts: ; @printf '%s\\n' '$(ARTIFACTS)'",
                "print-artifacts",
                "CURDIR=/caller",
                "PACKER_MAKEFILE=/caller/Makefile",
                "PACKER_ROOT=/caller",
                "ARTIFACTS=/caller/data",
            ],
            check=False,
            capture_output=True,
            text=True,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), str(PACKER_DIR / "artifacts"))

        rejected = subprocess.run(
            [
                self.make,
                "--file",
                str(PACKER_DIR / "Makefile"),
                "--dry-run",
                "check-source",
                "MAKEFILE_LIST=/caller/Makefile",
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(rejected.returncode, 0)
        self.assertIn("MAKEFILE_LIST is internal", rejected.stderr)

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
                dependency_check = output.index("command -v go")
                packer_init = output.index("/tmp/nkd-packer init ")
                fetch_command = str(PACKER_DIR / "scripts" / "fetch-artifacts.sh")
                artifact_fetch = output.index(fetch_command)
                packer_build = output.index("/tmp/nkd-packer build")

                self.assertIn(template, output[packer_init:].splitlines()[0])
                self.assertEqual(output.count(fetch_command), 1)
                self.assertLess(source_check, packer_init)
                self.assertLess(dependency_check, packer_init)
                self.assertLess(source_check, artifact_fetch)
                self.assertLess(dependency_check, artifact_fetch)
                self.assertLess(artifact_fetch, packer_build)

    def test_each_image_build_stages_clean_artifacts_once_before_packer_build(self):
        cases = (
            ("os-image-fde", "amd64"),
            ("os-image-fde", "arm64"),
            ("os-image-non-fde", "amd64"),
            ("os-image-non-fde", "arm64"),
        )
        for target, arch in cases:
            with self.subTest(target=target, arch=arch), tempfile.TemporaryDirectory() as tmp:
                result, events, artifacts, expected_fields = self.run_image_build(
                    target, arch, Path(tmp)
                )

                self.assertEqual(result.returncode, 0, result.stderr)
                fetch_event, stage = self.assert_single_staging_fetch(
                    events, artifacts, expected_fields
                )
                self.assertLess(events.index(fetch_event), events.index("packer:build"))
                self.assertTrue((artifacts / "ready").is_file())
                self.assertFalse((artifacts / "stale").exists())
                self.assertFalse(stage.exists())
                self.assertEqual(list(artifacts.parent.glob(".artifacts.tmp.*")), [])

    def test_failed_artifact_staging_prevents_packer_build(self):
        cases = (
            ("os-image-fde", "amd64"),
            ("os-image-non-fde", "arm64"),
        )
        for target, arch in cases:
            with self.subTest(target=target, arch=arch), tempfile.TemporaryDirectory() as tmp:
                result, events, artifacts, expected_fields = self.run_image_build(
                    target, arch, Path(tmp), fetch_fails=True
                )

                self.assertNotEqual(result.returncode, 0)
                _, stage = self.assert_single_staging_fetch(
                    events, artifacts, expected_fields
                )
                self.assertNotIn("packer:build", events)
                self.assertEqual((artifacts / "stale").read_text(), "stale\n")
                self.assertFalse((artifacts / "partial").exists())
                self.assertFalse(stage.exists())
                self.assertEqual(list(artifacts.parent.glob(".artifacts.tmp.*")), [])

    def test_failed_artifact_promotion_restores_previous_artifacts(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, events, artifacts, expected_fields = self.run_image_build(
                "os-image-fde",
                "amd64",
                Path(tmp),
                promotion_fails=True,
            )

            self.assertNotEqual(result.returncode, 0)
            _, stage = self.assert_single_staging_fetch(
                events, artifacts, expected_fields
            )
            self.assertNotIn("packer:build", events)
            self.assertEqual((artifacts / "stale").read_text(), "stale\n")
            self.assertFalse((artifacts / "partial").exists())
            self.assertFalse(stage.exists())
            self.assertEqual(list(artifacts.parent.glob(".artifacts.tmp.*")), [])
            self.assertEqual(
                list(artifacts.parent.glob(".artifacts.backup.*")), []
            )

    def test_environment_material_inputs_reach_fetch_without_make_expansion(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            expansion_marker = root / "make-expanded"
            material_inputs = (
                ("K8S_VERSIONS", f"$(shell touch {expansion_marker})"),
                ("CALICO_VERSIONS", "3.30.3 3.31.0"),
                ("OVN_STREAMS", "stable next"),
                ("SPIRE_VERSION", "1.13.1"),
                ("TPM_PLUGIN_VERSION", "0.13.1"),
            )

            result, events, artifacts, expected_fields = self.run_image_build(
                "os-image-fde",
                "amd64",
                root,
                material_inputs=material_inputs,
                material_inputs_from_environment=True,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_single_staging_fetch(events, artifacts, expected_fields)
            self.assertFalse(expansion_marker.exists())

    def test_artifact_path_override_cannot_redirect_make_owned_staging(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            caller_artifacts = root / "caller-artifacts"
            caller_artifacts.mkdir()
            caller_file = caller_artifacts / "keep"
            caller_file.write_text("caller data\n", encoding="utf-8")

            result, events, artifacts, expected_fields = self.run_image_build(
                "os-image-non-fde",
                "amd64",
                root,
                artifacts_override=caller_artifacts,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_single_staging_fetch(events, artifacts, expected_fields)
            self.assertEqual(caller_file.read_text(encoding="utf-8"), "caller data\n")
            self.assertFalse((caller_artifacts / "ready").exists())
            self.assertTrue((artifacts / "ready").is_file())

    def test_unsafe_artifact_symlink_is_rejected_without_fetch_or_cleanup(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            result, events, artifacts, _ = self.run_image_build(
                "os-image-fde",
                "amd64",
                root,
                unsafe_artifacts=True,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("refusing unsafe artifact staging path", result.stderr)
            self.assertFalse(any(event.startswith("fetch|") for event in events))
            self.assertNotIn("packer:build", events)
            self.assertTrue(artifacts.is_symlink())
            self.assertEqual(
                (artifacts / "keep").read_text(encoding="utf-8"), "caller data\n"
            )
            self.assertEqual(list(artifacts.parent.glob(".artifacts.tmp.*")), [])

    def test_failed_dependency_preflight_blocks_parallel_fetch_and_packer(self):
        for target in ("os-image-fde", "os-image-non-fde"):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                result, events, artifacts, _ = self.run_image_build(
                    target,
                    "amd64",
                    root,
                    make_options=("--jobs=4",),
                    python_path=root / "missing-python",
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Python 3.10 or newer is required", result.stderr)
                self.assertFalse(any(event.startswith("fetch|") for event in events))
                self.assertFalse(any(event.startswith("packer:") for event in events))
                self.assertEqual((artifacts / "stale").read_text(), "stale\n")

    def test_packer_templates_do_not_fetch_host_artifacts(self):
        for template in ("nke-node-fde.pkr.hcl", "nke-node-non-fde.pkr.hcl"):
            with self.subTest(template=template):
                contents = (PACKER_DIR / template).read_text(encoding="utf-8")
                self.assertNotIn("fetch-artifacts.sh", contents)
                self.assertNotIn('provisioner "shell-local"', contents)

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
            self.write_executable(tools / "go", "#!/bin/sh\nexit 0\n")
            self.write_executable(tools / "qemu-img", "#!/bin/sh\nexit 0\n")
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
                            f"PYTHON={sys.executable}",
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
