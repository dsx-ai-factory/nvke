#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Exercise Make's Packer argv and early failures without downloading or booting."""
import hashlib
import json
import os
import shutil
import signal
import stat
from pathlib import Path
import subprocess
import sys
import tempfile
import time
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
        shutil.copytree(PACKER_DIR / "builder", self.packer_dir / "builder")
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

    def test_acceleration_preflight_rejects_unknown_accelerator_and_mismatched_iso_arch(self):
        cases = (
            (["ACCELERATOR=whpx"], "not supported for image builds"),
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

    def macos_without_nix(self, *runtimes):
        """Darwin/arm64 with only the stubs and the base system on PATH."""
        self.script("uname", '#!/bin/sh\ncase "$1" in -m) echo arm64;; -s) echo Darwin;; esac\n')
        for name in runtimes:
            self.script(name, RUNTIME_STUB)
        self.env.update(PATH=f"{self.root}:/usr/bin:/bin:/usr/sbin:/sbin",
                        RUNTIME_LOG=str(self.root / "runtime.log"), IMAGE_PRESENT="1",
                        XDG_CACHE_HOME=str(self.root / "cache"))

    def runtime_calls(self):
        log = self.root / "runtime.log"
        return [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []

    def test_auto_on_macos_without_nix_selects_container_when_runtime_present(self):
        self.macos_without_nix("podman")
        result = self.make("check-deps", f"PYTHON={sys.executable}")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("package builder: container (arm64", result.stdout)
        self.assertEqual(self.runtime_calls()[0][:2], ["podman", "info"])

    def test_auto_on_macos_without_nix_or_runtime_names_both(self):
        self.macos_without_nix()
        result = self.make("check-deps", f"PYTHON={sys.executable}")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("os-image/nix/README.md", result.stderr)
        self.assertIn("podman", result.stderr)

    def test_auto_on_macos_with_only_docker_does_not_select_container(self):
        self.macos_without_nix("docker")
        result = self.make("check-deps", f"PYTHON={sys.executable}")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("podman", result.stderr)
        self.assertNotIn("package builder: container", result.stdout)

    def test_container_mode_rejected_off_macos_arm64(self):
        self.script("podman", RUNTIME_STUB)
        result = self.make("check-deps", "NKE_PACKAGE_BUILDER=container")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Apple-silicon macOS", result.stderr)

    def test_container_adapter_runs_fetch_artifacts_in_place(self):
        self.macos_without_nix("podman")
        artifacts = self.root / "artifacts with spaces"
        result = subprocess.run(
            ["bash", str(PACKER_DIR / "scripts/package-builder.sh")],
            env=self.env | {"ARCH": "arm64", "NKE_PACKAGE_BUILDER": "container",
                            "ARTIFACTS_DIR": str(artifacts), "NKE_TARGET_UBUNTU_SERIES": "26.04"},
            text=True, capture_output=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        run = next(c for c in self.runtime_calls() if c[1] == "run")
        self.assertEqual(run[-2:], ["bash", str(PACKER_DIR / "scripts/fetch-artifacts.sh")])
        self.assertTrue(BuilderContainerTest.has_pair(run, "-e", "NKE_BUILD_IN_PLACE=1"), run)
        self.assertTrue(BuilderContainerTest.has_pair(run, "-e", f"ARTIFACTS_DIR={artifacts}"), run)
        self.assertTrue(BuilderContainerTest.has_pair(run, "-e", f"SOURCE_ROOT={REPO_ROOT}"), run)
        self.assertTrue(BuilderContainerTest.has_pair(run, "-e", "NKE_TARGET_UBUNTU_SERIES=26.04"), run)

    def prepare_iso(self):
        """Run only the ISO step, with the patch script replaced by a recorder."""
        self.script_at(self.packer_dir / "scripts/prepare-installer-iso.sh",
                       '#!/bin/sh\nprintf "%s|%s\\n" "$ISO_URL" "$OUT_DIR" > "$ISO_MARKER"\n')
        self.env["ISO_MARKER"] = str(self.root / "iso.marker")
        return self.make("prepare-installer-iso", "-o", "preflight",
                         "ISO_URL=https://example.invalid/ubuntu-26.04.1-live-server-arm64.iso",
                         "ISO_CHECKSUM=sha256:" + "a" * 64)

    def script_at(self, path, text):
        path.write_text(text)
        path.chmod(0o755)

    def test_iso_preparation_runs_on_host_when_tools_present(self):
        self.macos_without_nix("podman")
        for tool in ("xorriso", "7z"):
            self.script(tool, "#!/bin/sh\nexit 0\n")
        result = self.prepare_iso()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue((self.root / "iso.marker").exists())
        self.assertEqual(self.runtime_calls(), [])

    def test_iso_preparation_runs_in_container_without_tools(self):
        self.macos_without_nix("podman")
        result = self.prepare_iso()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse((self.root / "iso.marker").exists())
        run = next(c for c in self.runtime_calls() if c[1] == "run")
        self.assertEqual(run[-1], "./scripts/prepare-installer-iso.sh")
        has = BuilderContainerTest.has_pair
        self.assertTrue(has(run, "-w", str(self.packer_dir)), run)
        self.assertTrue(has(run, "-e", "ISO_URL=https://example.invalid/ubuntu-26.04.1-live-server-arm64.iso"), run)
        self.assertTrue(has(run, "-e", "ISO_CHECKSUM=sha256:" + "a" * 64), run)
        self.assertTrue(has(run, "-e", f"OUT_DIR={self.packer_dir / 'iso-cache'}"), run)

    def test_iso_preparation_stays_on_linux_hosts_without_tools(self):
        # Container mode is for Apple-silicon Macs; Linux keeps the script's own
        # missing-tool error rather than a runtime requirement it never had.
        self.script("podman", RUNTIME_STUB)
        self.env.update(PATH=f"{self.root}:/usr/bin:/bin:/usr/sbin:/sbin",
                        RUNTIME_LOG=str(self.root / "runtime.log"))
        result = self.prepare_iso()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue((self.root / "iso.marker").exists())
        self.assertEqual(self.runtime_calls(), [])

    def test_install_packer_checks_digest(self):
        import zipfile
        archive = self.root / "packer.zip"
        with zipfile.ZipFile(archive, "w") as zf:
            zf.writestr("packer", "#!/bin/sh\necho fake packer\n")
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        self.script("uname", '#!/bin/sh\ncase "$1" in -m) echo arm64;; -s) echo Darwin;; esac\n')
        self.script("curl", f'''#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in -o) out=$2; shift 2;; *) printf '%s\\n' "$1" >> "{self.root}/curl.log"; shift;; esac
done
cp "{archive}" "$out"
''')
        installed = self.packer_dir / "bin/packer"
        for sha, ok in (("0" * 64, False), (digest, True)):
            with self.subTest(ok=ok):
                result = subprocess.run(
                    [os.environ.get("MAKE", "make"), "--no-print-directory", "-C", str(self.packer_dir),
                     "install-packer", f"PACKER_SHA256_DARWIN_ARM64={sha}"],
                    env=self.env, text=True, capture_output=True, check=False)
                if ok:
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertTrue(os.access(installed, os.X_OK))
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("checksum", result.stderr)
                    self.assertFalse(installed.exists())
        self.assertIn("_darwin_arm64.zip", (self.root / "curl.log").read_text())

    def test_packer_defaults_to_installed_binary(self):
        def print_packer(*extra):
            return subprocess.run(
                [os.environ.get("MAKE", "make"), "--no-print-directory", "-C", str(self.packer_dir),
                 "print-PACKER", *extra],
                env=self.env, text=True, capture_output=True, check=True).stdout.strip()
        self.assertEqual(print_packer(), "packer")
        (self.packer_dir / "bin").mkdir()
        self.script_at(self.packer_dir / "bin/packer", "#!/bin/sh\n")
        self.assertEqual(print_packer(), str(self.packer_dir / "bin/packer"))
        self.assertEqual(print_packer("PACKER=/x"), "/x")

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


RUNTIME_STUB = '''#!/usr/bin/env python3
import json, os, sys
with open(os.environ["RUNTIME_LOG"], "a") as log:
    log.write(json.dumps([os.path.basename(sys.argv[0])] + sys.argv[1:]) + "\\n")
args = sys.argv[1:]
if args[:1] == ["info"]:
    if "--format" in args:
        print(os.environ.get("SOCKET_PATH", "/run/podman/podman.sock"))
    sys.exit(int(os.environ.get("INFO_STATUS", "0")))
if args[:2] in (["image", "exists"], ["image", "inspect"]):
    sys.exit(0 if os.environ.get("IMAGE_PRESENT") else 1)
if args[:1] == ["run"] and os.environ.get("RUN_SLEEP"):
    import time
    time.sleep(float(os.environ["RUN_SLEEP"]))
'''


class BuilderContainerTest(unittest.TestCase):
    """The Linux-only build steps run in a pinned container on macOS hosts."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name).resolve()
        # A space in the checkout path proves every argv element is quoted.
        self.checkout = self.base / "check out"
        self.install_checkout(self.checkout)
        self.stubs = self.base / "stubs"
        self.stubs.mkdir()
        self.log = self.base / "runtime.log"
        self.env = {
            "PATH": f"{self.stubs}:/usr/bin:/bin",
            "HOME": str(self.base / "home"),
            "XDG_CACHE_HOME": str(self.base / "cache"),
            "RUNTIME_LOG": str(self.log),
            "ARCH": "arm64",
        }

    def install_checkout(self, root, containerfile=None):
        scripts = root / "os-image/packer/scripts"
        scripts.mkdir(parents=True, exist_ok=True)
        shutil.copy(PACKER_DIR / "scripts/builder-container.sh", scripts)
        builder = root / "os-image/packer/builder"
        builder.mkdir(parents=True, exist_ok=True)
        if containerfile is None:
            shutil.copy(PACKER_DIR / "builder/Dockerfile", builder)
        else:
            (builder / "Dockerfile").write_text(containerfile)

    def runtime(self, *names):
        for name in names:
            stub = self.stubs / name
            stub.write_text(RUNTIME_STUB)
            stub.chmod(0o755)

    def invoke(self, *args, root=None, cwd=None, **env):
        root = root or self.checkout
        return subprocess.run(
            [shutil.which("bash"), str(root / "os-image/packer/scripts/builder-container.sh"), *args],
            env=self.env | env, cwd=cwd or root, text=True, capture_output=True, check=False,
        )

    def calls(self):
        if not self.log.exists():
            return []
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def tag(self, root=None, arch="arm64"):
        root = root or self.checkout
        digest = hashlib.sha256((root / "os-image/packer/builder/Dockerfile").read_bytes()).hexdigest()
        return f"localhost/nvke-os-image-builder:{digest[:12]}-{arch}"

    @staticmethod
    def has_pair(argv, flag, value):
        return any(argv[i] == flag and argv[i + 1] == value for i in range(len(argv) - 1))

    def test_check_builds_missing_image_with_content_tag(self):
        self.runtime("podman")
        result = self.invoke("check")
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        self.assertEqual(calls[0][:2], ["podman", "info"])
        builder = self.checkout / "os-image/packer/builder"
        self.assertIn(["podman", "build", "--platform", "linux/arm64", "-t", self.tag(),
                       "-f", str(builder / "Dockerfile"), str(builder)], calls)

    def test_check_skips_build_when_image_present(self):
        self.runtime("podman")
        result = self.invoke("check", IMAGE_PRESENT="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse([c for c in self.calls() if c[1] == "build"], self.calls())

    def test_tag_changes_with_containerfile(self):
        self.runtime("podman")
        tags = []
        for content in ("FROM a\n", "FROM b\n"):
            root = self.base / f"fixture {content.strip()[-1]}"
            self.install_checkout(root, containerfile=content)
            self.log.unlink(missing_ok=True)
            result = self.invoke("check", root=root)
            self.assertEqual(result.returncode, 0, result.stderr)
            build = next(c for c in self.calls() if c[1] == "build")
            tags.append(build[build.index("-t") + 1])
        self.assertNotEqual(tags[0], tags[1])

    def test_check_fails_fast_when_runtime_unreachable(self):
        self.runtime("podman")
        result = self.invoke("check", INFO_STATUS="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("podman machine start", result.stderr)
        self.assertFalse([c for c in self.calls() if c[1] == "build"])

    def test_run_mounts_checkout_at_same_path_and_passes_env(self):
        self.runtime("podman")
        cwd = self.checkout / "os-image/packer"
        result = self.invoke("run", "./scripts/prepare-installer-iso.sh", "an arg", cwd=cwd,
                             IMAGE_PRESENT="1", K8S_VERSIONS="1.36.3")
        self.assertEqual(result.returncode, 0, result.stderr)
        run = next(c for c in self.calls() if c[1] == "run")
        self.assertIn("--rm", run)
        self.assertTrue(self.has_pair(run, "--platform", "linux/arm64"), run)
        self.assertTrue(self.has_pair(run, "-v", f"{self.checkout}:{self.checkout}"), run)
        self.assertTrue(self.has_pair(run, "-w", str(cwd)), run)
        self.assertTrue(self.has_pair(run, "-e", "ARCH=arm64"), run)
        self.assertTrue(self.has_pair(run, "-e", "K8S_VERSIONS=1.36.3"), run)
        self.assertTrue(self.has_pair(run, "-e", "GIT_CONFIG_VALUE_0=*"), run)
        # Go makes its module cache read-only, which macOS `rm -rf` cannot
        # delete; the README's clean step relies on a writable cache.
        self.assertTrue(self.has_pair(run, "-e", "GOFLAGS=-modcacherw"), run)
        self.assertFalse([a for a in run if a.startswith("CALICO_VERSIONS")], run)
        self.assertEqual(run[-3:], [self.tag(), "./scripts/prepare-installer-iso.sh", "an arg"])
        cache = self.base / "cache/nvke/os-image-builder/arm64"
        self.assertTrue(cache.is_dir())
        self.assertTrue(self.has_pair(run, "-v", f"{cache}:/var/cache/nvke-builder"), run)

    def test_run_mounts_external_git_dir_for_worktrees(self):
        # A linked worktree's .git file points into the main checkout, which
        # git describe (the agent's version stamp) has to reach.
        git = shutil.which("git")
        main = self.base / "main repo"
        main.mkdir()
        quiet = {"capture_output": True, "check": True}
        subprocess.run([git, "-C", str(main), "init", "-q"], **quiet)
        subprocess.run([git, "-C", str(main), "-c", "user.name=t", "-c", "user.email=t@example.com",
                        "commit", "-q", "--allow-empty", "-m", "init"], **quiet)
        worktree = self.base / "linked tree"
        subprocess.run([git, "-C", str(main), "worktree", "add", "-q", "--detach", str(worktree)], **quiet)
        self.install_checkout(worktree)
        self.runtime("podman")
        result = self.invoke("run", "true", root=worktree, IMAGE_PRESENT="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        run = next(c for c in self.calls() if c[1] == "run")
        git_dir = main / ".git"
        self.assertTrue(self.has_pair(run, "-v", f"{git_dir}:{git_dir}"), run)

    def dist_volume(self, root, package, arch="arm64"):
        checkout = hashlib.sha256(str(root).encode()).hexdigest()[:12]
        return f"nvke-os-image-builder-{arch}-{package}-{checkout}"

    def test_run_backs_package_dist_dirs_with_volumes(self):
        # Package scratch must not live on a macOS-backed mount, where container
        # root has no override for a mode-0 file (GNU tar's placeholder for a
        # `..` symlink): a Linux-native volume per package and checkout instead.
        for name, makefile in (("alpha", True), ("beta", True), ("common", False)):
            package = self.checkout / "os-image/packages" / name
            package.mkdir(parents=True)
            if makefile:
                (package / "Makefile").write_text("debs:\n")
        self.runtime("podman")
        result = self.invoke("run", "true", IMAGE_PRESENT="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        run = next(c for c in self.calls() if c[1] == "run")
        packages = self.checkout / "os-image/packages"
        for name in ("alpha", "beta"):
            self.assertTrue(self.has_pair(run, "-v", f"{self.dist_volume(self.checkout, name)}:{packages / name / 'dist'}"), run)
        self.assertFalse([a for a in run if "common/dist" in a], run)

    def real_builder(self, cwd, script, root=None):
        root = root or self.checkout
        return subprocess.run(
            [shutil.which("bash"), str(root / "os-image/packer/scripts/builder-container.sh"),
             "run", "sh", "-c", script],
            env=os.environ | {"ARCH": "arm64"},
            cwd=cwd, text=True, capture_output=True, check=False, timeout=1800,
        )

    @unittest.skipUnless(os.environ.get("NKE_TEST_BUILDER_RUNTIME"),
                         "set NKE_TEST_BUILDER_RUNTIME=1 to run the real builder image under podman")
    def test_builder_extracts_dotdot_symlinks_into_package_dist(self):
        # cni-ovn extracts a git archive whose symlinks point through `..`.
        package = self.checkout / "os-image/packages/dotdot-probe"
        package.mkdir(parents=True)
        (package / "Makefile").write_text("debs:\n")
        volume = self.dist_volume(self.checkout, "dotdot-probe")
        self.addCleanup(subprocess.run, ["podman", "volume", "rm", "-f", volume], capture_output=True)
        script = ("set -e; mkdir -p /tmp/a/sub; echo payload > /tmp/a/root.md; "
                  "ln -s ../root.md /tmp/a/sub/link; tar -cf /tmp/t.tar -C /tmp/a .; "
                  "mkdir -p dist/src; tar -x --no-same-owner -C dist/src -f /tmp/t.tar; cat dist/src/sub/link")
        result = self.real_builder(package, script)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("payload", result.stdout.split())

    @unittest.skipUnless(os.environ.get("NKE_TEST_BUILDER_RUNTIME"),
                         "set NKE_TEST_BUILDER_RUNTIME=1 to run the real builder image under podman")
    def test_builder_builds_and_saves_an_image_through_podman(self):
        # cni-ovn builds its image from source with OCI_BIN and saves a
        # docker-archive; in the container that goes to the machine's podman.
        package = self.checkout / "os-image/packages/oci-probe"
        package.mkdir(parents=True)
        (package / "Makefile").write_text("debs:\n")
        volume = self.dist_volume(self.checkout, "oci-probe")
        ref = "localhost/nvke-builder-oci-probe:test"
        self.addCleanup(subprocess.run, ["podman", "volume", "rm", "-f", volume], capture_output=True)
        self.addCleanup(subprocess.run, ["podman", "rmi", "-f", ref], capture_output=True)
        script = (f'set -e; mkdir -p dist/ctx; echo probe > dist/ctx/f; printf "FROM scratch\\nCOPY f /f\\n" > dist/ctx/Containerfile; '
                  f'"$OCI_BIN" build --platform linux/arm64 -t {ref} -f dist/ctx/Containerfile dist/ctx >/dev/null; '
                  f'"$OCI_BIN" save --format docker-archive -o dist/image.tar {ref}; tar -tf dist/image.tar | grep -c manifest.json')
        result = self.real_builder(package, script)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout.split()[-1:], ["1"], result.stdout + result.stderr)

    def interrupt_run(self, cwd, *command, env=None, root=None, settle=5.0):
        """Start `run`, wait until the container is up, then send SIGINT."""
        root = root or self.checkout
        proc = subprocess.Popen(
            [shutil.which("bash"), str(root / "os-image/packer/scripts/builder-container.sh"), "run", *command],
            env=env, cwd=cwd, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        self.addCleanup(lambda: proc.poll() is None and proc.kill())
        return proc

    def test_interrupt_removes_the_container_and_exits(self):
        # podman forwards SIGINT only to the container's PID 1, which waits for
        # its own child, so Ctrl-C used to wait out the whole package build.
        self.runtime("podman")
        proc = self.interrupt_run(self.checkout, "true",
                                  env=self.env | {"IMAGE_PRESENT": "1", "RUN_SLEEP": "60"})
        deadline = time.monotonic() + 10
        while not [c for c in self.calls() if c[1] == "run"] and time.monotonic() < deadline:
            time.sleep(0.1)
        run = next(c for c in self.calls() if c[1] == "run")
        name = run[run.index("--name") + 1]
        proc.send_signal(signal.SIGINT)
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.fail("builder-container.sh kept waiting after SIGINT")
        self.assertEqual(proc.returncode, 130, proc.stderr.read())
        self.assertIn(["podman", "rm", "-f", "-t", "0", name], self.calls())

    @unittest.skipUnless(os.environ.get("NKE_TEST_BUILDER_RUNTIME"),
                         "set NKE_TEST_BUILDER_RUNTIME=1 to run the real builder image under podman")
    def test_builder_interrupt_stops_the_real_container(self):
        proc = self.interrupt_run(REPO_ROOT, "sleep", "300", root=REPO_ROOT,
                                  env=os.environ | {"ARCH": "arm64"})
        name = f"nvke-os-image-builder-{proc.pid}"
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            listed = subprocess.run(["podman", "ps", "-q", "--filter", f"name=^{name}$"],
                                    capture_output=True, text=True).stdout.strip()
            if listed:
                break
            time.sleep(0.5)
        self.assertTrue(listed, "the container never started")
        proc.send_signal(signal.SIGINT)
        proc.wait(timeout=60)
        self.assertEqual(proc.returncode, 130)
        left = subprocess.run(["podman", "ps", "-aq", "--filter", f"name=^{name}$"],
                              capture_output=True, text=True).stdout.strip()
        self.assertEqual(left, "")

    def test_run_hands_the_podman_service_to_the_container(self):
        self.runtime("podman")
        result = self.invoke("run", "true", IMAGE_PRESENT="1", SOCKET_PATH="/run/user/502/podman/podman.sock")
        self.assertEqual(result.returncode, 0, result.stderr)
        run = next(c for c in self.calls() if c[1] == "run")
        self.assertTrue(self.has_pair(run, "-v", "/run/user/502/podman/podman.sock:/run/podman/podman.sock"), run)
        self.assertTrue(self.has_pair(run, "-e", "CONTAINER_HOST=unix:///run/podman/podman.sock"), run)
        self.assertTrue(self.has_pair(run, "-e", "OCI_BIN=podman"), run)
        # The machine's SELinux policy denies a container the runtime socket.
        self.assertTrue(self.has_pair(run, "--security-opt", "label=disable"), run)

    def test_runtime_is_podman_only(self):
        # The OVN image is built through the podman service, so docker alone
        # cannot do the whole build.
        self.runtime("docker")
        result = self.invoke("check", IMAGE_PRESENT="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("podman", result.stderr)
        self.assertEqual(self.calls(), [])

    @unittest.skipUnless(os.environ.get("NKE_TEST_BUILDER_RUNTIME"),
                         "set NKE_TEST_BUILDER_RUNTIME=1 to run the real builder image under podman")
    def test_builder_awk_supports_interval_expressions(self):
        # The package bundles parse publisher checksum lists with
        # /^[0-9a-fA-F]{64}$/. Bookworm's default mawk (1.3.4-20200120) has no
        # interval expressions, so every digest line silently failed to match.
        digest = "0" * 64
        program = '$1 ~ /^[0-9a-fA-F]{64}$/ { print "match" }'
        result = self.real_builder(REPO_ROOT, f'echo "{digest}  cni.tgz" | awk \'{program}\'', root=REPO_ROOT)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("match", result.stdout.split(), result.stdout + result.stderr)

    def test_amd64_requests_amd64_platform(self):
        self.runtime("podman")
        self.assertEqual(self.invoke("check", ARCH="amd64").returncode, 0)
        self.assertEqual(self.invoke("run", "true", ARCH="amd64", IMAGE_PRESENT="1").returncode, 0)
        build = next(c for c in self.calls() if c[1] == "build")
        run = next(c for c in self.calls() if c[1] == "run")
        self.assertTrue(self.has_pair(build, "--platform", "linux/amd64"), build)
        self.assertTrue(self.has_pair(run, "--platform", "linux/amd64"), run)
        self.assertEqual(build[build.index("-t") + 1], self.tag(arch="amd64"))


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
