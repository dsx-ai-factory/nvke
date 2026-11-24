#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Exercise the stock-builder guest script with its share in a temporary directory."""
import io
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("stock-guest.sh")
RUN_ID = "1795470000-4242"


class StockGuestTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.xchg = self.root / "xchg"
        self.xchg.mkdir()
        self.home = self.root / "home"
        self.home.mkdir()
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.events = self.root / "events"
        # nix develop runs the --command with the shell's tools on PATH.
        shell_bin = self.root / "devshell-bin"
        shell_bin.mkdir()
        self.tool("nke-ubuntu-cc", "#!/bin/sh\nexit 0\n", directory=shell_bin)
        self.tool("nix", f'''#!/usr/bin/env bash
echo "nix $*" >> "$TEST_EVENTS"
while (($#)) && [[ "$1" != --command ]]; do shift; done
shift
PATH="{shell_bin}:$PATH" exec "$@"
''')
        self.tool("systemctl", '#!/bin/sh\necho "systemctl $*" >> "$TEST_EVENTS"\n')
        self.env = os.environ | {
            "PATH": f"{self.tools}:{os.environ['PATH']}",
            "HOME": str(self.home),
            "NKE_XCHG": str(self.xchg),
            "TEST_EVENTS": str(self.events),
        }
        self.env.pop("TAR_OPTIONS", None)
        self.source(build_guest='''#!/usr/bin/env bash
set -euo pipefail
{
  echo "ARCH=$ARCH VERSION=$VERSION NGC=${NGC_API_KEY:-unset}"
  echo "CC=$NKE_UBUNTU_CC TAR_OPTIONS=${TAR_OPTIONS:-unset}"
  echo "REPO_ROOT=$REPO_ROOT"
  cat "$HOME/.config/containers/registries.conf" "$HOME/.config/containers/policy.json"
} > "$ARTIFACTS_DIR/seen.txt"
echo built > "$ARTIFACTS_DIR/nvke-agent.deb"
mkdir -p "$ARTIFACTS_DIR/debs"
echo index > "$ARTIFACTS_DIR/debs/Packages"
echo "build $ARCH" >> "$TEST_EVENTS"
''')
        (self.xchg / f"env-{RUN_ID}.sh").write_text(
            "export ARCH=arm64 VERSION=v1.2.3-4-gabc NGC_API_KEY=example-token\n")

    def tool(self, name, text, directory=None):
        path = (directory or self.tools) / name
        path.write_text(text)
        path.chmod(0o755)

    def source(self, build_guest):
        data = build_guest.encode()
        with tarfile.open(self.xchg / f"src-{RUN_ID}.tar", "w") as tar:
            info = tarfile.TarInfo("os-image/nix/build-guest.sh")
            info.size = len(data)
            info.mode = 0o755
            tar.addfile(info, io.BytesIO(data))

    def run_guest(self):
        return subprocess.run([shutil.which("bash"), str(SCRIPT), RUN_ID], env=self.env,
                              text=True, capture_output=True, timeout=30)

    def events_lines(self):
        return self.events.read_text().splitlines() if self.events.exists() else []

    def out_members(self):
        with tarfile.open(self.xchg / f"out-{RUN_ID}.tar") as tar:
            return {member.name.removeprefix("./"): member for member in tar.getmembers()}

    def test_success_returns_artifacts_then_status_then_powers_off(self):
        result = self.run_guest()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.xchg / f"started-{RUN_ID}").exists())
        self.assertEqual((self.xchg / f"status-{RUN_ID}").read_text().strip(), "0")
        members = self.out_members()
        self.assertIn("nvke-agent.deb", members)
        self.assertIn("debs/Packages", members)
        events = self.events_lines()
        self.assertTrue(events[0].startswith("nix "), events)
        self.assertIn("develop", events[0])
        self.assertEqual(events[1], "build arm64")
        self.assertEqual(events[-1], "systemctl poweroff")

    def test_devshell_comes_from_the_extracted_checkout(self):
        self.run_guest()
        nix = self.events_lines()[0]
        self.assertIn("--extra-experimental-features nix-command flakes develop", nix)
        self.assertIn(f"{self.home}/nke-build/src/os-image#package-build", nix)

    def test_env_file_is_read_then_deleted(self):
        self.run_guest()
        with tarfile.open(self.xchg / f"out-{RUN_ID}.tar") as tar:
            seen = tar.extractfile("./seen.txt").read().decode()
        self.assertIn("ARCH=arm64 VERSION=v1.2.3-4-gabc NGC=example-token", seen)
        self.assertFalse((self.xchg / f"env-{RUN_ID}.sh").exists())
        self.assertNotIn("example-token", (self.xchg / f"log-{RUN_ID}").read_text())

    def test_build_environment_matches_a_root_build(self):
        self.run_guest()
        with tarfile.open(self.xchg / f"out-{RUN_ID}.tar") as tar:
            seen = tar.extractfile("./seen.txt").read().decode()
        self.assertIn(f"CC={self.root}/devshell-bin/nke-ubuntu-cc", seen)
        # GNU tar keeps archive modes by default only for root.
        self.assertIn("TAR_OPTIONS=--same-permissions", seen)
        self.assertIn(f"REPO_ROOT={self.home}/nke-build/src", seen)
        self.assertIn('unqualified-search-registries = ["docker.io"]', seen)
        self.assertIn('"insecureAcceptAnything"', seen)

    def test_build_failure_still_reports_and_powers_off(self):
        self.source(build_guest="#!/usr/bin/env bash\necho 'compile error' >&2\nexit 3\n")
        result = self.run_guest()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.xchg / f"status-{RUN_ID}").read_text().strip(), "3")
        self.assertFalse((self.xchg / f"out-{RUN_ID}.tar").exists())
        self.assertIn("compile error", (self.xchg / f"log-{RUN_ID}").read_text())
        self.assertEqual(self.events_lines()[-1], "systemctl poweroff")

    def test_missing_env_file_fails_without_building(self):
        (self.xchg / f"env-{RUN_ID}.sh").unlink()
        self.run_guest()
        self.assertNotEqual((self.xchg / f"status-{RUN_ID}").read_text().strip(), "0")
        self.assertFalse(any(line.startswith("build ") for line in self.events_lines()))
        self.assertEqual(self.events_lines()[-1], "systemctl poweroff")

    def test_previous_build_tree_is_replaced(self):
        stale = self.home / "nke-build/artifacts/debs/stale.deb"
        stale.parent.mkdir(parents=True)
        stale.write_text("old")
        self.run_guest()
        self.assertNotIn("debs/stale.deb", self.out_members())


if __name__ == "__main__":
    unittest.main()
