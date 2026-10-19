# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Behavioral contract tests for the OS-image Packer Makefile."""

import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
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

    @staticmethod
    def stage_packer_makefile(root):
        """Copy the Makefile into a repo-shaped workspace under root.

        The Makefile includes ../../make/tool-versions.mk for the committed
        installer digests, so a copy has to sit two levels below a directory
        holding that file. Mirror os-image/packer and stage the real
        tool-versions.mk beside it, so the fixture resolves ISO_CHECKSUM the
        way a real checkout does.
        """
        workspace = root / "os-image" / "packer"
        workspace.mkdir(parents=True)
        shutil.copy2(PACKER_DIR / "Makefile", workspace / "Makefile")
        make_dir = root / "make"
        make_dir.mkdir(exist_ok=True)
        shutil.copy2(REPO_ROOT / "make" / "tool-versions.mk", make_dir / "tool-versions.mk")
        return workspace

    def create_image_build_fixture(self, root, *, unsafe_artifacts=False):
        root = root.resolve()
        workspace = self.stage_packer_makefile(root)
        source = root / "source"
        self.create_source_repo(source)

        scripts = workspace / "scripts"
        scripts.mkdir()
        shutil.copy2(PACKER_DIR / "scripts" / "qemu-arch.sh", scripts / "qemu-arch.sh")
        shutil.copy2(PACKER_DIR / "scripts" / "run-packer-build.sh", scripts / "run-packer-build.sh")
        shutil.copy2(PACKER_DIR / "scripts" / "package-builder.sh", scripts / "package-builder.sh")
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
            """#!/bin/sh
set -eu
mkdir -p "${OUT_DIR:?}"
printf 'patched installer iso\n' > "${OUT_DIR}/${ISO_URL##*/}.patched.iso"
""",
        )

        tools = root / "tools"
        tools.mkdir()
        packer = tools / "packer"
        self.write_executable(
            packer,
            """#!/bin/sh
set -eu
printf 'packer:%s\\n' "$1" >> "${EVENT_LOG:?}"
printf 'packer-args|%s\\n' "$*" >> "${EVENT_LOG:?}"
if [ "$1" = "build" ] && [ "${PACKER_KVM_DENIED_ONCE:-0}" = "1" ] && [ ! -e "${EVENT_LOG}.kvm-denied" ]; then
  : > "${EVENT_LOG}.kvm-denied"
  printf 'Qemu stderr: Could not access KVM kernel module: Permission denied\\n' >> "${PACKER_LOG_PATH:?}"
  exit 1
fi
if [ "$1" = "build" ] && [ "${PACKER_BUILD_FAIL:-0}" = "1" ]; then
  printf 'Error launching VM: Qemu failed to start\\n' >> "${PACKER_LOG_PATH:?}"
  exit 1
fi
if [ "$1" = "build" ]; then
  test -f "${EXPECTED_ARTIFACTS_DIR:?}/ready"
  test ! -e "${EXPECTED_ARTIFACTS_DIR}/stale"
  output_directory=""
  variant=""
  for argument in "$@"; do
    case "${argument}" in
      output_directory=*) output_directory="${argument#output_directory=}" ;;
      -only=nvke-node-fde.qemu.nvke-fde) variant=fde ;;
      -only=nvke-node-non-fde.qemu.nvke-non-fde) variant=non-fde ;;
    esac
  done
  test -n "${output_directory}"
  test -n "${variant}"
  mkdir -p "${output_directory}/${variant}"
  printf 'qcow2 bytes\n' > "${output_directory}/${variant}/nvke-node.qcow2"
  printf 'efi vars\n' > "${output_directory}/${variant}/efivars.fd"
fi
""",
        )
        self.write_executable(tools / "go", "#!/bin/sh\nexit 0\n")
        self.write_executable(tools / "qemu-img", "#!/bin/sh\nexit 0\n")
        self.write_executable(tools / "sudo", "#!/bin/sh\nexit 0\n")
        self.write_executable(
            tools / "ssh-keygen",
            """#!/bin/sh
set -eu
if [ "${1-}" = "-y" ]; then
  shift
  test "${1-}" = "-f"
  key="${2:?}"
  if grep -q '^ENCRYPTED$' "${key}"; then
    exit 1
  fi
  key_data="$(sed -n 's/^KEY://p' "${key}")"
  test -n "${key_data}"
  printf '%s generated-comment\n' "${key_data}"
  exit 0
fi
key=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-f" ]; then
    shift
    key="${1:?}"
  fi
  shift
done
test -n "${key}"
mkdir -p "${key%/*}"
printf '%s\n' 'KEY:ssh-ed25519 generated-key' > "${key}"
printf '%s\n' 'ssh-ed25519 generated-key generated' > "${key}.pub"
""",
        )

        ssh_dir = workspace / ".ssh"
        ssh_dir.mkdir()
        (ssh_dir / "id_ed25519").write_text(
            "KEY:ssh-ed25519 test-key\n", encoding="utf-8"
        )
        (ssh_dir / "id_ed25519.pub").write_text(
            "ssh-ed25519 test-key packer-test\n", encoding="utf-8"
        )
        for template in (
            "versions.pkr.hcl",
            "variables.pkr.hcl",
            "locals.pkr.hcl",
            "nvke-node-fde.pkr.hcl",
            "nvke-node-non-fde.pkr.hcl",
        ):
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
        preexisting_output=False,
        preexisting_scratch=False,
        preexisting_other_scratch=False,
        ssh_key_files=None,
        generate_ssh_key=False,
        generated_key_state=None,
        guest_memory_mib=None,
        packer_kvm_denied_once=False,
        packer_build_fails=False,
    ):
        workspace, source, tools, packer, events, artifacts = (
            self.create_image_build_fixture(
                root, unsafe_artifacts=unsafe_artifacts
            )
        )
        host_arch = "x86_64" if arch == "amd64" else "aarch64"
        self.write_executable(
            tools / "uname",
            f'#!/bin/sh\ncase "$1" in -m) echo {host_arch};; -s) echo Linux;; esac\n',
        )
        self.write_executable(
            tools / f"qemu-system-{host_arch}",
            '#!/bin/sh\nif [ "$1" = -accel ]; then echo tcg; fi\n',
        )
        if generate_ssh_key or generated_key_state is not None:
            shutil.rmtree(workspace / ".ssh")
        if generated_key_state is not None:
            ssh_dir = workspace / ".ssh"
            ssh_dir.mkdir()
            if generated_key_state == "private-only":
                (ssh_dir / "id_ed25519").write_text(
                    "KEY:ssh-ed25519 generated-key\n", encoding="utf-8"
                )
            elif generated_key_state == "public-only":
                (ssh_dir / "id_ed25519.pub").write_text(
                    "ssh-ed25519 generated-key generated\n", encoding="utf-8"
                )
            else:
                raise ValueError(f"unknown generated key state: {generated_key_state}")
        if preexisting_output:
            variant = "fde" if target == "os-image-fde" else "non-fde"
            output = workspace / f"output-{variant}"
            output.mkdir()
            (output / "keep").write_text("existing output\n", encoding="utf-8")
        if preexisting_scratch:
            variant = "fde" if target == "os-image-fde" else "non-fde"
            scratch = workspace / f".packer-output-{variant}"
            scratch.mkdir()
            (scratch / "keep").write_text("existing scratch\n", encoding="utf-8")
        if preexisting_other_scratch:
            other_variant = "non-fde" if target == "os-image-fde" else "fde"
            scratch = workspace / f".packer-output-{other_variant}"
            scratch.mkdir()
            (scratch / "keep").write_text("other scratch\n", encoding="utf-8")
        if material_inputs is None:
            material_inputs = (
                ("K8S_VERSIONS", "1.33.5 1.34.1"),
                ("CALICO_VERSIONS", "3.30.3 3.31.0"),
                ("OVN_STREAMS", "stable next"),
                ("SPIRE_VERSION", "1.13.1"),
                ("TPM_PLUGIN_VERSION", "0.13.1"),
            )
        for name in ("code.fd", "vars.fd"):
            (root / name).touch()
        variables = [
            f"PACKER={packer}",
            f"PYTHON={python_path or sys.executable}",
            f"SOURCE_ROOT={source}",
            f"ARCH={arch}",
            "ACCELERATOR=tcg",
            f"OVMF_CODE={root / 'code.fd'}",
            f"OVMF_VARS_TEMPLATE={root / 'vars.fd'}",
            "UBUNTU_VERSION=not-the-iso-version",
        ]
        if not material_inputs_from_environment:
            variables.extend(f"{name}={value}" for name, value in material_inputs)
        if artifacts_override is not None:
            variables.append(f"ARTIFACTS={artifacts_override}")
        if guest_memory_mib is not None:
            variables.append(f"GUEST_MEMORY_MIB={guest_memory_mib}")
        if ssh_key_files is not None:
            private_key, public_key = ssh_key_files
            if private_key is not None:
                variables.append(f"SSH_PRIVATE_KEY_FILE={private_key}")
            if public_key is not None:
                variables.append(f"SSH_PUBLIC_KEY_FILE={public_key}")

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
                "PACKER_KVM_DENIED_ONCE": "1" if packer_kvm_denied_once else "0",
                "PACKER_BUILD_FAIL": "1" if packer_build_fails else "0",
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

    def test_variant_packer_scratch_directories_are_ignored(self):
        for name in (".packer-output-fde", ".packer-output-non-fde"):
            with self.subTest(name=name):
                ignored = subprocess.run(
                    [
                        "git",
                        "check-ignore",
                        "--quiet",
                        str((PACKER_DIR / name / "scratch").relative_to(REPO_ROOT)),
                    ],
                    cwd=REPO_ROOT,
                    check=False,
                )
                self.assertEqual(ignored.returncode, 0)

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

    def test_unrelated_target_ignores_incomplete_ssh_key_environment(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = self.run_make(
                "clean",
                cwd=Path(tmp),
                make_options=("--dry-run",),
                extra_env={"SSH_PRIVATE_KEY_FILE": "/caller/private-key"},
            )

            self.assertEqual(result.returncode, 0, result.stderr)

    def test_check_deps_rejects_missing_ssh_keygen(self):
        with tempfile.TemporaryDirectory() as tmp:
            empty_path = Path(tmp) / "empty-bin"
            empty_path.mkdir()
            result = self.run_make(
                "check-deps",
                cwd=PACKER_DIR,
                variables=(f"PYTHON={sys.executable}",),
                extra_env={"PATH": str(empty_path)},
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn(
                "ssh-keygen is required to prepare the Packer SSH key pair",
                result.stderr,
            )

    def test_source_preflight_runs_before_any_packer_command(self):
        cases = (
            ("os-image-fde", "nvke-node-fde.qemu.nvke-fde"),
            ("os-image-non-fde", "nvke-node-non-fde.qemu.nvke-non-fde"),
        )
        for target, selector in cases:
            with self.subTest(target=target):
                output = self.dry_run(target)
                source_check = output.index('git -C "$SOURCE_ROOT" rev-parse')
                dependency_check = output.index("bash ./scripts/package-builder.sh --check")
                packer_init = output.index("/tmp/nkd-packer init ")
                fetch_command = str(PACKER_DIR / "scripts" / "fetch-artifacts.sh")
                artifact_fetch = output.index(fetch_command)
                packer_build = output.index("/tmp/nkd-packer build")

                self.assertIn(str(PACKER_DIR), output[packer_init:].splitlines()[0])
                build_command = output[packer_build:]
                self.assertIn(f"-only={selector}", build_command)
                self.assertIn(str(PACKER_DIR), build_command)
                self.assertEqual(output.count(fetch_command), 1)
                self.assertLess(source_check, packer_init)
                self.assertLess(dependency_check, packer_init)
                self.assertLess(source_check, artifact_fetch)
                self.assertLess(dependency_check, artifact_fetch)
                self.assertLess(artifact_fetch, packer_build)

    def test_each_image_build_stages_clean_artifacts_once_before_packer_build(self):
        cases = (
            ("os-image-fde", "amd64", "nvke-node-fde.qemu.nvke-fde"),
            ("os-image-fde", "arm64", "nvke-node-fde.qemu.nvke-fde"),
            (
                "os-image-non-fde",
                "amd64",
                "nvke-node-non-fde.qemu.nvke-non-fde",
            ),
            (
                "os-image-non-fde",
                "arm64",
                "nvke-node-non-fde.qemu.nvke-non-fde",
            ),
        )
        for target, arch, selector in cases:
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

                build_events = [
                    event
                    for event in events
                    if event.startswith("packer-args|build ")
                ]
                self.assertEqual(len(build_events), 1)
                build = build_events[0]
                variant = "fde" if target == "os-image-fde" else "non-fde"
                self.assertIn(f"-only={selector}", build)
                self.assertIn(f"-var architecture={arch}", build)
                self.assertIn("-var guest_memory_mib=8192", build)
                self.assertIn(
                    f"-var material_manifest={artifacts.parent}/.packer-inputs/material-manifest.json",
                    build,
                )
                self.assertIn(
                    f"-var output_directory={artifacts.parent}/.packer-output-{variant}",
                    build,
                )
                self.assertIn(
                    f"-var ssh_private_key_file={artifacts.parent}/.ssh/id_ed25519",
                    build,
                )
                self.assertIn(
                    f"-var ssh_public_key_file={artifacts.parent}/.ssh/id_ed25519.pub",
                    build,
                )
                self.assertTrue(build.endswith(str(artifacts.parent)))

                output_name = (
                    f"nvke-node-{variant}-ubuntu-26.04.1-{arch}.qcow2"
                )
                self.assertEqual(
                    (artifacts.parent / f"output-{variant}" / output_name).read_bytes(),
                    b"qcow2 bytes\n",
                )
                self.assertFalse(
                    (artifacts.parent / f".packer-output-{variant}").exists()
                )

                archive = artifacts.parent / ".packer-inputs/materials/image-inputs"
                self.assertTrue(archive.is_file())
                with tarfile.open(archive) as image_inputs:
                    self.assertIn("./ready", image_inputs.getnames())
                manifest = json.loads(
                    (
                        artifacts.parent
                        / ".packer-inputs/material-manifest.json"
                    ).read_text(encoding="utf-8")
                )
                materials = {
                    material["binding"]: material
                    for material in manifest["materials"]
                }
                self.assertEqual(
                    materials["installer-iso"]["digest"],
                    "sha256:"
                    + hashlib.sha256(b"patched installer iso\n").hexdigest(),
                )
                self.assertEqual(manifest["schemaVersion"], "nvke.nvidia.com/os-image-materials/v1")
                self.assertEqual(materials["installer-iso"]["architecture"], arch)
                self.assertEqual(materials["image-inputs"]["architecture"], arch)
                self.assertEqual(
                    materials["image-inputs"]["digest"],
                    "sha256:" + hashlib.sha256(archive.read_bytes()).hexdigest(),
                )

    def test_guest_memory_override_reaches_both_packer_builds(self):
        for target in ("os-image-fde", "os-image-non-fde"):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as tmp:
                result, events, _, _ = self.run_image_build(
                    target, "arm64", Path(tmp), guest_memory_mib=16384
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                builds = [event for event in events if event.startswith("packer-args|build ")]
                self.assertEqual(len(builds), 1)
                self.assertIn("-var guest_memory_mib=16384", builds[0])

    def test_packer_build_retries_once_when_qemu_cannot_open_kvm(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, events, artifacts, _ = self.run_image_build(
                "os-image-fde", "amd64", Path(tmp), packer_kvm_denied_once=True
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            builds = [event for event in events if event.startswith("packer-args|build ")]
            self.assertEqual(len(builds), 2)
            kept_log = artifacts.parent / "build-logs" / "packer-fde.kvm-denied.log"
            self.assertIn(
                "Could not access KVM kernel module",
                kept_log.read_text(encoding="utf-8"),
            )

    def test_packer_build_does_not_retry_other_failures(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, events, _, _ = self.run_image_build(
                "os-image-non-fde", "arm64", Path(tmp), packer_build_fails=True
            )
            self.assertNotEqual(result.returncode, 0)
            builds = [event for event in events if event.startswith("packer-args|build ")]
            self.assertEqual(len(builds), 1)

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

    def test_make_uses_supplied_matching_ssh_key_pair(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = root / "caller state"
            state.mkdir()
            private_key = state / "build-key"
            public_key = state / "build-key.pub"
            private_key.write_text(
                "KEY:ssh-ed25519 supplied-key\n", encoding="utf-8"
            )
            public_key.write_text(
                "ssh-ed25519 supplied-key caller\n", encoding="utf-8"
            )

            result, events, _, _ = self.run_image_build(
                "os-image-non-fde",
                "amd64",
                root,
                ssh_key_files=(private_key, public_key),
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            build = next(
                event for event in events if event.startswith("packer-args|build ")
            )
            self.assertIn(f"-var ssh_private_key_file={private_key}", build)
            self.assertIn(f"-var ssh_public_key_file={public_key}", build)

    def test_make_generates_ssh_key_pair_when_none_is_supplied(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            result, events, artifacts, _ = self.run_image_build(
                "os-image-non-fde",
                "amd64",
                root,
                generate_ssh_key=True,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            private_key = artifacts.parent / ".ssh/id_ed25519"
            public_key = artifacts.parent / ".ssh/id_ed25519.pub"
            self.assertEqual(
                private_key.read_text(encoding="utf-8"),
                "KEY:ssh-ed25519 generated-key\n",
            )
            self.assertEqual(
                public_key.read_text(encoding="utf-8"),
                "ssh-ed25519 generated-key generated\n",
            )
            build = next(
                event for event in events if event.startswith("packer-args|build ")
            )
            self.assertIn(f"-var ssh_private_key_file={private_key}", build)
            self.assertIn(f"-var ssh_public_key_file={public_key}", build)

    def test_make_rejects_invalid_ssh_key_inputs(self):
        cases = (
            "private-only",
            "public-only",
            "relative",
            "mismatched",
            "symlink-private",
            "symlink-public",
            "missing-public",
            "non-regular-private",
            "malformed-public",
            "encrypted-private",
        )
        for case in cases:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                private_key = root / "caller-key"
                public_key = root / "caller-key.pub"
                private_key.write_text(
                    "KEY:ssh-ed25519 supplied-key\n", encoding="utf-8"
                )
                public_key.write_text(
                    "ssh-ed25519 other-key caller\n", encoding="utf-8"
                )
                if case == "symlink-private":
                    real_private_key = root / "real-caller-key"
                    private_key.rename(real_private_key)
                    private_key.symlink_to(real_private_key)
                    public_key.write_text(
                        "ssh-ed25519 supplied-key caller\n", encoding="utf-8"
                    )
                elif case == "symlink-public":
                    real_public_key = root / "real-caller-key.pub"
                    public_key.write_text(
                        "ssh-ed25519 supplied-key caller\n", encoding="utf-8"
                    )
                    public_key.rename(real_public_key)
                    public_key.symlink_to(real_public_key)
                elif case == "missing-public":
                    public_key.unlink()
                elif case == "non-regular-private":
                    private_key.unlink()
                    private_key.mkdir()
                elif case == "malformed-public":
                    public_key.write_text("ssh-ed25519\n", encoding="utf-8")
                elif case == "encrypted-private":
                    private_key.write_text("ENCRYPTED\n", encoding="utf-8")
                    public_key.write_text(
                        "ssh-ed25519 supplied-key caller\n", encoding="utf-8"
                    )
                key_files = {
                    "private-only": (private_key, None),
                    "public-only": (None, public_key),
                    "relative": (Path("caller-key"), Path("caller-key.pub")),
                    "mismatched": (private_key, public_key),
                    "symlink-private": (private_key, public_key),
                    "symlink-public": (private_key, public_key),
                    "missing-public": (private_key, public_key),
                    "non-regular-private": (private_key, public_key),
                    "malformed-public": (private_key, public_key),
                    "encrypted-private": (private_key, public_key),
                }[case]

                result, events, _, _ = self.run_image_build(
                    "os-image-non-fde",
                    "amd64",
                    root,
                    ssh_key_files=key_files,
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(
                    any(event.startswith("packer-args|build ") for event in events)
                )
                if case == "private-only":
                    self.assertIn("SSH_PUBLIC_KEY_FILE is required", result.stderr)
                elif case == "public-only":
                    self.assertIn("SSH_PRIVATE_KEY_FILE is required", result.stderr)
                elif case == "relative":
                    self.assertIn("SSH private key path must be absolute", result.stderr)
                elif case in ("symlink-private", "non-regular-private"):
                    self.assertIn("SSH private key is not a regular file", result.stderr)
                elif case in ("symlink-public", "missing-public"):
                    self.assertIn("SSH public key is not a regular file", result.stderr)
                elif case == "encrypted-private":
                    self.assertIn(
                        "SSH private key must be readable and unencrypted",
                        result.stderr,
                    )
                else:
                    self.assertIn(
                        "SSH public key does not match private key", result.stderr
                    )

    def test_make_rejects_incomplete_generated_ssh_key_state(self):
        for state in ("private-only", "public-only"):
            with self.subTest(state=state), tempfile.TemporaryDirectory() as tmp:
                result, events, _, _ = self.run_image_build(
                    "os-image-non-fde",
                    "amd64",
                    Path(tmp),
                    generated_key_state=state,
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertIn("generated SSH key state is incomplete", result.stderr)
                self.assertFalse(
                    any(event.startswith("packer:") for event in events)
                )
                self.assertFalse(any(event.startswith("fetch|") for event in events))

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
        for template in ("nvke-node-fde.pkr.hcl", "nvke-node-non-fde.pkr.hcl"):
            with self.subTest(template=template):
                contents = (PACKER_DIR / template).read_text(encoding="utf-8")
                self.assertNotIn("fetch-artifacts.sh", contents)
                self.assertEqual(contents.count('provisioner "shell-local"'), 3)
                self.assertEqual(contents.count("scripts/prepare-image-inputs.sh"), 3)
                self.assertEqual(
                    contents.count('error-cleanup-provisioner "shell-local"'), 1
                )
                self.assertIn("generated   = true", contents)
                self.assertNotIn("/tmp/nvke-image-inputs.tar", contents)
                self.assertEqual(contents.count('"MODE=clean"'), 2)
                self.assertLess(
                    contents.index('"MODE=stage"'),
                    contents.index('provisioner "file"'),
                )
                self.assertLess(
                    contents.index('provisioner "file"'),
                    contents.rindex('"MODE=clean"'),
                )

    def test_existing_output_or_scratch_blocks_packer_without_destroying_state(self):
        cases = (
            ("os-image-fde", "output", "output-fde already exists"),
            ("os-image-non-fde", "output", "output-non-fde already exists"),
            ("os-image-fde", "scratch", "Packer output scratch already exists"),
            ("os-image-non-fde", "scratch", "Packer output scratch already exists"),
        )
        for target, state, message in cases:
            with self.subTest(target=target, state=state), tempfile.TemporaryDirectory() as tmp:
                result, events, artifacts, _ = self.run_image_build(
                    target,
                    "amd64",
                    Path(tmp),
                    preexisting_output=state == "output",
                    preexisting_scratch=state == "scratch",
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertFalse(any(event.startswith("packer:") for event in events))
                self.assertFalse(any(event.startswith("fetch|") for event in events))
                if state == "output":
                    variant = "fde" if target == "os-image-fde" else "non-fde"
                    self.assertEqual(
                        (artifacts.parent / f"output-{variant}" / "keep").read_text(encoding="utf-8"),
                        "existing output\n",
                    )
                else:
                    variant = "fde" if target == "os-image-fde" else "non-fde"
                    self.assertEqual(
                        (
                            artifacts.parent
                            / f".packer-output-{variant}"
                            / "keep"
                        ).read_text(encoding="utf-8"),
                        "existing scratch\n",
                    )

    def test_variant_cleanup_preserves_other_variant_scratch(self):
        cases = (
            ("os-image-fde", "non-fde"),
            ("os-image-non-fde", "fde"),
        )
        for target, other_variant in cases:
            with self.subTest(target=target), tempfile.TemporaryDirectory() as tmp:
                result, _, artifacts, _ = self.run_image_build(
                    target,
                    "amd64",
                    Path(tmp),
                    preexisting_other_scratch=True,
                )

                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(
                    (
                        artifacts.parent
                        / f".packer-output-{other_variant}"
                        / "keep"
                    ).read_text(encoding="utf-8"),
                    "other scratch\n",
                )

    def test_variant_clean_target_preserves_other_variant_scratch(self):
        cases = (
            ("clean-fde-output", "fde", "non-fde"),
            ("clean-non-fde-output", "non-fde", "fde"),
        )
        for target, variant, other_variant in cases:
            with self.subTest(target=target), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                workspace, _, _, _, _, _ = self.create_image_build_fixture(root)
                (workspace / f"output-{variant}").mkdir()
                (workspace / f".packer-output-{variant}").mkdir()
                other = workspace / f".packer-output-{other_variant}"
                other.mkdir()
                (other / "keep").write_text("other scratch\n", encoding="utf-8")

                result = self.run_make(
                    target,
                    cwd=workspace,
                    makefile=workspace / "Makefile",
                )

                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertFalse((workspace / f"output-{variant}").exists())
                self.assertFalse((workspace / f".packer-output-{variant}").exists())
                self.assertEqual(
                    (other / "keep").read_text(encoding="utf-8"),
                    "other scratch\n",
                )

    def test_material_verifier_accepts_only_matching_regular_file(self):
        verifier = PACKER_DIR / "scripts/verify-material.sh"
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            material = root / "image-inputs"
            material.write_bytes(b"authenticated material\n")
            digest = "sha256:" + hashlib.sha256(material.read_bytes()).hexdigest()

            accepted = subprocess.run(
                [verifier],
                env={
                    **os.environ,
                    "MATERIAL_PATH": str(material),
                    "EXPECTED_DIGEST": digest,
                },
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertEqual(accepted.returncode, 0, accepted.stderr)

            for name, path, expected in (
                ("mismatch", material, "sha256:" + "0" * 64),
                ("malformed", material, "sha256:not-a-digest"),
                ("symlink", root / "image-inputs-link", digest),
                ("missing", root / "missing", digest),
                ("directory", root / "directory", digest),
                ("uppercase", material, "sha256:" + hashlib.sha256(material.read_bytes()).hexdigest().upper()),
            ):
                with self.subTest(name=name):
                    if name == "symlink":
                        path.symlink_to(material.name)
                    elif name == "directory":
                        path.mkdir()
                    rejected = subprocess.run(
                        [verifier],
                        env={
                            **os.environ,
                            "MATERIAL_PATH": str(path),
                            "EXPECTED_DIGEST": expected,
                        },
                        check=False,
                        capture_output=True,
                        text=True,
                    )
                    self.assertNotEqual(rejected.returncode, 0)

    def test_image_input_staging_verifies_extracts_and_cleans(self):
        helper = PACKER_DIR / "scripts/prepare-image-inputs.sh"
        helper_contents = helper.read_text(encoding="utf-8")
        self.assertIn('tar -xf "${material_snapshot}"', helper_contents)
        self.assertNotIn('tar -xf "${material_path}"', helper_contents)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            payload = root / "payload"
            (payload / "debs").mkdir(parents=True)
            (payload / "debs/example.deb").write_bytes(b"package bytes\n")
            (payload / "wait-spiffe-socket.sh").write_bytes(b"script bytes\n")
            archive = root / "image-inputs"
            with tarfile.open(archive, "w") as image_inputs:
                image_inputs.add(payload, arcname=".")

            digest = "sha256:" + hashlib.sha256(archive.read_bytes()).hexdigest()
            rejected_staging = root / "output/non-fde/.image-inputs"
            rejected = subprocess.run(
                [helper],
                env={
                    **os.environ,
                    "MODE": "stage",
                    "MATERIAL_PATH": str(archive),
                    "EXPECTED_DIGEST": "sha256:" + "0" * 64,
                    "STAGING_DIRECTORY": str(rejected_staging),
                },
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(rejected.returncode, 0)
            self.assertFalse(rejected_staging.exists())

            staging = root / "output/fde/.image-inputs"
            staged = subprocess.run(
                [helper],
                env={
                    **os.environ,
                    "MODE": "stage",
                    "MATERIAL_PATH": str(archive),
                    "EXPECTED_DIGEST": digest,
                    "STAGING_DIRECTORY": str(staging),
                },
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertEqual(staged.returncode, 0, staged.stderr)
            self.assertEqual(
                (staging / "debs/example.deb").read_bytes(), b"package bytes\n"
            )
            self.assertEqual(
                (staging / "wait-spiffe-socket.sh").read_bytes(), b"script bytes\n"
            )
            self.assertEqual(list(staging.glob(".material.*")), [])

            cleaned = subprocess.run(
                [helper],
                env={
                    **os.environ,
                    "MODE": "clean",
                    "STAGING_DIRECTORY": str(staging),
                },
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertEqual(cleaned.returncode, 0, cleaned.stderr)
            self.assertFalse(staging.exists())

    def test_image_input_staging_rejects_unsafe_archive_members(self):
        helper = PACKER_DIR / "scripts/prepare-image-inputs.sh"
        cases = ("absolute", "traversal", "symlink", "hardlink", "fifo")
        for case in cases:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                archive = root / "image-inputs"
                with tarfile.open(archive, "w") as image_inputs:
                    member = tarfile.TarInfo("payload")
                    if case == "absolute":
                        member.name = "/host-path"
                    elif case == "traversal":
                        member.name = "../host-path"
                    elif case == "symlink":
                        member.type = tarfile.SYMTYPE
                        member.linkname = "/etc/passwd"
                    elif case == "hardlink":
                        member.type = tarfile.LNKTYPE
                        member.linkname = "payload"
                    else:
                        member.type = tarfile.FIFOTYPE
                    image_inputs.addfile(member)

                staging = root / "output/fde/.image-inputs"
                digest = "sha256:" + hashlib.sha256(archive.read_bytes()).hexdigest()
                result = subprocess.run(
                    [helper],
                    env={
                        **os.environ,
                        "PYTHON": sys.executable,
                        "MODE": "stage",
                        "MATERIAL_PATH": str(archive),
                        "EXPECTED_DIGEST": digest,
                        "STAGING_DIRECTORY": str(staging),
                    },
                    check=False,
                    capture_output=True,
                    text=True,
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertIn("unsafe archive", result.stderr)
                self.assertFalse(staging.exists())

    def test_packer_directory_project_has_one_explicit_shared_contract(self):
        project_files = tuple(sorted(PACKER_DIR.glob("*.pkr.hcl")))
        project = "\n".join(
            path.read_text(encoding="utf-8") for path in project_files
        )

        self.assertEqual(project.count("packer {"), 1)
        for variable in (
            "architecture",
            "material_manifest",
            "output_directory",
            "ssh_private_key_file",
            "ssh_public_key_file",
        ):
            self.assertEqual(
                project.count(f'variable "{variable}"'),
                1,
                f"{variable} must be declared once in the directory project",
            )
        self.assertRegex(
            project,
            r"architecture\s*=\s*local\.material_bound_architectures\[var\.architecture\]",
        )

        self.assertRegex(project, r'required_version\s*=\s*"= 1\.15\.4"')
        self.assertRegex(project, r'version\s*=\s*"= 1\.1\.6"')
        self.assertIn(
            "abspath(var.output_directory) == var.output_directory", project
        )
        self.assertEqual(
            project.count('can(regex("^sha256:[0-9a-f]{64}$", material.digest))'),
            2,
        )
        self.assertIn('source "qemu" "nvke-fde"', project)
        self.assertIn('source "qemu" "nvke-non-fde"', project)
        self.assertRegex(project, r'name\s*=\s*"nvke-node-fde"')
        self.assertRegex(project, r'name\s*=\s*"nvke-node-non-fde"')
        self.assertEqual(
            len(re.findall(r"ssh_private_key_file\s*=\s*var\.ssh_private_key_file", project)),
            2,
        )
        self.assertEqual(
            len(re.findall(r"ssh_public_key\s*=\s*local\.ssh_public_key", project)),
            2,
        )
        self.assertNotIn("{{ .SSHPublicKey }}", project)
        self.assertEqual(
            project.count(
                'script = "${local.project_root}/scripts/prepare-image-inputs.sh"'
            ),
            6,
        )
        self.assertEqual(len(re.findall(r"generated\s*=\s*true", project)), 2)
        self.assertNotIn("/tmp/nvke-image-inputs.tar", project)
        self.assertEqual(len(re.findall(r"skip_compaction\s*=\s*true", project)), 2)
        self.assertEqual(len(re.findall(r"disk_compression\s*=\s*false", project)), 2)
        self.assertEqual(len(re.findall(r"efi_drop_efivars\s*=\s*true", project)), 2)
        self.assertEqual(len(re.findall(r"memory\s*=\s*var\.guest_memory_mib", project)), 2)
        self.assertNotIn('${path.root}/artifacts', project)
        self.assertNotIn('${path.root}/.ssh', project)

    def test_workflow_validates_packer_before_scarce_runner_builds(self):
        workflow = (REPO_ROOT / ".github/workflows/os-image-artifact.yml").read_text(
            encoding="utf-8"
        )

        self.assertIn("  validate-packer:\n", workflow)
        self.assertIn("    runs-on: ubuntu-latest\n", workflow)
        self.assertIn("    needs: [matrix, validate-packer]\n", workflow)
        self.assertEqual(workflow.count("Validate the Packer directory project"), 1)
        self.assertIn(
            "          PACKER_GITHUB_API_TOKEN: ${{ secrets.GITHUB_TOKEN }}",
            workflow,
        )

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
            self.write_executable(tools / "uname", '#!/bin/sh\ncase "$1" in -s) echo Linux;; -m) echo x86_64;; esac\n')
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

    def test_ubuntu_release_selects_the_installer_iso_for_both_flavors(self):
        # Literal, not derived: 26.04's URLs are what consumers already pin, so
        # a change to the derivation must not move them quietly. The checksums
        # are the committed digests from make/tool-versions.mk, restated here
        # so a silent edit to a pin fails rather than reading as agreement.
        cases = (
            ("26.04.1", "amd64",
             "https://releases.ubuntu.com/26.04/ubuntu-26.04.1-live-server-amd64.iso",
             "sha256:cc8a95cde20f6ced61a322420de00f10cc3c90ced545daa46cb9c1a117f1d927"),
            ("26.04.1", "arm64",
             "https://cdimage.ubuntu.com/ubuntu/releases/26.04/release/ubuntu-26.04.1-live-server-arm64.iso",
             "sha256:af78753c94b924c85c4964c670692a853f7c80299a4064e317edad95462e3168"),
            ("24.04.5", "amd64",
             "https://releases.ubuntu.com/24.04/ubuntu-24.04.5-live-server-amd64.iso",
             "sha256:97f3d7ffb032c3eb3b23d2c8be9cc76e60c2c1f2c0146ba5ba9fe01cafae0fd8"),
            ("24.04.5", "arm64",
             "https://cdimage.ubuntu.com/ubuntu/releases/24.04/release/ubuntu-24.04.5-live-server-arm64.iso",
             "sha256:f12484464c7d4d73e2167e0a7dbda1e669f8fae9bcf778dc0944937034b320cb"),
        )
        for release, arch, iso_url, iso_checksum in cases:
            with self.subTest(release=release, arch=arch):
                variables = (f"UBUNTU_RELEASE={release}", f"ARCH={arch}")
                for name, expected in (
                    ("ISO_URL", iso_url),
                    ("ISO_CHECKSUM", iso_checksum),
                    ("UBUNTU_VERSION", release),
                ):
                    result = self.run_make(
                        f"print-{name}", cwd=PACKER_DIR, variables=variables
                    )
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout.strip(), expected)

    def test_iso_checksum_accepts_a_manifest_override_and_fails_closed_unpinned(self):
        # The committed digest is the default, but a locally served or
        # non-Canonical ISO still needs the SHA256SUMS-manifest form.
        manifest = "file:https://mirror.invalid/24.04/SHA256SUMS"
        result = self.run_make(
            "print-ISO_CHECKSUM",
            cwd=PACKER_DIR,
            variables=("UBUNTU_RELEASE=24.04.5", "ARCH=amd64", f"ISO_CHECKSUM={manifest}"),
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), manifest)

        # A release with no committed digest leaves nothing after "sha256:",
        # which prepare-installer-iso refuses rather than building unverified.
        unpinned = self.run_make(
            "print-ISO_CHECKSUM",
            cwd=PACKER_DIR,
            variables=("UBUNTU_RELEASE=24.04.6", "ARCH=amd64"),
        )
        self.assertEqual(unpinned.returncode, 0, unpinned.stderr)
        self.assertEqual(unpinned.stdout.strip(), "sha256:")

    def test_explicit_iso_url_still_names_the_image_over_ubuntu_release(self):
        # ISO_URL stays authoritative, so an override cannot produce an image
        # labelled as a release it is not.
        result = self.run_make(
            "print-UBUNTU_VERSION",
            cwd=PACKER_DIR,
            variables=(
                "UBUNTU_RELEASE=26.04.1",
                "ARCH=amd64",
                "ISO_URL=https://example.invalid/ubuntu-24.04.9-live-server-amd64.iso",
            ),
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "24.04.9")

    def test_collection_copies_exact_bytes_and_prints_one_basename(self):
        # Collection re-derives the expected filename from ISO_URL, so a 24.04
        # leg must pass the same UBUNTU_RELEASE or collection refuses.
        cases = (
            ("fde", "amd64", "26.04.1"),
            ("non-fde", "arm64", "26.04.1"),
            ("non-fde", "amd64", "24.04.5"),
        )
        for variant, arch, release in cases:
            with self.subTest(variant=variant, arch=arch, release=release), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                source = root / "source"
                workspace = root / "workspace"
                workspace.mkdir()
                commit = self.create_source_repo(source)
                output = workspace / f"output-{variant}"
                output.mkdir()
                name = f"nvke-node-{variant}-ubuntu-{release}-{arch}.qcow2"
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
                        f"UBUNTU_RELEASE={release}",
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
                expected = output / "nvke-node-fde-ubuntu-26.04.1-amd64.qcow2"
                if case == "multiple":
                    expected.write_bytes(b"one")
                    (output / "extra.qcow2").write_bytes(b"two")
                elif case == "wrong-name":
                    (output / "nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2").write_bytes(b"wrong")
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
            name = "nvke-node-fde-ubuntu-26.04.1-amd64.qcow2"
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
            name = "nvke-node-fde-ubuntu-26.04.1-amd64.qcow2"
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

    def test_cni_dockerhub_mirror_changes_transport_not_image_identity(self):
        bundle_lib = REPO_ROOT / "os-image/packages/cni-common/bundle-lib.sh"
        script = """
set -euo pipefail
source "$1"
CNI_DOCKERHUB_MIRROR=dc.local.gha-runners.nvidia.com
image_transport_ref docker.io/calico/node:v3.30.1
image_transport_ref quay.io/tigera/operator:v1.38.1
unset CNI_DOCKERHUB_MIRROR
image_transport_ref docker.io/calico/node:v3.30.1
"""

        result = subprocess.run(
            ["bash", "-c", script, "bundle-lib-test", str(bundle_lib)],
            check=True,
            capture_output=True,
            text=True,
        )

        self.assertEqual(
            result.stdout.splitlines(),
            [
                "dc.local.gha-runners.nvidia.com/calico/node:v3.30.1",
                "quay.io/tigera/operator:v1.38.1",
                "docker.io/calico/node:v3.30.1",
            ],
        )

    def test_cni_dockerhub_mirror_falls_back_for_resolution_and_transfer(self):
        bundle_lib = REPO_ROOT / "os-image/packages/cni-common/bundle-lib.sh"
        digest = "sha256:" + "1" * 64
        bash_candidates = (
            "/opt/homebrew/bin/bash",
            "/usr/local/bin/bash",
            shutil.which("bash"),
        )
        modern_bash = next(
            (
                candidate
                for candidate in bash_candidates
                if candidate
                and Path(candidate).is_file()
                and subprocess.run(
                    [candidate, "-c", "(( BASH_VERSINFO[0] >= 4 ))"],
                    check=False,
                ).returncode
                == 0
            ),
            None,
        )
        if modern_bash is None:
            self.skipTest("Bash 4 or newer is required for CNI bundle tests")
        # `make lock` resolves the tag through the mirror, with retry and
        # fallback. A build never resolves a tag to decide what to fetch: it
        # probes the mirror once for the locked digest, and reads the tag only
        # for the moved-tag notice.
        cases = (
            ("lock: digest miss", "--write-lock", "1", "0", "0", 3, 0, 0, True),
            ("lock: transfer miss", "--write-lock", "0", "1", "0", 1, 3, 0, True),
            ("build: mirror serves digest", "", "0", "0", "0", 1, 1, 1, False),
            ("build: mirror lacks digest", "", "0", "0", "1", 1, 0, 1, True),
        )
        for (
            name,
            lock_flag,
            digest_fails,
            pull_fails,
            manifest_fails,
            mirror_digests,
            mirror_pulls,
            mirror_manifests,
            canonical_pull,
        ) in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                lock = root / "locks/amd64/v1.lock"
                lock.parent.mkdir(parents=True)
                lock.write_text(
                    f"# test lock\ntest v1\nimage docker.io/calico/node:v1 {digest}\n",
                    encoding="utf-8",
                )
                crane = root / "crane"
                self.write_executable(
                    crane,
                    """#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> "${CRANE_LOG:?}"
operation="$1"
shift
case "$operation" in
  digest)
    ref="${@: -1}"
    if [[ "$ref" == mirror.gcr.io/* && "${MIRROR_DIGEST_FAIL:-0}" == 1 ]]; then
      exit 1
    fi
    printf '%s\\n' "${LOCKED_DIGEST:?}"
    ;;
  manifest)
    ref="${@: -1}"
    if [[ "$ref" == mirror.gcr.io/* && "${MIRROR_MANIFEST_FAIL:-0}" == 1 ]]; then
      exit 1
    fi
    printf '{}\\n'
    ;;
  pull)
    output="${@: -1}"
    if [[ "$*" == *mirror.gcr.io/* && "${MIRROR_PULL_FAIL:-0}" == 1 ]]; then
      exit 1
    fi
    if [[ "$*" == *mirror.gcr.io/* ]]; then
      repo=mirror.gcr.io/calico/node
    else
      repo=docker.io/calico/node
    fi
    temporary="$(mktemp -d)"
    printf '[{"RepoTags":["%s:i-was-a-digest"]}]\\n' "$repo" >"${temporary}/manifest.json"
    tar -C "$temporary" -cf "$output" manifest.json
    rm -rf -- "$temporary"
    ;;
  *) exit 2 ;;
esac
""",
                )
                calls = root / "crane.log"
                script = """
set -euo pipefail
source "$1"
sleep() { :; }
if [[ "$(uname -s)" == Darwin ]]; then
  sed() {
    if [[ "$1" == -i ]]; then
      command sed -i '' "$2" "$3"
    else
      command sed "$@"
    fi
  }
  tar() {
    if [[ "$1" == --format=gnu ]]; then
      command tar -C "$8" -cf "${10}" -T "${12}"
    else
      command tar "$@"
    fi
  }
fi
CNI=cni-test
CNI_DOCKERHUB_MIRROR=mirror.gcr.io
parse_bundle_args --version v1 --dist "$2/dist" --lock-dir "$2/locks" --crane "$3" --arch amd64 ${4:+"$4"}
stage_images
"""

                result = subprocess.run(
                    [
                        modern_bash,
                        "-c",
                        script,
                        "bundle-lib-test",
                        str(bundle_lib),
                        str(root),
                        str(crane),
                        lock_flag,
                    ],
                    env={
                        **os.environ,
                        "CRANE_LOG": str(calls),
                        "LOCKED_DIGEST": digest,
                        "MIRROR_DIGEST_FAIL": digest_fails,
                        "MIRROR_PULL_FAIL": pull_fails,
                        "MIRROR_MANIFEST_FAIL": manifest_fails,
                    },
                    check=False,
                    capture_output=True,
                    text=True,
                )

                self.assertEqual(result.returncode, 0, result.stderr)
                call_lines = calls.read_text(encoding="utf-8").splitlines()
                self.assertEqual(
                    sum(
                        "digest --platform linux/amd64 mirror.gcr.io/" in line
                        for line in call_lines
                    ),
                    mirror_digests,
                )
                self.assertEqual(
                    sum(
                        line.startswith("pull ") and "mirror.gcr.io/" in line
                        for line in call_lines
                    ),
                    mirror_pulls,
                )
                self.assertEqual(
                    sum(
                        line.startswith("manifest ") and "mirror.gcr.io/" in line
                        for line in call_lines
                    ),
                    mirror_manifests,
                )
                if not lock_flag:
                    # A build pins from the lock: no tag is resolved to decide
                    # what to fetch, and every pull names a digest.
                    self.assertTrue(
                        all(
                            f"@{digest}" in line
                            for line in call_lines
                            if line.startswith("pull ")
                        )
                    )
                # The canonical reference carries the transfer whenever the
                # mirror did not; when the mirror served it, nothing should
                # have reached docker.io.
                self.assertEqual(
                    any(
                        line.startswith("pull ") and "docker.io/" in line
                        for line in call_lines
                    ),
                    canonical_pull,
                )
                with tarfile.open(root / "dist/v1/images.tgz", "r:gz") as archive:
                    manifest_member = next(
                        member
                        for member in archive.getmembers()
                        if member.name in ("manifest.json", "./manifest.json")
                    )
                    manifest_file = archive.extractfile(manifest_member)
                    self.assertIsNotNone(manifest_file)
                    manifest = json.load(manifest_file)
                self.assertEqual(
                    manifest[0]["RepoTags"],
                    ["docker.io/calico/node:v1"],
                )


if __name__ == "__main__":
    unittest.main()
