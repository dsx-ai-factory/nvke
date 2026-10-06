# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Contract tests for source-owned APT archive capture (not real bake proof)."""

import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shlex
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import traceback
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
                self.assertEqual(stat.S_IMODE(self.output.stat().st_mode), 0o644)
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
            ("missing URI", {"uris": []}),
        ):
            with self.subTest(name=name):
                with self.assertRaises(ValueError):
                    self.capture(cache=self.cache(**changes))
                self.assertFalse(self.output.exists())
                self.assertFalse(list(self.root.glob(".apt-capture-*")))

    def test_unsafe_repository_uris_fail_with_safe_diagnostics(self):
        for name, uris, reason in (
            ("credentials", ["https://secret:token@example.invalid/package.deb"], "embedded credentials"),
            ("query", ["https://example.invalid/package.deb?token=secret"], "query or fragment"),
            ("fragment", ["https://example.invalid/package.deb#fragment"], "query or fragment"),
            ("mixed", ["https://example.invalid/package.deb", "file:/installer/package.deb"], "unsupported scheme"),
            ("file host", ["file://host/package.deb"], "unsupported scheme 'file'"),
            ("CD", ["cdrom:[Ubuntu]/package.deb"], "unsupported scheme 'cdrom'"),
            ("FTP", ["ftp://example.invalid/package.deb"], "unsupported scheme 'ftp'"),
            ("missing host", ["https:///package.deb"], "missing hostname"),
            ("whitespace", ["https://example.invalid/package .deb"], "whitespace"),
            ("malformed", ["https://[malformed/package.deb"], "malformed URI"),
            ("malformed secret", ["https://[secret:token]/package.deb"], "malformed URI"),
        ):
            with self.subTest(name=name):
                with self.assertRaisesRegex(ValueError, "unsafe repository metadata locations") as raised:
                    self.capture(cache=self.cache(uris=uris))
                self.assertIn("fixture=1:2.0-1:amd64", str(raised.exception))
                self.assertIn(reason, str(raised.exception))
                self.assertNotIn("secret", str(raised.exception))
                self.assertNotIn("token", str(raised.exception))
                diagnostic = "".join(traceback.format_exception(raised.exception))
                self.assertNotIn("secret:token", diagnostic)
                self.assertFalse(self.output.exists())

    def test_repository_location_count_has_distinct_error(self):
        uris = [f"https://mirror{number}.example.invalid/package.deb" for number in range(33)]
        with self.assertRaisesRegex(ValueError, "exceeds the 32 repository location limit"):
            self.capture(cache=self.cache(uris=uris))
        self.assertFalse(self.output.exists())

    def test_installed_only_version_does_not_read_unavailable_hash(self):
        class InstalledVersion:
            version, architecture, uris = "1:2.0-1", "amd64", []

            @property
            def sha256(self):
                raise SystemError("installed-only version has no archive hash")

        cache = {"fixture:amd64": SimpleNamespace(versions=[InstalledVersion()])}
        with self.assertRaisesRegex(ValueError, "no exact repository digest/size match"):
            self.capture(cache=cache)
        self.assertFalse(self.output.exists())

    def test_dpkg_error_preserves_tool_diagnostic(self):
        failure = subprocess.CalledProcessError(2, "dpkg-deb", stderr="not a Debian format archive\n")
        with patch.object(CAPTURE.subprocess, "run", side_effect=failure):
            with self.assertRaisesRegex(ValueError, "dpkg-deb could not read captured package: not a Debian format archive"):
                CAPTURE.package_metadata(self.package)

    def test_wrong_package_architecture(self):
        with patch.object(CAPTURE, "package_metadata", return_value=["fixture", "1:2.0-1", "arm64"]):
            with self.assertRaisesRegex(ValueError, "wrong architecture"):
                CAPTURE.capture(self.captured, self.output, "amd64", self.cache())
        self.assertFalse(self.output.exists())

    def test_nonregular_and_zero_size_packages(self):
        self.package.unlink()
        self.package.mkdir()
        with self.assertRaisesRegex(ValueError, "bounded regular file"):
            self.capture()
        self.package.rmdir()
        self.package.touch()
        with self.assertRaisesRegex(ValueError, "bounded regular file"):
            self.capture()
        self.assertFalse(self.output.exists())

    def test_fifo_package_fails_without_waiting_for_writer(self):
        self.package.unlink()
        os.mkfifo(self.package)
        # Isolate the call so a blocking open is a timed test failure rather
        # than a hung test runner. Metadata must not be reached for a FIFO.
        script = (
            "import importlib.util, sys; "
            f"spec=importlib.util.spec_from_file_location('capture_apt', {str(SCRIPTS / 'capture-apt-acquisitions.py')!r}); "
            "module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module); "
            "module.capture(sys.argv[1], sys.argv[2], 'amd64', {})"
        )
        result = subprocess.run([sys.executable, "-B", "-c", script, str(self.captured), str(self.output)],
                                capture_output=True, text=True, timeout=5)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("bounded regular file", result.stderr)
        self.assertFalse(self.output.exists())

    def test_symlinked_capture_roots_are_rejected(self):
        alias = self.root / "alias"
        alias.symlink_to(self.captured, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "capture root must not be a symbolic link"):
            CAPTURE.capture(alias, self.output, "amd64", self.cache())
        self.package.unlink()
        (self.captured / "packages").rmdir()
        (self.captured / "packages").symlink_to(self.root, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "capture root must not be a symbolic link"):
            self.capture()
        self.assertFalse(self.output.exists())

    def test_manifest_budget_rejects_before_publication(self):
        with patch.object(CAPTURE.json, "dumps", return_value="x" * ((8 << 20) + 1)):
            with self.assertRaisesRegex(ValueError, "capture manifest exceeds its byte budget"):
                self.capture()
        self.assertFalse(self.output.exists())
        self.assertFalse(list(self.root.glob(".apt-capture-*")))

    def test_main_uses_guest_apt_cache_and_explicit_arguments(self):
        cache = self.cache()
        with patch.dict(sys.modules, {"apt": SimpleNamespace(Cache=lambda: cache)}), patch.object(
            CAPTURE, "capture"
        ) as captured, patch.object(sys, "argv", ["capture", "--root", str(self.captured),
                                                "--output", str(self.output), "--architecture", "arm64"]):
            CAPTURE.main()
        captured.assert_called_once_with(self.captured, self.output, "arm64", cache)

    def test_malformed_dpkg_metadata_produces_no_output(self):
        for name, output in (
            ("missing field", "fixture\t1:2.0-1\n"),
            ("extra field", "fixture\t1:2.0-1\tamd64\textra\n"),
            ("empty package", "\t1:2.0-1\tamd64\n"),
            ("empty version", "fixture\t\tamd64\n"),
            ("empty architecture", "fixture\t1:2.0-1\t\n"),
            ("package whitespace", "fixture name\t1:2.0-1\tamd64\n"),
            ("version whitespace", "fixture\t1:2.0-1 \tamd64\n"),
            ("architecture whitespace", "fixture\t1:2.0-1\tamd64\r\n"),
            ("multiple rows", "fixture\t1:2.0-1\tamd64\nother\t1\tamd64\n"),
        ):
            with self.subTest(name=name), patch.object(
                CAPTURE.subprocess, "run", return_value=SimpleNamespace(stdout=output)
            ) as dpkg:
                with self.assertRaisesRegex(ValueError, "dpkg-deb returned invalid package metadata"):
                    CAPTURE.package_metadata(self.package)
                with self.assertRaisesRegex(ValueError, "dpkg-deb returned invalid package metadata"):
                    CAPTURE.capture(self.captured, self.output, "amd64", self.cache())
                self.assertEqual(dpkg.call_count, 2)
                self.assertEqual(dpkg.call_args.args[0], [
                    "dpkg-deb", "--show", "--showformat=${Package}\t${Version}\t${Architecture}\n", str(self.package)
                ])
                self.assertFalse(self.output.exists())
                self.assertFalse(list(self.root.glob(".apt-capture-*")))

    def test_all_architecture_package_uses_bare_repository_name(self):
        cache = {"fixture": self.cache("all")["fixture:all"]}
        for architecture in ("amd64", "arm64"):
            with self.subTest(architecture=architecture):
                with patch.object(CAPTURE, "package_metadata", return_value=["fixture", "1:2.0-1", "all"]):
                    manifest = CAPTURE.capture(self.captured, self.output, architecture, cache)
                self.assertEqual(manifest["architecture"], architecture)
                self.assertEqual(manifest["packages"][0]["architecture"], "all")
                self.assertEqual(manifest["packages"][0]["repositoryUris"], cache["fixture"].versions[0].uris)
                self.output.unlink()

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

    def test_changed_package_size_is_rejected_before_export(self):
        for name, replacement in (("grown", self.payload + b"extra"), ("truncated", self.payload[:-1])):
            with self.subTest(name=name):
                self.package.write_bytes(self.payload)
                def change_after_hashing(path):
                    path.write_bytes(replacement)
                    return ["fixture", "1:2.0-1", "amd64"]
                with patch.object(CAPTURE, "package_metadata", side_effect=change_after_hashing):
                    with self.assertRaisesRegex(ValueError, "changed before export"):
                        CAPTURE.capture(self.captured, self.output, "amd64", self.cache())
                self.assertFalse(self.output.exists())
                self.assertFalse(list(self.root.glob(".apt-capture-*")))

    def test_failed_publication_sync_removes_own_output(self):
        fsync = os.fsync
        def fail_directory_sync(descriptor):
            if stat.S_ISDIR(os.fstat(descriptor).st_mode):
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

    def test_capture_without_python_311_file_digest(self):
        # Reproduce the Python 3.10 stdlib boundary without raising the host floor.
        file_digest = getattr(hashlib, "file_digest", None)
        if file_digest is not None:
            del hashlib.file_digest
            self.addCleanup(setattr, hashlib, "file_digest", file_digest)
        self.capture()
        with tarfile.open(self.output) as archive:
            self.assertEqual(archive.extractfile(f"packages/{self.digest}.deb").read(), self.payload)

    @unittest.skipUnless(shutil.which("dpkg-deb"), "native dpkg-deb is unavailable")
    def test_native_dpkg_reads_real_package_metadata(self):
        package_root = self.root / "fixture"
        (package_root / "DEBIAN").mkdir(parents=True)
        (package_root / "DEBIAN" / "control").write_text(
            "Package: fixture\nVersion: 1:2.0-1\nArchitecture: all\n"
            "Maintainer: Fixture <fixture@example.invalid>\nDescription: metadata test fixture\n")
        package = self.root / "fixture.deb"
        subprocess.run(["dpkg-deb", "--build", str(package_root), str(package)],
                       check=True, capture_output=True, text=True, timeout=30)
        self.assertEqual(CAPTURE.package_metadata(package), ["fixture", "1:2.0-1", "all"])

    @unittest.skipUnless(os.environ.get("NKE_TEST_APT_CONFIG"), "native APT is opt-in")
    def test_native_apt_hook_retains_repeated_archive_names(self):
        # APT owns decoding its configuration. Have bash read apt-config's
        # shell output; redirect only the fixed retention directory to a fixture.
        configuration = subprocess.run([
            os.environ["NKE_TEST_APT_CONFIG"], "-c", str(SCRIPTS.parent / "cidata" / "apt-capture.conf"),
            "shell", "NKE_CAPTURE", "DPkg::Pre-Install-Pkgs::nke-capture",
        ], check=True, capture_output=True, text=True, timeout=30)
        decoded = subprocess.run(["bash", "-c", configuration.stdout + '\nprintf "%s" "$NKE_CAPTURE"'],
                                 check=True, capture_output=True, text=True, timeout=30).stdout
        self.assertIn("/var/lib/nke/apt-acquisitions/packages", decoded)
        destination = self.root / "retained archive bytes"
        command = decoded.replace("/var/lib/nke/apt-acquisitions/packages", shlex.quote(str(destination)))
        paths = []
        payloads = (b"first retained archive bytes", b"second retained archive bytes")
        for number, payload in enumerate(payloads):
            directory = self.root / f"input archives {number}"
            directory.mkdir()
            package = directory / "fixture.deb"
            package.write_bytes(payload)
            paths.append(package)
        subprocess.run(["bash", "-c", command], input="".join(f"{path}\n" for path in paths),
                       check=True, capture_output=True, text=True, timeout=30)
        retained = sorted(destination.iterdir())
        self.assertEqual([path.name for path in retained], ["fixture.deb", "fixture.deb.~1~"])
        self.assertCountEqual([path.read_bytes() for path in retained], payloads)
        self.assertEqual([path.read_bytes() for path in paths], list(payloads))

    def test_both_packer_variants_export_before_ssh_key_cleanup(self):
        for variant, selector in (("non-fde", "qemu.nke-non-fde"), ("fde", "qemu.nke-fde")):
            text = (SCRIPTS.parent / f"nke-node-{variant}.pkr.hcl").read_text()
            text = " ".join(text.split())
            self.assertEqual(text.count(f'except = var.capture_apt_acquisitions ? [] : ["{selector}"]'), 4)
            self.assertIn('direction = "download"', text)
            download = text.index('direction = "download"')
            self.assertLess(text.index('sudo systemctl mask --now apt-daily.timer'),
                            text.index('sudo python3 /var/tmp/capture-apt-acquisitions.py'))
            self.assertLess(text.index('sudo python3 /var/tmp/capture-apt-acquisitions.py'), download)
            provisioning = text.index('/scripts/provision.sh')
            for cleanup in ("/etc/apt/apt.conf.d/94curtin-config", "sudo rm -rf /var/lib/nke/apt-acquisitions"):
                self.assertLess(download, text.index(cleanup))
                self.assertLess(text.index(cleanup), provisioning)
            self.assertIn(f'/{variant}/apt-acquisitions.tar', text)
            self.assertLess(text.index("configuration=$(apt-config dump) || exit 1"), provisioning)
            self.assertIn("*DPkg::Pre-Install-Pkgs::nke-capture*", text)
            self.assertLess(text.index("sudo test ! -e /var/lib/nke/apt-acquisitions"), provisioning)

    def test_capture_cleanup_checks_existing_and_removed_directory_as_owner(self):
        # The fixture owner represents the privileged guest process. Forward
        # sudo locally, but require the source command to request that privilege.
        for variant in ("non-fde", "fde"):
            text = (SCRIPTS.parent / f"nke-node-{variant}.pkr.hcl").read_text()
            command = json.loads(next(line.strip().removesuffix(",") for line in text.splitlines()
                                      if "test ! -e /var/lib/nke/apt-acquisitions" in line))
            self.assertEqual(command, "sudo test ! -e /var/lib/nke/apt-acquisitions")
            command = command.replace("/var/lib/nke/apt-acquisitions", shlex.quote(str(self.captured)))
            for exists in (True, False):
                with self.subTest(variant=variant, exists=exists):
                    if exists:
                        self.captured.mkdir(exist_ok=True)
                    else:
                        shutil.rmtree(self.captured)
                    result = subprocess.run(["bash", "-c", 'sudo() { "$@"; }; ' + command],
                                            capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, 1 if exists else 0)

    def test_ci_validates_both_capture_settings(self):
        script = (SCRIPTS / "validate-packer-project.sh").read_text()
        self.assertIn("for capture in false true; do", script)
        self.assertIn('-var "capture_apt_acquisitions=${capture}"', script)

    def test_ci_renders_capture_configuration_with_installed_packer(self):
        workflow = (SCRIPTS.parents[2] / ".github" / "workflows" / "os-image-artifact.yml").read_text()
        validation = workflow.split("  validate-packer:", 1)[1].split("  publish:", 1)[0]
        render = validation.index("- name: Render the autoinstall capture configurations")
        self.assertLess(validation.index("- name: Install pinned Packer"), render)
        self.assertLess(render, validation.index("- name: Validate the Packer directory project"))
        rendering = validation[render:].split("- name: Validate the Packer directory project", 1)[0]
        self.assertIn("working-directory: os-image/packer/scripts", rendering)
        self.assertIn("NKE_TEST_PACKER: packer", rendering)
        self.assertIn("test_capture_apt_acquisitions.CaptureAPTTest.test_real_packer_renders_autoinstall_capture_configuration", rendering)

    def test_hook_cleanup_rejects_live_hook_and_apt_config_failure(self):
        for variant in ("non-fde", "fde"):
            text = (SCRIPTS.parent / f"nke-node-{variant}.pkr.hcl").read_text()
            command = json.loads(next(line.strip().removesuffix(",") for line in text.splitlines()
                                      if "configuration=$(apt-config dump)" in line))
            for dump, status, accepted in (("APT::Architecture amd64;", 0, True),
                                           ("DPkg::Pre-Install-Pkgs::nke-capture hook;", 0, False),
                                           ("", 1, False)):
                with self.subTest(variant=variant, dump=dump, status=status):
                    result = subprocess.run([
                        "bash", "-c", 'apt-config() { printf "%s" "$APT_DUMP"; return "$APT_STATUS"; }; ' + command,
                    ], env={**os.environ, "APT_DUMP": dump, "APT_STATUS": str(status)},
                        check=False, capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode == 0, accepted)

    @unittest.skipUnless(os.environ.get("NKE_TEST_PACKER"), "real Packer is opt-in")
    def test_real_packer_renders_autoinstall_capture_configuration(self):
        # Packer owns HCL template expansion and YAML decoding. The test does
        # not approximate the template grammar or add a Python YAML dependency.
        configuration = (SCRIPTS.parent / "cidata" / "apt-capture.conf").read_text()
        template = str(SCRIPTS.parent / "cidata" / "autoinstall-lvm.pkrtpl")
        for capture in (False, True):
            for encrypted in (False, True):
                with self.subTest(capture=capture, encrypted=encrypted):
                    values = dict(ssh_public_key="ssh-ed25519 fixture", luks_enabled=encrypted,
                                  luks_passphrase="fixture-only", nvidia_kernel_package="linux-nvidia",
                                  serial_console="ttyS0", capture_apt_acquisitions=capture,
                                  apt_capture_configuration=configuration)
                    expression = f"jsonencode(yamldecode(templatefile({json.dumps(template)}, {json.dumps(values)})))\n"
                    result = subprocess.run([os.environ["NKE_TEST_PACKER"], "console", "-config-type=hcl2"],
                                            input=expression, check=True, capture_output=True, text=True, timeout=30)
                    autoinstall = json.loads(result.stdout)["autoinstall"]
                    if capture:
                        self.assertEqual(autoinstall["apt"]["conf"], configuration)
                    else:
                        self.assertNotIn("apt", autoinstall)

if __name__ == "__main__":
    unittest.main()
