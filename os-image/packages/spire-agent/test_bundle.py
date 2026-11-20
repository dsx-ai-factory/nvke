# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise local bundles, optional IMDS packages, and installation behavior."""

import hashlib
import io
import os
from pathlib import Path
import shutil
import struct
import subprocess
import tarfile
import tempfile
import unittest

PACKAGE = Path(__file__).resolve().parent
FILES = PACKAGE.parents[1] / "files"
IMDS = "dsx-imds-node-attestor-agent"
VERSION = "1.14.1+tpm1.9.0"


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def elf(arch):
    """Minimal ELF fixture for header validation; never executed."""
    return (b"\x7fELF\x02\x01\x01" + bytes(9) +
            struct.pack("<HH", 2, {"amd64": 62, "arm64": 183}[arch]) + bytes(44))


def deb_members(path):
    """Read nfpm's ar container without requiring host Debian tools."""
    raw = path.read_bytes()
    assert raw[:8] == b"!<arch>\n"
    offset = 8
    result = {}
    while offset < len(raw):
        header = raw[offset:offset + 60]
        size = int(header[48:58])
        name = header[:16].decode().strip().rstrip("/")
        result[name] = raw[offset + 60:offset + 60 + size]
        offset += 60 + size + size % 2
    return result


class BundleTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.locks = self.root / "lock"
        self.locks.mkdir()
        rows = []
        for arch in ("amd64", "arm64"):
            for component in ("spire-agent", "tpm_attestor_agent", "get_tpm_pubhash"):
                if component == "spire-agent":
                    name = f"spire-1.14.1-linux-{arch}-musl.tar.gz"
                    release = self.root / "spiffe/spire/releases/download/v1.14.1"
                    member = "spire-1.14.1/bin/spire-agent"
                else:
                    name = f"spire_tpm_plugin_{component}_linux_{arch}_v1.9.0.tar.gz"
                    release = self.root / "spiffe/spire-tpm-plugin/releases/download/v1.9.0"
                    member = component
                release.mkdir(parents=True, exist_ok=True)
                archive = release / name
                payload = f"{component}/{arch}\n".encode()
                with tarfile.open(archive, "w:gz") as tar:
                    info = tarfile.TarInfo(member)
                    info.size = len(payload)
                    tar.addfile(info, io.BytesIO(payload))
                sha = digest(archive)
                if component == "spire-agent":
                    checksum = f"spire-1.14.1-linux-{arch}-musl_sha256sum.txt"
                    (release / checksum).write_text(f"{sha}  {name}\n")
                rows.append(f"artifact {component} {arch} {sha} {archive.as_uri()}\n")
        (self.locks / f"{VERSION}.lock").write_text("".join(rows))

    def bundle(self, arch, *args):
        return subprocess.run(
            ["bash", str(PACKAGE / "bundle.sh"), "--spire-version", "1.14.1",
             "--tpm-version", "1.9.0", "--arch", arch,
             "--dist", str(self.root / "dist"), "--lock-dir", str(self.locks), *args],
            env={**os.environ, "BUNDLE_GITHUB_URL": self.root.as_uri()},
            capture_output=True, text=True, check=False,
        )

    def input(self, arch):
        path = self.root / f"imds-{arch}"
        path.write_bytes(elf(arch))
        return ["--imds-agent-file", str(path), "--imds-agent-sha256", digest(path),
                "--imds-agent-arch", arch]

    def staged(self, arch):
        return self.root / "dist" / VERSION / arch

    def test_cold_build_stages_each_architecture(self):
        for arch in ("amd64", "arm64"):
            with self.subTest(arch=arch):
                result = self.bundle(arch)
                self.assertEqual(result.returncode, 0, result.stderr)
                for component in ("spire-agent", "tpm_attestor_agent", "get_tpm_pubhash"):
                    binary = self.staged(arch) / component
                    self.assertEqual(binary.read_text(), f"{component}/{arch}\n")
                    self.assertTrue(os.access(binary, os.X_OK))
                self.assertFalse((self.staged(arch) / IMDS).exists())

    def test_optional_input_and_removal_from_reused_bundle(self):
        for arch in ("amd64", "arm64"):
            with self.subTest(arch=arch):
                result = self.bundle(arch, *self.input(arch))
                self.assertEqual(result.returncode, 0, result.stderr)
                binary = self.staged(arch) / IMDS
                self.assertEqual(binary.read_bytes(), elf(arch))
                self.assertEqual(binary.stat().st_mode & 0o777, 0o755)
                self.assertEqual(binary.with_suffix(".sha256").read_text().strip(), digest(binary))
                result = self.bundle(arch)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertFalse(binary.exists())
                self.assertFalse(binary.with_suffix(".sha256").exists())

    def test_rejects_bad_inputs_before_staging(self):
        for case in ("digest", "declared_arch", "elf_arch", "missing", "incomplete", "non_elf"):
            with self.subTest(case=case):
                args = self.input("amd64")
                expected = ""
                if case == "digest":
                    args[3] = "0" * 64
                    expected = "SHA-256 mismatch"
                elif case == "declared_arch":
                    args[5] = "arm64"
                    expected = "does not match target"
                elif case in ("elf_arch", "non_elf"):
                    path = Path(args[1])
                    path.write_bytes(elf("arm64") if case == "elf_arch" else b"#!/bin/sh\n")
                    args[3] = digest(path)
                    expected = "ELF64 executable"
                elif case == "missing":
                    Path(args[1]).unlink()
                    expected = "missing regular IMDS input"
                else:
                    args = args[:2]
                    expected = "requires --imds-agent-file"
                result = self.bundle("amd64", *args)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected, result.stderr)
                self.assertFalse(self.staged("amd64").exists())

    @unittest.skipUnless(os.environ.get("NFPM_BIN"), "set NFPM_BIN to inspect real .deb output")
    def test_packages_both_architectures_with_and_without_imds(self):
        # Copy the source-owned layout so maintainer-script staging stays out
        # of the checkout. Packaging itself is real, not a mocked nfpm call.
        package = self.root / "source/os-image/packages/spire-agent"
        shutil.copytree(PACKAGE / "packaging", package / "packaging")
        shutil.copy(PACKAGE / "verify-imds.sh", package / "verify-imds.sh")
        shutil.copytree(FILES, package.parents[1] / "files")
        env = {**os.environ, "SPIRE_VERSION": "1.14.1", "TPM_PLUGIN_VERSION": "1.9.0",
               "DEB_REVISION": "5"}
        for arch in ("amd64", "arm64"):
            for present in (True, False):
                with self.subTest(arch=arch, present=present):
                    args = self.input(arch) if present else []
                    result = self.bundle(arch, *args)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    output = self.root / f"packages-{arch}-{present}"
                    command = ["bash", str(package / "packaging/build-deb.sh"), arch,
                               str(self.staged(arch)), str(output)]
                    result = subprocess.run(command, env=env, capture_output=True, text=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    members = deb_members(next(output.glob("*.deb")))
                    with tarfile.open(fileobj=io.BytesIO(members["control.tar.gz"])) as tar:
                        control = tar.extractfile("./control").read().decode()
                        self.assertIn(f"Architecture: {arch}\n", control)
                        self.assertIn(f"Version: {VERSION}-5\n", control)
                    with tarfile.open(fileobj=io.BytesIO(members["data.tar.gz"])) as tar:
                        files = {m.name.removeprefix("./"): m for m in tar.getmembers()}
                        name = f"usr/local/lib/spire-agent/{IMDS}"
                        self.assertEqual(name in files, present)
                        self.assertIn("usr/lib/nvke/spire-check-imds.sh", files)
                        if present:
                            self.assertEqual(files[name].mode, 0o755)
                            self.assertEqual((files[name].uid, files[name].gid), (0, 0))
                            self.assertEqual(tar.extractfile(files[name]).read(), elf(arch))
                    if present:
                        (self.staged(arch) / IMDS).write_bytes(b"tampered")
                        result = subprocess.run(command, env=env, capture_output=True, text=True)
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn("SHA-256 mismatch", result.stderr)


class InstalledTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.tools = self.root / "tools"
        self.tools.mkdir()
        for command in ("id", "install", "systemctl", "getent"):
            tool = self.tools / command
            tool.write_text("#!/bin/sh\n" + ("exit 1\n" if command == "getent" else "exit 0\n"))
            tool.chmod(0o755)
        for directory in ("etc/spire-agent", "usr/local/lib/spire-agent"):
            (self.root / directory).mkdir(parents=True)
        (self.root / "usr/local/lib/spire-agent/tpm_attestor_agent").write_bytes(b"TPM")
        self.envfile = self.root / "etc/spire-agent/spire-agent.env"

    def run_script(self, path, *args):
        # Redirect absolute node paths only in the disposable copy; never run
        # postinst against the host's /etc or system users.
        script = self.root / path.name
        source = path.read_text()
        for prefix in ("/etc/", "/usr/local/", "/var/lib/"):
            source = source.replace(prefix, f"{self.root}{prefix}")
        script.write_text(source)
        return subprocess.run(["sh", str(script), *args], capture_output=True, text=True,
                              env={**os.environ, "PATH": f"{self.tools}:{os.environ['PATH']}"})

    def test_revision4_upgrade_and_checksum_refresh(self):
        # dpkg configure receives the old revision as its second argument.
        self.envfile.write_text("TPM_PLUGIN_CHECKSUM=old-revision4-value\n")
        binary = self.root / f"usr/local/lib/spire-agent/{IMDS}"
        for payload in (elf("amd64"), elf("arm64"), None):
            with self.subTest(payload=payload):
                if payload is None:
                    binary.unlink()
                else:
                    binary.write_bytes(payload)
                result = self.run_script(PACKAGE / "packaging/postinst", "configure", f"{VERSION}-4")
                self.assertEqual(result.returncode, 0, result.stderr)
                values = dict(line.split("=", 1) for line in self.envfile.read_text().splitlines())
                self.assertEqual(values["TPM_PLUGIN_CHECKSUM"], hashlib.sha256(b"TPM").hexdigest())
                self.assertEqual("DSX_IMDS_PLUGIN_CHECKSUM" in values, payload is not None)
                if payload:
                    self.assertEqual(values["DSX_IMDS_PLUGIN_CHECKSUM"], digest(binary))
                self.assertEqual(self.envfile.stat().st_mode & 0o777, 0o644)
        service = (FILES / "spire-agent.service").read_text()
        self.assertIn("EnvironmentFile=-/etc/spire-agent/spire-agent.env", service)
        self.assertIn("-config /etc/spire-agent/agent.conf -expandEnv", service)

    def test_selected_imds_requires_executable_without_affecting_other_attestors(self):
        binary = self.root / f"usr/local/lib/spire-agent/{IMDS}"
        config = self.root / "etc/spire-agent/agent.conf"
        for attestor in ("tpm", "join_token", "dsx_imds"):
            config.write_text(f'plugins {{\n  NodeAttestor "{attestor}" {{\n  }}\n}}\n')
            result = self.run_script(FILES / "spire-check-imds.sh")
            self.assertEqual(result.returncode, int(attestor == "dsx_imds"), result.stderr)
            if attestor == "dsx_imds":
                self.assertIn("dsx_imds selected", result.stderr)
        binary.write_bytes(elf("amd64"))
        binary.chmod(0o644)
        self.assertNotEqual(self.run_script(FILES / "spire-check-imds.sh").returncode, 0)
        binary.chmod(0o755)
        self.assertEqual(self.run_script(FILES / "spire-check-imds.sh").returncode, 0)


if __name__ == "__main__":
    unittest.main()
