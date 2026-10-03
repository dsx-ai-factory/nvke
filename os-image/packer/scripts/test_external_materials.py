# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the source-owned external-runner material adapters."""

import hashlib
import importlib.util
import io
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest


SCRIPTS = Path(__file__).resolve().parent


def write_executable(path: Path, content: str) -> None:
    path.write_text(content, encoding="utf-8")
    path.chmod(0o755)


def write_package_archive(path: Path, package_name: str) -> None:
    payload = f"package:{package_name}\n".encode()
    with tarfile.open(path, "w") as archive:
        member = tarfile.TarInfo(package_name)
        member.mode = 0o644
        member.size = len(payload)
        archive.addfile(member, io.BytesIO(payload))


class ComposeImageInputsTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.materials = self.root / "materials"
        self.source_files = self.root / "source-files"
        self.tools = self.root / "tools"
        for directory in (self.materials, self.source_files, self.tools):
            directory.mkdir()
        (self.materials / "agent").write_bytes(b"agent-deb\n")
        for binding in ("calico", "kubeadm-distro", "ovn", "spire-agent"):
            write_package_archive(self.materials / binding, f"{binding}.deb")
        (self.source_files / "nke-agent-spire.conf").write_text("override\n", encoding="utf-8")
        (self.source_files / "wait-spiffe-socket.sh").write_text("#!/bin/sh\n", encoding="utf-8")
        write_executable(
            self.tools / "dpkg-scanpackages",
            "#!/bin/sh\nprintf 'Package: fixture\\n'\n",
        )

    def tearDown(self):
        self.temporary.cleanup()

    def run_compose(self, output: Path, umask: int = -1):
        environment = os.environ.copy()
        environment["PATH"] = f"{self.tools}:{environment['PATH']}"
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPTS / "compose-image-inputs.py"),
                "--materials-dir",
                str(self.materials),
                "--source-files-dir",
                str(self.source_files),
                "--output",
                str(output),
                "--source-date-epoch",
                "1787932620",
            ],
            env=environment,
            check=False,
            capture_output=True,
            text=True,
            umask=umask,
        )

    def test_composes_deterministic_archive(self):
        first = self.root / "first.tar"
        second = self.root / "second.tar"
        first_result = self.run_compose(first)
        second_result = self.run_compose(second)
        self.assertEqual(first_result.returncode, 0, first_result.stderr)
        self.assertEqual(second_result.returncode, 0, second_result.stderr)
        self.assertEqual(
            hashlib.sha256(first.read_bytes()).digest(),
            hashlib.sha256(second.read_bytes()).digest(),
        )
        with tarfile.open(first) as archive:
            names = {member.name.removeprefix("./") for member in archive.getmembers()}
        self.assertEqual(
            names,
            {
                ".",
                "debs",
                "debs/Packages",
                "debs/calico.deb",
                "debs/kubeadm-distro.deb",
                "debs/ovn.deb",
                "debs/spire-agent.deb",
                "nke-agent-spire.conf",
                "nke-agent.deb",
                "wait-spiffe-socket.sh",
            },
        )

    def test_directory_modes_are_independent_of_umask(self):
        outputs = [self.root / "permissive.tar", self.root / "restrictive.tar"]
        for output, umask in zip(outputs, (0o022, 0o077)):
            with self.subTest(umask=umask):
                result = self.run_compose(output, umask=umask)
                self.assertEqual(result.returncode, 0, result.stderr)
                with tarfile.open(output) as archive:
                    for member in archive.getmembers():
                        if member.isdir():
                            self.assertEqual(member.mode, 0o755)
        self.assertEqual(outputs[0].read_bytes(), outputs[1].read_bytes())

    def test_index_requests_multiversion(self):
        write_executable(
            self.tools / "dpkg-scanpackages",
            "#!/bin/sh\n"
            "test \"$#\" = 3 && test \"$1\" = --multiversion || exit 1\n"
            "printf 'Package: fixture\\n'\n",
        )
        result = self.run_compose(self.root / "multiversion.tar")
        self.assertEqual(result.returncode, 0, result.stderr)

    @unittest.skipUnless(
        shutil.which("dpkg-deb") and shutil.which("dpkg-scanpackages"),
        "real Debian package tools are required",
    )
    def test_real_index_covers_multiple_versions(self):
        (self.tools / "dpkg-scanpackages").unlink()
        for number, binding in enumerate(("calico", "kubeadm-distro", "ovn", "spire-agent"), 1):
            name = "fixture-shared" if number <= 2 else f"fixture-{binding}"
            package = self.root / f"package-{number}"
            control = package / "DEBIAN"
            control.mkdir(parents=True)
            (control / "control").write_text(
                f"Package: {name}\nVersion: {number}.0\nArchitecture: all\n"
                "Maintainer: Fixture <fixture@example.invalid>\n"
                "Description: External material adapter test\n",
                encoding="utf-8",
            )
            deb = self.root / f"{name}_{number}.0_all.deb"
            subprocess.run(
                ["dpkg-deb", "--build", "--root-owner-group", str(package), str(deb)],
                check=True, capture_output=True,
            )
            with tarfile.open(self.materials / binding, "w") as archive:
                archive.add(deb, arcname=deb.name)
        output = self.root / "real-index.tar"
        result = self.run_compose(output)
        self.assertEqual(result.returncode, 0, result.stderr)
        with tarfile.open(output) as archive:
            index = archive.extractfile("debs/Packages").read().decode()
        self.assertEqual(index.count("Package: fixture-shared\n"), 2)
        for number in (1, 2):
            self.assertIn(f"Filename: ./fixture-shared_{number}.0_all.deb\n", index)

    def test_rejects_duplicate_package_names(self):
        write_package_archive(self.materials / "ovn", "calico.deb")
        result = self.run_compose(self.root / "duplicate.tar")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("duplicate Debian package filename", result.stderr)

    def test_rejects_archive_links(self):
        with tarfile.open(self.materials / "ovn", "w") as archive:
            member = tarfile.TarInfo("ovn.deb")
            member.type = tarfile.SYMTYPE
            member.linkname = "/etc/passwd"
            archive.addfile(member)
        result = self.run_compose(self.root / "unsafe.tar")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsafe or unexpected member", result.stderr)

    def test_rejects_oversized_member_before_reading_payload(self):
        with (self.materials / "ovn").open("wb") as archive:
            member = tarfile.TarInfo("large.deb")
            member.size = (1 << 30) + 1
            archive.write(member.tobuf())
        output = self.root / "oversized.tar"
        result = self.run_compose(output)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("count or size budget", result.stderr)
        self.assertFalse(output.exists())

    def test_streaming_stage_checks_all_budgets(self):
        spec = importlib.util.spec_from_file_location("compose_image_inputs", SCRIPTS / "compose-image-inputs.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        for limit, value, total, expected in (
            ("MAX_ARCHIVE_MEMBERS", 0, 0, "member budget"),
            ("MAX_PACKAGES", 0, 0, "count or size budget"),
            ("MAX_TOTAL_PACKAGE_BYTES", 1, 0, "total package byte budget"),
        ):
            with self.subTest(limit=limit):
                previous = getattr(module, limit)
                setattr(module, limit, value)
                stage = self.root / limit
                stage.mkdir()
                try:
                    with self.assertRaisesRegex(SystemExit, expected):
                        module.stage_archive_debs(self.materials / "calico", stage, set(), total)
                    self.assertEqual(list(stage.iterdir()), [])
                finally:
                    setattr(module, limit, previous)


class PatchInstallerISOTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.source = self.root / "source.iso"
        self.source.write_bytes(b"source-iso\n")
        write_executable(
            self.tools / "7z",
            """#!/bin/sh
set -eu
for argument in "$@"; do
  case "$argument" in -o*) output=${argument#-o};; esac
done
mkdir -p "$output"
printf 'set timeout=30\nmenuentry fixture {}\n' >"$output/grub.cfg"
""",
        )
        write_executable(
            self.tools / "xorriso",
            """#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
  case "$1" in
    -indev) input=$2; shift 2;;
    -outdev) output=$2; shift 2;;
    *) shift;;
  esac
done
cp "$input" "$output"
printf 'patched\n' >>"$output"
""",
        )

    def tearDown(self):
        self.temporary.cleanup()

    def run_patch(self, source: Path, output: Path):
        environment = os.environ.copy()
        environment["PATH"] = f"{self.tools}:{environment['PATH']}"
        return subprocess.run(
            [str(SCRIPTS / "patch-installer-iso.sh"), str(source), str(output)],
            env=environment,
            check=False,
            capture_output=True,
            text=True,
        )

    def test_patches_caller_supplied_iso(self):
        output = self.root / "output" / "installer.iso"
        result = self.run_patch(self.source, output)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output.read_bytes(), b"source-iso\npatched\n")

    def test_rejects_source_symlink(self):
        source_link = self.root / "source-link.iso"
        source_link.symlink_to(self.source)
        result = self.run_patch(source_link, self.root / "installer.iso")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("source is not one regular file", result.stderr)

    @unittest.skipUnless(
        shutil.which("7z") and shutil.which("xorriso"),
        "real ISO tools are required",
    )
    def test_real_iso_grub_transform(self):
        (self.tools / "7z").unlink()
        (self.tools / "xorriso").unlink()
        source_tree = self.root / "iso-tree"
        grub = source_tree / "boot" / "grub"
        grub.mkdir(parents=True)
        (grub / "grub.cfg").write_text("set timeout=30\nmenuentry fixture {}\n", encoding="utf-8")
        source = self.root / "real-source.iso"
        subprocess.run(
            ["xorriso", "-as", "mkisofs", "-o", str(source), str(source_tree)],
            check=True, capture_output=True, timeout=30,
        )
        output = self.root / "real-patched.iso"
        result = self.run_patch(source, output)
        self.assertEqual(result.returncode, 0, result.stderr)
        extracted = subprocess.run(
            ["7z", "e", "-so", str(output), "boot/grub/grub.cfg"],
            check=True, capture_output=True, timeout=30,
        ).stdout
        self.assertEqual(extracted, b"set timeout=300\nmenuentry fixture {}\n")


if __name__ == "__main__":
    unittest.main()
