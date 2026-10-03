# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Contract tests for source-owned APT archive capture (not real bake proof)."""

import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


SCRIPTS = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("capture_apt", SCRIPTS / "capture-apt-acquisitions.py")
CAPTURE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CAPTURE)


class CaptureAPTTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.captured = self.root / "captured"
        (self.captured / "packages").mkdir(parents=True)
        self.payload = b"fake package for contract tests, not real Debian bytes\n"
        self.digest = hashlib.sha256(self.payload).hexdigest()
        self.package = self.captured / "packages" / f"{self.digest}.deb"
        self.package.write_bytes(self.payload)
        self.output = self.root / "capture.tar"

    def cache(self, architecture="amd64", **changes):
        values = dict(version="1:2.0-1", architecture=architecture, sha256=self.digest,
                      size=len(self.payload), uris=["https://archive.example.invalid/pool/fixture.deb"])
        values.update(changes)
        return {f"fixture:{architecture}": SimpleNamespace(versions=[SimpleNamespace(**values)])}

    def capture(self, architecture="amd64", cache=None):
        with patch.object(CAPTURE, "package_metadata", return_value=["fixture", "1:2.0-1", architecture]):
            return CAPTURE.capture(self.captured, self.output, architecture, cache or self.cache(architecture))

    def test_both_architectures_and_deterministic_archive(self):
        for architecture in ("amd64", "arm64"):
            with self.subTest(architecture=architecture):
                self.capture(architecture)
                first = self.output.read_bytes()
                with tarfile.open(self.output) as archive:
                    self.assertEqual(archive.getnames(), ["manifest.json", f"packages/{self.digest}.deb"])
                    metadata = json.load(archive.extractfile("manifest.json"))
                    self.assertEqual(metadata["architecture"], architecture)
                    self.assertEqual(metadata["scope"], "apt-pre-install-pkgs")
                    self.assertEqual(archive.extractfile(f"packages/{self.digest}.deb").read(), self.payload)
                self.output.unlink()
                self.capture(architecture)
                self.assertEqual(self.output.read_bytes(), first)
                self.output.unlink()

    def test_bad_captures_produce_no_output(self):
        for name, changes in (
            ("digest mismatch", {"sha256": "0" * 64}),
            ("size mismatch", {"size": 1}),
            ("version mismatch", {"version": "unrelated"}),
            ("URI credentials", {"uris": ["https://secret:token@example.invalid/package.deb"]}),
            ("URI query", {"uris": ["https://example.invalid/package.deb?token=secret"]}),
            ("missing URI", {"uris": []}),
        ):
            with self.subTest(name=name):
                with self.assertRaises(ValueError):
                    self.capture(cache=self.cache(**changes))
                self.assertFalse(self.output.exists())
                self.assertFalse(list(self.root.glob(".apt-capture-*")))

    def test_rejects_wrong_name_links_extra_files_and_empty_set(self):
        self.package.write_bytes(b"substitution")
        with self.assertRaises(ValueError):
            self.capture()
        self.package.unlink()
        with self.assertRaises(ValueError):
            self.capture()
        self.package.symlink_to(self.root / "outside")
        with self.assertRaises(OSError):
            self.capture()
        self.package.unlink()
        self.package.write_bytes(self.payload)
        (self.captured / "extra").write_text("unaccounted")
        with self.assertRaises(ValueError):
            self.capture()
        self.assertFalse(self.output.exists())

    def test_existing_output_is_not_overwritten(self):
        self.output.write_bytes(b"original")
        with self.assertRaises(FileExistsError):
            self.capture()
        self.assertEqual(self.output.read_bytes(), b"original")
        self.assertFalse(list(self.root.glob(".apt-capture-*")))

    def test_capture_limits_fail_before_output(self):
        for limit, value in (("MAX_PACKAGE_BYTES", 1), ("MAX_TOTAL_BYTES", 1), ("MAX_PACKAGES", 0)):
            with self.subTest(limit=limit), patch.object(CAPTURE, limit, value):
                with self.assertRaises(ValueError):
                    self.capture()
                self.assertFalse(self.output.exists())
                self.assertFalse(list(self.root.glob(".apt-capture-*")))

    def test_hashes_exported_bytes_not_reverted_source(self):
        addfile = tarfile.TarFile.addfile

        def corrupt(archive, header, source):
            if header.name.startswith("packages/"):
                source = io.BytesIO(b"x" * header.size)
            return addfile(archive, header, source)

        with patch.object(tarfile.TarFile, "addfile", corrupt):
            with self.assertRaises(ValueError):
                self.capture()
        self.assertEqual(self.package.read_bytes(), self.payload)
        self.assertFalse(self.output.exists())
        self.assertFalse(list(self.root.glob(".apt-capture-*")))

    def test_failed_publication_sync_removes_own_output(self):
        fsync = os.fsync
        calls = 0

        def fail_directory_sync(descriptor):
            nonlocal calls
            calls += 1
            if calls == 2:
                raise OSError("injected publication directory sync failure")
            return fsync(descriptor)

        with patch.object(CAPTURE.os, "fsync", fail_directory_sync):
            with self.assertRaises(OSError):
                self.capture()
        self.assertFalse(self.output.exists())
        self.assertFalse(list(self.root.glob(".apt-capture-*")))

    def test_capture_names_are_not_authority_and_duplicate_bytes_are_deduplicated(self):
        self.package.rename(self.package.with_name("fixture_1_amd64.deb"))
        (self.captured / "packages" / "fixture_1_amd64.deb.~1~").write_bytes(self.payload)
        self.capture()
        with tarfile.open(self.output) as archive:
            self.assertEqual(archive.getnames(), ["manifest.json", f"packages/{self.digest}.deb"])

    @unittest.skipUnless(os.environ.get("NKE_TEST_APT_CONFIG"), "native APT is opt-in")
    def test_native_apt_parses_capture_hook(self):
        result = subprocess.run([os.environ["NKE_TEST_APT_CONFIG"], "-c",
                                 str(SCRIPTS.parent / "cidata" / "apt-capture.conf"), "dump"],
                                check=True, capture_output=True, text=True, timeout=30)
        self.assertIn("DPkg::Pre-Install-Pkgs::nke-capture", result.stdout)

    def test_both_packer_variants_export_before_ssh_key_cleanup(self):
        for variant, selector in (("non-fde", "qemu.nke-non-fde"), ("fde", "qemu.nke-fde")):
            text = (SCRIPTS.parent / f"nke-node-{variant}.pkr.hcl").read_text()
            self.assertIn(f'except      = var.capture_apt_acquisitions ? [] : ["{selector}"]', text)
            self.assertIn('direction   = "download"', text)
            self.assertLess(text.index('direction   = "download"'), text.index('/scripts/provision.sh'))
            self.assertIn(f'/{variant}-apt-acquisitions.tar', text)

    @unittest.skipUnless(os.environ.get("NKE_TEST_PACKER"), "real Packer is opt-in")
    def test_real_packer_validates_capture_and_default_for_all_targets(self):
        materials = self.root / "materials"
        materials.mkdir()
        for binding in ("installer-iso", "image-inputs"):
            (materials / binding).write_bytes(b"validation fixture only")
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(self.root / "key")], check=True, timeout=30)
        (self.root / "firmware").write_bytes(b"validation fixture only")
        for architecture in ("amd64", "arm64"):
            manifest = {"schemaVersion": "nke.dev/os-image-materials/v1", "materials": [
                {"binding": binding, "component": binding, "kind": kind, "architecture": architecture,
                 "digest": "sha256:" + "a" * 64, "path": f"materials/{binding}"}
                for binding, kind in (("installer-iso", "disk-image"), ("image-inputs", "archive"))]}
            (self.root / "material-manifest.json").write_text(json.dumps(manifest))
            for variant in ("fde", "non-fde"):
                for capture in ("false", "true"):
                    with self.subTest(architecture=architecture, variant=variant, capture=capture):
                        result = subprocess.run([
                            os.environ["NKE_TEST_PACKER"], "validate", f"-only=nke-node-{variant}.qemu.nke-{variant}",
                            "-var", f"architecture={architecture}", "-var", f"capture_apt_acquisitions={capture}",
                            "-var", f"material_manifest={self.root / 'material-manifest.json'}",
                            "-var", f"output_directory={self.root / 'output'}",
                            "-var", f"ssh_private_key_file={self.root / 'key'}",
                            "-var", f"ssh_public_key_file={self.root / 'key.pub'}",
                            "-var", f"efi_firmware_code={self.root / 'firmware'}",
                            "-var", f"efi_firmware_vars={self.root / 'firmware'}",
                            str(SCRIPTS.parent),
                        ], text=True, capture_output=True, timeout=60)
                        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                        self.assertEqual(result.stdout.strip(), "The configuration is valid.")
                        self.assertEqual(result.stderr, "")


if __name__ == "__main__":
    unittest.main()
