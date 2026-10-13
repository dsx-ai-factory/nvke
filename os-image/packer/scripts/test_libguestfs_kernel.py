# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise runner kernel installation without modifying the host."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


HELPER = Path(__file__).with_name("libguestfs-kernel.sh")


class LibguestfsKernelTest(unittest.TestCase):
    def invoke(self, root, *, default_ok=True, fallback_ok=True, create_kernel=True,
               existing=False, release="24.04", distro="ubuntu", mode=0o644,
               chmod_ok=True, extra_readable=False, extra_unreadable=False):
        boot = root / "boot"
        boot.mkdir()
        if existing:
            kernel = boot / "vmlinuz-existing"
            kernel.write_text("kernel")
            kernel.chmod(mode)
        if extra_readable:
            (boot / "vmlinuz-z-readable").write_text("kernel")
        if extra_unreadable:
            kernel = boot / "vmlinuz-z-unreadable"
            kernel.write_text("kernel")
            kernel.chmod(0)
        release_file = root / "os-release"
        release_file.write_text(f'ID={distro}\nVERSION_ID="{release}"\n')
        tools = root / "tools"
        tools.mkdir()
        sudo = tools / "sudo"
        sudo.write_text("""#!/usr/bin/env bash
if [[ "$1" == chmod && "$CHMOD_OK" != 1 ]]; then
  exit 1
fi
exec "$@"
""")
        sudo.chmod(0o755)
        apt = tools / "apt-get"
        apt.write_text("""#!/usr/bin/env bash
printf '%s\\n' "$*" >> "$APT_LOG"
case "${*: -1}" in
  linux-image-virtual) ok="$DEFAULT_OK" ;;
  linux-image-virtual-hwe-*) ok="$FALLBACK_OK" ;;
  *) exit 99 ;;
esac
if [[ "$ok" != 1 ]]; then
  echo 'unavailable kernel dependency' >&2
  exit 100
fi
if [[ "$CREATE_KERNEL" == 1 ]]; then
  printf kernel > "$BOOT_DIR/vmlinuz-installed"
fi
""")
        apt.chmod(0o755)
        log = root / "apt.log"
        result = subprocess.run(
            ["bash", "-euc", 'source "$1"; ensure_appliance_kernel "$2" "$3"',
             "bash", str(HELPER), str(boot), str(release_file)],
            env={**os.environ, "PATH": f"{tools}{os.pathsep}{os.environ['PATH']}",
                 "APT_LOG": str(log), "BOOT_DIR": str(boot),
                 "DEFAULT_OK": str(int(default_ok)),
                 "FALLBACK_OK": str(int(fallback_ok)),
                 "CREATE_KERNEL": str(int(create_kernel)),
                 "CHMOD_OK": str(int(chmod_ok))},
            capture_output=True, text=True, check=False,
        )
        calls = log.read_text().splitlines() if log.exists() else []
        return result, calls

    def test_existing_kernel_skips_installation(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, calls = self.invoke(Path(tmp), existing=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(calls, [])

    @unittest.skipIf(os.geteuid() == 0, "root can read mode 0600 files")
    def test_unreadable_kernel_gets_read_permission(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            # Mode 000 makes the current unprivileged user unable to read it.
            result, calls = self.invoke(root, existing=True, mode=0)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((root / "boot/vmlinuz-existing").stat().st_mode & 0o777, 0o644)
            self.assertEqual(calls, [])

    @unittest.skipIf(os.geteuid() == 0, "root can read mode 000 files")
    def test_readable_kernel_survives_another_permission_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, calls = self.invoke(Path(tmp), existing=True, mode=0,
                                        chmod_ok=False, extra_readable=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(calls, [])
            self.assertIn("cannot make kernel readable", result.stderr)

    @unittest.skipIf(os.geteuid() == 0, "root can read mode 000 files")
    def test_readable_kernel_does_not_skip_later_permission_repair(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            result, calls = self.invoke(root, existing=True, extra_unreadable=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((root / "boot/vmlinuz-z-unreadable").stat().st_mode & 0o777, 0o644)
            self.assertEqual(calls, [])

    @unittest.skipIf(os.geteuid() == 0, "root can read mode 000 files")
    def test_readable_kernel_survives_a_later_permission_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, calls = self.invoke(Path(tmp), existing=True, extra_unreadable=True,
                                        chmod_ok=False)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(calls, [])
            self.assertIn("cannot make kernel readable", result.stderr)

    @unittest.skipIf(os.geteuid() == 0, "root can read mode 000 files")
    def test_no_readable_kernel_after_permission_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, calls = self.invoke(Path(tmp), existing=True, mode=0, chmod_ok=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(calls, [])
            self.assertIn("no readable kernel", result.stderr)

    def test_default_installation(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, calls = self.invoke(Path(tmp))
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(calls, ["install -y --no-install-recommends linux-image-virtual"])

    def test_default_failure_uses_runner_release(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, calls = self.invoke(Path(tmp), default_ok=False)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(calls, [
                "install -y --no-install-recommends linux-image-virtual",
                "install -y --no-install-recommends linux-image-virtual-hwe-24.04",
            ])
            self.assertIn("unavailable kernel dependency", result.stderr)

    def test_both_installations_fail(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, calls = self.invoke(Path(tmp), default_ok=False, fallback_ok=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(len(calls), 2)
            self.assertEqual(result.stderr.count("unavailable kernel dependency"), 2)

    def test_installation_without_kernel_fails_without_recursion(self):
        with tempfile.TemporaryDirectory() as tmp:
            result, calls = self.invoke(Path(tmp), create_kernel=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(len(calls), 1)
            self.assertIn("left no readable kernel", result.stderr)

    def test_unsupported_runner_does_not_guess_a_fallback(self):
        for distro, release in [("debian", "12"), ("ubuntu", "invalid")]:
            with self.subTest(distro=distro, release=release):
                with tempfile.TemporaryDirectory() as tmp:
                    result, calls = self.invoke(Path(tmp), default_ok=False,
                                                distro=distro, release=release)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(len(calls), 1)
                    self.assertIn("no Ubuntu HWE kernel fallback", result.stderr)


if __name__ == "__main__":
    unittest.main()
