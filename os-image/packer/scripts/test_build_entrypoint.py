#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Exercise Make's Packer argv and early failures without downloading or booting."""
import json
import os
import shutil
import stat
from pathlib import Path
import subprocess
import tempfile
import unittest

PACKER_DIR = Path(__file__).resolve().parents[1]
REPO_ROOT = PACKER_DIR.parents[1]


class BuildEntrypointTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.packer_dir = self.root / "os-image/packer"
        self.packer_dir.mkdir(parents=True)
        shutil.copy(PACKER_DIR / "Makefile", self.packer_dir)
        shutil.copytree(PACKER_DIR / "scripts", self.packer_dir / "scripts")
        # The Makefile includes ../../make/tool-versions.mk for the committed
        # installer digests. Copy the real file rather than a stub so the
        # fixture resolves ISO_CHECKSUM exactly as a real checkout does.
        make_dir = self.root / "make"
        make_dir.mkdir()
        shutil.copy(REPO_ROOT / "make/tool-versions.mk", make_dir)
        self.env = os.environ.copy()
        self.env.update(
            PATH=f"{self.root}:{self.env['PATH']}",
            OVMF_CODE=str(self.root / "code.fd"),
            OVMF_VARS_TEMPLATE=str(self.root / "vars.fd"),
            PACKER_ARGS=str(self.root / "args.json"),
        )
        for name in ("code.fd", "vars.fd"):
            (self.root / name).touch()
        self.script("uname", '#!/bin/sh\ncase "$1" in -m) echo aarch64;; -s) echo Linux;; esac\n')
        # nvke_qemu_env resolves the QEMU binary by name off PATH; check-deps
        # looks for qemu-img, which the macOS CI runner lacks.
        self.script("qemu-system-aarch64", '#!/bin/sh\nif [ "$1" = -accel ]; then echo tcg; fi\n')
        self.script("qemu-img", '#!/bin/sh\nexit 0\n')
        self.script("packer", '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
Path(os.environ['PACKER_ARGS']).write_text(json.dumps(sys.argv[1:]))
values = dict(arg.split('=', 1) for arg in sys.argv[1:] if '=' in arg)
variant = 'non-fde' if 'non-fde' in values['-only'] else 'fde'
output = Path(values['output_directory']) / variant
output.mkdir(parents=True, exist_ok=True)
(output / 'nvke-node.qcow2').touch()
''')

    def script(self, name, text):
        p = self.root / name
        p.write_text(text)
        p.chmod(0o755)

    def make(self, target, *extra, skip_build_setup=False):
        args = [os.environ.get("MAKE", "make"), "--no-print-directory", "-C", str(self.packer_dir)]
        if skip_build_setup:
            args += ["-o", "prepare-image-build"]
            target = "_" + target
        args += [target, "ARCH=arm64", "ACCELERATOR=tcg", f"PACKER={self.root / 'packer'}",
                 f"ARTIFACTS_DIR={self.root / 'artifacts'}", f"BUILD_LOGS={self.root / 'logs'}", *extra]
        return subprocess.run(args, env=self.env, text=True, capture_output=True, check=False)

    def test_both_variants_pass_firmware_before_template_and_keep_current_defaults(self):
        for variant in ("fde", "non-fde"):
            with self.subTest(variant=variant):
                result = self.make(f"os-image-{variant}", skip_build_setup=True)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                args = json.loads(Path(self.env['PACKER_ARGS']).read_text())
                self.assertEqual(args[0], "build")
                self.assertEqual(args[-1], str(self.packer_dir))
                for value in ("accelerator=tcg", "cpu_model=max,pauth-impdef=on", "ssh_timeout=180m",
                              "root_fs_label=cloudimg-rootfs", f"material_manifest={self.packer_dir / '.packer-inputs/material-manifest.json'}",
                              f"efi_firmware_code={self.env['OVMF_CODE']}",
                              f"efi_firmware_vars={self.env['OVMF_VARS_TEMPLATE']}"):
                    self.assertIn(value, args)

    def test_serial_console_defaults_to_empty_and_passes_an_override_through(self):
        # Empty means "use the architecture default", resolved in the template
        # rather than in Make. A variant each, not the same one twice: the build
        # target refuses a pre-existing output directory.
        for variant, extra, expected in (
            ("non-fde", [], "serial_console="),
            ("fde", ["SERIAL_CONSOLE=ttyS1"], "serial_console=ttyS1"),
        ):
            with self.subTest(variant=variant, extra=extra):
                result = self.make(f"os-image-{variant}", *extra, skip_build_setup=True)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                args = json.loads(Path(self.env["PACKER_ARGS"]).read_text())
                self.assertIn(expected, args)

    def test_native_requirement_rejects_tcg_before_packer_build(self):
        result = self.make("os-image-non-fde", "REQUIRE_ACCELERATION=1", skip_build_setup=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot be hardware accelerated", result.stderr)
        self.assertFalse(Path(self.env['PACKER_ARGS']).exists())

    def test_acceleration_preflight_rejects_hvf_and_mismatched_iso_arch(self):
        cases = (
            (["ACCELERATOR=hvf"], "not supported for image builds"),
            (["ISO_URL=https://example.invalid/ubuntu-26.04.1-live-server-amd64.iso"],
             "does not match ARCH=arm64"),
        )
        for extra, expected in cases:
            with self.subTest(extra=extra):
                result = self.make("check-acceleration", *extra)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected, result.stderr)

    def test_package_preflight_selects_vm_for_macos_and_mismatched_linux(self):
        self.script("nix", "#!/bin/sh\nexit 0\n")
        for system, arch in (("Darwin", "arm64"), ("Linux", "x86_64")):
            with self.subTest(system=system, arch=arch):
                self.script("uname", f'#!/bin/sh\ncase "$1" in -m) echo {arch};; -s) echo {system};; esac\n')
                result = self.make("check-deps")
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("package builder: nix", result.stdout)

    def test_host_newer_than_the_target_release_cannot_build_packages(self):
        # The agent .deb declares a bare `Depends: libc6`, so an agent built on a
        # newer Ubuntu installs into the image and then fails to exec.
        self.script("go", "#!/bin/sh\nexit 0\n")
        self.script("nix", "#!/bin/sh\nexit 0\n")
        for host, mode, expected in (
            ("26.04", "auto", "package builder: nix"),
            ("24.04", "auto", "package builder: host"),
            ("22.04", "auto", "package builder: host"),
            ("24.04", "host", "package builder: host"),
        ):
            with self.subTest(host=host, mode=mode):
                self.env["NKE_HOST_UBUNTU_SERIES"] = host
                result = self.make(
                    "check-deps", f"NKE_PACKAGE_BUILDER={mode}", "UBUNTU_RELEASE=24.04.5"
                )
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(expected, result.stdout)

        self.env["NKE_HOST_UBUNTU_SERIES"] = "26.04"
        result = self.make("check-deps", "NKE_PACKAGE_BUILDER=host", "UBUNTU_RELEASE=24.04.5")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot build packages for an Ubuntu 24.04 image", result.stderr)
        del self.env["NKE_HOST_UBUNTU_SERIES"]

    def test_explicit_host_builder_rejects_mismatched_linux(self):
        self.script("uname", '#!/bin/sh\ncase "$1" in -m) echo x86_64;; -s) echo Linux;; esac\n')
        result = self.make("check-deps", "NKE_PACKAGE_BUILDER=host")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("matching Linux host", result.stderr)

    def test_missing_nix_explains_setup_but_native_build_does_not_require_it(self):
        bash = shutil.which("bash")
        self.script("go", "#!/bin/sh\nexit 0\n")
        for mode, expected_status in (("nix", 1), ("auto", 0)):
            with self.subTest(mode=mode):
                result = subprocess.run(
                    [bash, str(PACKER_DIR / "scripts/package-builder.sh"), "--check"],
                    # /usr/bin and /bin supply dirname; the stubs still come first.
                    env=self.env | {"PATH": f"{self.root}:/usr/bin:/bin", "ARCH": "arm64",
                                    "NKE_PACKAGE_BUILDER": mode},
                    text=True, capture_output=True,
                )
                self.assertEqual(result.returncode, expected_status, result.stderr)
                if mode == "nix":
                    self.assertIn("nix is required", result.stderr)
                    self.assertIn("Install Nix", result.stderr)
                    self.assertIn("os-image/nix/README.md", result.stderr)
                else:
                    self.assertIn("package builder: host", result.stdout)

    def test_check_deps_fails_when_the_guest_runner_cannot_be_realized(self):
        self.script("nix", '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
Path(os.environ['PACKER_ARGS']).write_text(json.dumps(sys.argv[1:]))
sys.exit(1 if sys.argv[1] == "build" else 0)
''')
        result = self.make("check-deps", "NKE_PACKAGE_BUILDER=nix")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads(Path(self.env["PACKER_ARGS"]).read_text()),
                         ["build", "--no-link", f"{self.packer_dir.parent}#builder-arm64"])

    def test_unknown_package_builder_fails_before_packer(self):
        result = self.make("check-deps", "NKE_PACKAGE_BUILDER=invalid")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("NKE_PACKAGE_BUILDER", result.stderr)

    def test_vm_adapter_invokes_nix_app_with_checkout_and_output(self):
        self.script("nix", '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
Path(os.environ['PACKER_ARGS']).write_text(json.dumps(sys.argv[1:]))
''')
        artifacts = self.root / "artifacts with spaces"
        result = subprocess.run(
            ["bash", str(PACKER_DIR / "scripts/package-builder.sh")],
            env=self.env | {"ARCH": "arm64", "NKE_PACKAGE_BUILDER": "nix",
                            "ARTIFACTS_DIR": str(artifacts)},
            text=True, capture_output=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(Path(self.env["PACKER_ARGS"]).read_text()), [
            "run", f"{PACKER_DIR.parent}#build-packages-arm64", "--",
            "--source", str(PACKER_DIR.parent.parent), "--output", str(artifacts),
        ])


class GuestUpdaterTest(unittest.TestCase):
    """The guest must reject absent or unreadable updater build outputs."""

    def test_guest_fetch_uses_guest_checkout(self):
        for source_root in (None, "/host/checkout"):
            with self.subTest(source_root=source_root), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp).resolve()
                repo = root / "guest checkout"
                scripts = repo / "os-image/packer/scripts"
                scripts.mkdir(parents=True)
                shutil.copy(PACKER_DIR / "scripts/fetch-artifacts.sh", scripts)
                files = repo / "os-image/files"
                files.mkdir()
                for name in (
                    "00-dhcp-all.yaml",
                    "99-disable-network-config.cfg",
                    "nvke-agent-spire.conf",
                    "wait-spiffe-socket.sh",
                    "conntrack-tune.service",
                    "configure-conntrack.sh",
                    "10-nke-conntrack-timeouts.conf",
                ):
                    shutil.copy(PACKER_DIR.parent / "files" / name, files)
                updater = repo / "agent/bin/linux_arm64/nvke-agent-updater"
                updater.parent.mkdir(parents=True)
                updater.touch()
                artifacts = root / "artifacts"
                artifacts.mkdir()
                tools = root / "tools"
                tools.mkdir()
                # Keep the real fetch script and shell; replace compilation and
                # guest-only cache setup, which cannot run on the test host.
                for name in ("mkdir", "ln", "go", "check-ubuntu-elf", "readelf"):
                    tool = tools / name
                    tool.write_text("#!/bin/sh\nexit 0\n")
                    tool.chmod(0o755)
                make = tools / "make"
                make.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$TEST_MAKE_LOG"\n')
                make.chmod(0o755)
                log = root / "make.log"
                env = os.environ | {"PATH": f"{tools}:{os.environ['PATH']}",
                                    "REPO_ROOT": str(repo), "ARCH": "arm64",
                                    "NKE_TARGET_UBUNTU_SERIES": "26.04",
                                    "ARTIFACTS_DIR": str(artifacts), "TEST_MAKE_LOG": str(log)}
                env.pop("SOURCE_ROOT", None)
                if source_root is not None:
                    env["SOURCE_ROOT"] = source_root
                result = subprocess.run(
                    [shutil.which("bash"), str(PACKER_DIR.parent / "nix/build-guest.sh")],
                    env=env, text=True, capture_output=True, timeout=15,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                calls = log.read_text().splitlines()
                self.assertEqual(len(calls), 6, calls)
                self.assertIn(f"-C {repo}/agent deb-arm64", calls[0])
                self.assertIn(f"-C {repo}/os-image/packages/doca-ovs debs", calls[5])
                self.assertIn("UBUNTU_SERIES=26.04", calls[5])
                self.assertTrue((artifacts / "00-dhcp-all.yaml").is_file())
                self.assertTrue((artifacts / "99-disable-network-config.cfg").is_file())
                self.assertTrue((artifacts / "nvke-agent-spire.conf").is_file())
                # provision.sh installs these from the artifacts directory, so
                # each must arrive byte-for-byte, with the mode it is installed with.
                for name, mode in (("conntrack-tune.service", 0o644),
                                   ("configure-conntrack.sh", 0o755),
                                   ("10-nke-conntrack-timeouts.conf", 0o644)):
                    staged = artifacts / name
                    self.assertTrue(staged.is_file(), name)
                    self.assertEqual(stat.S_IMODE(staged.stat().st_mode), mode, name)
                    self.assertEqual(staged.read_bytes(), (files / name).read_bytes(), name)


if __name__ == "__main__":
    unittest.main()
