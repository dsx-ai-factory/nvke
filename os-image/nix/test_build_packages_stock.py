#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Exercise the macOS stock-builder driver with a mock VM that plays the guest."""
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import subprocess
import tempfile
import time
import unittest

SCRIPT = Path(__file__).with_name("build-packages-stock.sh")

# Plays nixpkgs' stock builder: prints the console autologin, reads the lines
# typed on its serial console (stdin), and runs the guest side of one build
# against $TMPDIR/xchg, the directory QEMU shares as /tmp/xchg.
MOCK_VM = r'''#!/usr/bin/env python3
import io, json, os, re, sys, tarfile, time
from pathlib import Path

root = Path(os.environ["TEST_ROOT"])
xchg = Path(os.environ["TMPDIR"]) / "xchg"

def event(*item):
    with open(os.environ["TEST_EVENTS"], "a") as f:
        f.write(json.dumps(list(item)) + "\n")

boots = root / "boots"
boot = int(boots.read_text()) + 1 if boots.exists() else 1
boots.write_text(str(boot))
mask = os.umask(0)
os.umask(mask)
event("umask", oct(mask))
event("boot", boot, {k: os.environ.get(k) for k in ("KEYS", "NIX_DISK_IMAGE", "USE_TMPDIR", "QEMU_NET_OPTS")},
      Path(os.environ["NIX_DISK_IMAGE"]).exists())
Path(os.environ["NIX_DISK_IMAGE"]).touch()
event("share", sorted(p.name for p in xchg.iterdir()))
if boot <= int(os.environ.get("BOOT_FAIL", "0")):
    print("Kernel panic", flush=True)
    sys.exit(1)
time.sleep(float(os.environ.get("LOGIN_DELAY", "0")))
print("<<< Welcome to NixOS >>>\nnixos login: builder (automatic login)", flush=True)

def wait_for_poweroff():
    for line in sys.stdin:
        event("console", line.rstrip("\n"))
        if line.strip() == "systemctl poweroff":
            event("poweroff")
            sys.exit(0)
    sys.exit(0)

for line in sys.stdin:
    line = line.rstrip("\n")
    event("console", line)
    if line == "systemctl poweroff":
        event("poweroff")
        sys.exit(0)
    m = re.fullmatch(r"bash /tmp/xchg/run-(\S+)\.sh (\S+) &", line)
    if not m:
        continue
    rid = m.group(2)
    assert m.group(1) == rid, line
    assert (xchg / f"run-{rid}.sh").exists()
    if os.environ.get("NO_START"):
        wait_for_poweroff()
    (xchg / f"started-{rid}").touch()
    env_file = xchg / f"env-{rid}.sh"
    event("env", env_file.read_text())
    env_file.unlink()
    with tarfile.open(xchg / f"src-{rid}.tar") as tar:
        event("src", sorted(m.name.removeprefix("./") for m in tar.getmembers() if m.isfile()))
    if os.environ.get("HANG"):
        wait_for_poweroff()
    corrupt = boot <= int(os.environ.get("CORRUPT", "0"))
    status = 1 if corrupt else int(os.environ.get("GUEST_STATUS", "0"))
    if not os.environ.get("NO_LOG"):
        with open(xchg / f"log-{rid}", "w") as log:
            log.write("building packages\n")
            if corrupt:
                log.write("error: database disk image is malformed\n")
            if status:
                log.write("make: *** [Makefile:1: debs] Error 2\n")
    if status == 0 and not os.environ.get("NO_ARTIFACTS"):
        with tarfile.open(xchg / f"out-{rid}.tar", "w") as tar:
            for name, data in (("./nvke-agent.deb", b"agent"), ("./nvke-agent-spire.conf", b"conf"),
                               ("./debs/Packages", b"index"), ("./debs/new.deb", b"new")):
                info = tarfile.TarInfo(name)
                info.size = len(data)
                tar.addfile(info, io.BytesIO(data))
    if not os.environ.get("GUEST_NO_STATUS"):
        (xchg / f"status-{rid}").write_text(f"{status}\n")
    event("poweroff")
    sys.exit(0)
'''


class StockDriverTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.events = self.root / "events"
        vm_bin = self.root / "vm/bin"
        vm_bin.mkdir(parents=True)
        self.write(vm_bin / "run-nixos-vm", MOCK_VM)
        self.write(self.tools / "nix", '''#!/usr/bin/env python3
import json, os, sys
with open(os.environ["TEST_EVENTS"], "a") as f:
    f.write(json.dumps(["nix", *sys.argv[1:]]) + "\\n")
if os.environ.get("NIX_FAIL"):
    print("error: a 'aarch64-linux' with features {} is required", file=sys.stderr)
    sys.exit(1)
print(os.environ["TEST_ROOT"] + "/vm")
''')
        self.write(self.tools / "flock", '''#!/usr/bin/env python3
import fcntl, sys
fcntl.flock(int(sys.argv[-1]), fcntl.LOCK_EX | fcntl.LOCK_NB)
''')
        self.repo = self.root / "checkout"
        self.repo.mkdir()
        self.git("init", "-q")
        (self.repo / ".gitignore").write_text("ignored.txt\n")
        (self.repo / "tracked.txt").write_text("t")
        (self.repo / "deleted.txt").write_text("d")
        self.git("add", ".")
        self.git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "init")
        (self.repo / "deleted.txt").unlink()
        (self.repo / "untracked.txt").write_text("u")
        (self.repo / "ignored.txt").write_text("i")
        self.output = self.root / "output"
        self.state = self.root / "state with spaces"
        self.env = os.environ | {
            "PATH": f"{self.tools}:{os.environ['PATH']}",
            "TEST_ROOT": str(self.root),
            "TEST_EVENTS": str(self.events),
            "arch": "arm64",
            "vm_drv": "/nix/store/stock-vm.drv",
            "vm_program": "run-nixos-vm",
            "guest_script": str(self.root / "stock-guest.sh"),
            "NKE_BUILDER_STATE_DIR": str(self.state),
            "NKE_BUILDER_BOOT_TIMEOUT": "5",
            "NKE_BUILDER_START_TIMEOUT": "3",
            "VERSION": "v1.2.3",
        }
        for name in ("NGC_API_KEY", "K8S_VERSIONS", "NKE_TARGET_UBUNTU_SERIES"):
            self.env.pop(name, None)
        (self.root / "stock-guest.sh").write_text("# guest\n")

    def write(self, path, text):
        path.write_text(text)
        path.chmod(0o755)

    def git(self, *args):
        subprocess.run(["git", "-C", str(self.repo), *args], check=True)

    def run_driver(self, *args, **env):
        args = args or ("--source", str(self.repo), "--output", str(self.output))
        return subprocess.run([shutil.which("bash"), str(SCRIPT), *args], env=self.env | env,
                              text=True, capture_output=True, timeout=60)

    def event_list(self):
        if not self.events.exists():
            return []
        return [json.loads(line) for line in self.events.read_text().splitlines()]

    def boots(self):
        return [e for e in self.event_list() if e[0] == "boot"]

    def console(self):
        return [e[1] for e in self.event_list() if e[0] == "console"]

    def test_build_returns_artifacts_and_keeps_unrelated_output(self):
        (self.output / "debs").mkdir(parents=True)
        (self.output / "debs/stale.deb").write_text("old")
        (self.output / "unrelated.txt").write_text("keep")
        result = self.run_driver()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.output / "nvke-agent.deb").read_text(), "agent")
        self.assertEqual((self.output / "nvke-agent-spire.conf").read_text(), "conf")
        self.assertEqual(sorted(p.name for p in (self.output / "debs").iterdir()), ["Packages", "new.deb"])
        self.assertEqual((self.output / "unrelated.txt").read_text(), "keep")
        self.assertIn("building packages", result.stdout)
        # Secrets and sources do not outlive the run.
        self.assertFalse((self.state / "stock/runtime").exists())

    def test_vm_has_no_forwarded_port_and_a_persistent_disk(self):
        self.run_driver(QEMU_NET_OPTS="hostfwd=tcp:127.0.0.1:31022-:22")
        boot = self.boots()[0]
        self.assertIsNone(boot[2]["QEMU_NET_OPTS"])
        self.assertEqual(boot[2]["NIX_DISK_IMAGE"], str(self.state / "stock/root.qcow2"))
        self.assertEqual(boot[2]["USE_TMPDIR"], "1")
        self.assertTrue(Path(boot[2]["KEYS"]).is_dir())
        self.assertEqual(self.console()[0].endswith(" &"), True)

    def test_vm_starts_with_a_umask_the_guest_can_read(self):
        # The launcher copies the host's CA bundle into a share the guest reads
        # as another user; the 0700 state directory keeps other host users out.
        self.run_driver()
        self.assertEqual([e[1] for e in self.event_list() if e[0] == "umask"], ["0o22"])

    def test_check_realizes_the_vm_without_booting(self):
        result = self.run_driver("--check")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.event_list(), [["nix", "build", "--no-link", "--print-out-paths",
                                              "/nix/store/stock-vm.drv^out"]])

    def test_uncached_guest_fails_with_guidance(self):
        result = self.run_driver(NIX_FAIL="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("binary cache", result.stderr)
        self.assertIn("os-image/nix/README.md", result.stderr)
        self.assertEqual(self.boots(), [])

    def test_guest_failure_leaves_output_untouched_and_names_the_log(self):
        self.output.mkdir()
        (self.output / "nvke-agent.deb").write_text("previous")
        result = self.run_driver(GUEST_STATUS="2")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.output / "nvke-agent.deb").read_text(), "previous")
        log = self.state / "stock/build.log"
        self.assertIn(str(log), result.stderr)
        self.assertIn("Error 2", log.read_text())
        self.assertEqual(len(self.boots()), 1)

    def test_missing_status_is_a_failure(self):
        result = self.run_driver(GUEST_NO_STATUS="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no status", result.stderr)
        self.assertFalse((self.output / "nvke-agent.deb").exists())

    def test_missing_artifacts_leave_output_untouched(self):
        (self.output / "debs").mkdir(parents=True)
        (self.output / "debs/previous.deb").write_text("previous")
        result = self.run_driver(NO_ARTIFACTS="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.boots()), 1)
        self.assertIn("no artifacts", result.stderr)
        self.assertEqual(sorted(p.name for p in (self.output / "debs").iterdir()), ["previous.deb"])

    def test_boot_failure_recreates_the_disk_once(self):
        disk = self.state / "stock/root.qcow2"
        disk.parent.mkdir(parents=True)
        disk.write_text("old disk")
        result = self.run_driver(BOOT_FAIL="1")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        boots = self.boots()
        self.assertEqual(len(boots), 2)
        self.assertTrue(boots[0][3])        # the old disk was there
        self.assertFalse(boots[1][3])       # and was deleted before the retry
        self.assertIn("recreating", result.stderr)

    def test_second_boot_failure_gives_up(self):
        disk = self.state / "stock/root.qcow2"
        disk.parent.mkdir(parents=True)
        disk.write_text("old disk")
        result = self.run_driver(BOOT_FAIL="2")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.boots()), 2)
        self.assertIn(str(self.state / "stock/console.log"), result.stderr)

    def test_boot_failure_on_a_fresh_disk_does_not_retry(self):
        result = self.run_driver(BOOT_FAIL="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.boots()), 1)

    def test_corrupt_guest_store_recreates_the_disk_once(self):
        disk = self.state / "stock/root.qcow2"
        disk.parent.mkdir(parents=True)
        disk.write_text("old disk")
        result = self.run_driver(CORRUPT="1")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        boots = self.boots()
        self.assertEqual(len(boots), 2)
        self.assertFalse(boots[1][3])
        self.assertEqual((self.output / "nvke-agent.deb").read_text(), "agent")

    def test_lost_console_line_times_out_and_powers_off(self):
        result = self.run_driver(NO_START="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("did not start", result.stderr)
        self.assertIn("poweroff", [e[0] for e in self.event_list()])

    def test_signal_mid_build_powers_the_guest_off_from_the_console(self):
        proc = subprocess.Popen([shutil.which("bash"), str(SCRIPT), "--source", str(self.repo),
                                 "--output", str(self.output)], env=self.env | {"HANG": "1"},
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        deadline = time.time() + 20
        while time.time() < deadline and not any(e[0] == "src" for e in self.event_list()):
            time.sleep(0.1)
        proc.send_signal(signal.SIGTERM)
        out, err = proc.communicate(timeout=30)
        self.assertEqual(proc.returncode, 143, out + err)
        self.assertEqual(self.console()[-1], "systemctl poweroff")
        self.assertEqual(self.event_list()[-1], ["poweroff"])
        self.assertFalse((self.state / "stock/runtime").exists())

    def interrupt_on_a_terminal(self, trigger, **env):
        """Run the driver on a terminal, as a person does, and press Ctrl-C once `trigger` is logged."""
        self.output.mkdir()
        (self.output / "previous.deb").write_text("previous")
        pid, fd = pty.fork()
        if pid == 0:
            try:
                os.execve(shutil.which("bash"), ["bash", str(SCRIPT), "--source", str(self.repo),
                                                 "--output", str(self.output)], self.env | env)
            finally:
                os._exit(127)
        out, status, sent = b"", None, False
        deadline = time.time() + 60
        while status is None and time.time() < deadline:
            if not sent and any(e[0] == trigger for e in self.event_list()):
                time.sleep(1.5)  # into the wait that follows
                os.write(fd, b"\x03")
                sent = True
            if select.select([fd], [], [], 0.1)[0]:
                try:
                    out += os.read(fd, 4096)
                except OSError:
                    pass
            done, wait_status = os.waitpid(pid, os.WNOHANG)
            if done:
                status = os.waitstatus_to_exitcode(wait_status)
        if status is None:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        os.close(fd)
        out = out.decode(errors="replace")
        self.assertTrue(sent, out)
        self.assertEqual(status, 130, out)
        self.assertNotIn("Packages returned", out)
        self.assertEqual(sorted(p.name for p in self.output.iterdir()), ["previous.deb"])
        self.assertFalse((self.state / "stock/runtime").exists())

    def test_ctrl_c_on_a_terminal_while_booting_fails(self):
        self.interrupt_on_a_terminal("boot", LOGIN_DELAY="30", NKE_BUILDER_BOOT_TIMEOUT="60")

    def test_ctrl_c_on_a_terminal_while_starting_the_build_fails(self):
        self.interrupt_on_a_terminal("console", NO_START="1", NKE_BUILDER_START_TIMEOUT="60")
        self.assertEqual(self.console()[-1], "systemctl poweroff")

    def test_ctrl_c_on_a_terminal_mid_build_powers_the_guest_off(self):
        self.interrupt_on_a_terminal("src", HANG="1")
        self.assertEqual(self.console()[-1], "systemctl poweroff")

    def test_output_sync_failure_is_not_taken_for_a_corrupt_disk(self):
        # rsync exits 11 on a write error, which once meant "corrupt guest store" here.
        self.write(self.tools / "rsync", """#!/bin/sh
echo "rsync: write failed: No space left on device (28)" >&2
exit 11
""")
        disk = self.state / "stock/root.qcow2"
        disk.parent.mkdir(parents=True)
        disk.write_text("warm disk")
        result = self.run_driver()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.boots()), 1)
        self.assertTrue(disk.exists())
        self.assertNotIn("corrupt", result.stderr)

    def test_failed_source_staging_stops_before_boot(self):
        self.write(self.tools / "tar", """#!/bin/sh
echo "tar: write error" >&2
exit 2
""")
        result = self.run_driver()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.boots(), [])

    def test_failed_console_setup_stops_before_boot(self):
        self.write(self.tools / "mkfifo", """#!/bin/sh
echo "mkfifo: Operation not permitted" >&2
exit 1
""")
        result = self.run_driver()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.boots(), [])

    def test_secret_goes_through_the_env_file_not_the_console(self):
        result = self.run_driver(NGC_API_KEY="example-secret", NKE_TARGET_UBUNTU_SERIES="26.04")
        self.assertEqual(result.returncode, 0, result.stderr)
        env = [e[1] for e in self.event_list() if e[0] == "env"][0]
        self.assertIn("NGC_API_KEY=example-secret", env)
        self.assertIn("NKE_TARGET_UBUNTU_SERIES=26.04", env)
        self.assertIn("ARCH=arm64", env)
        self.assertIn("VERSION=v1.2.3", env)
        self.assertNotIn("K8S_VERSIONS", env)
        self.assertFalse(any("example-secret" in line for line in self.console()))
        self.assertNotIn("example-secret", (self.state / "stock/console.log").read_text())

    def test_source_tarball_has_tracked_and_untracked_files_only(self):
        self.run_driver()
        src = [e[1] for e in self.event_list() if e[0] == "src"][0]
        self.assertEqual(src, [".gitignore", "tracked.txt", "untracked.txt"])

    def test_nested_repository_is_not_archived(self):
        # Git lists an untracked repository, such as a worktree kept inside the
        # checkout, as one directory; its files, ignored ones included, stay out.
        nested = self.repo / "nested"
        subprocess.run(["git", "init", "-q", str(nested)], check=True)
        (nested / ".gitignore").write_text("secret.env\n")
        (nested / "secret.env").write_text("NGC_API_KEY=nested-secret\n")
        (nested / "file.txt").write_text("n")
        result = self.run_driver()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        src = [e[1] for e in self.event_list() if e[0] == "src"][0]
        self.assertEqual(src, [".gitignore", "tracked.txt", "untracked.txt"])

    def test_failed_file_listing_stops_before_boot(self):
        self.write(self.tools / "git", f"""#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = ls-files ]; then echo "fatal: index file corrupt" >&2; exit 128; fi
done
exec {shutil.which("git")} "$@"
""")
        result = self.run_driver()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.boots(), [])

    def test_output_inside_the_builder_state_is_refused(self):
        # The runtime directory is deleted at exit, and the rest is the VM's.
        output = self.state / "stock/runtime/artifacts"
        result = self.run_driver("--source", str(self.repo), "--output", str(output))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("outside the builder state", result.stderr)
        self.assertEqual(self.boots(), [])

    def test_stale_build_log_is_not_blamed_for_this_run(self):
        # A guest that fails before writing its log; an older log says "corrupt".
        disk = self.state / "stock/root.qcow2"
        disk.parent.mkdir(parents=True)
        disk.write_text("warm disk")
        (self.state / "stock/build.log").write_text("error: database disk image is malformed\n")
        result = self.run_driver(NO_LOG="1", GUEST_STATUS="2")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.boots()), 1)
        self.assertTrue(disk.exists())
        self.assertNotIn("corrupt", result.stderr)
        self.assertFalse((self.state / "stock/build.log").exists())

    def test_concurrent_run_fails_fast(self):
        lock_path = self.state / "stock/build.lock"
        lock_path.parent.mkdir(parents=True)
        with open(lock_path, "w") as held:
            fcntl.flock(held, fcntl.LOCK_EX)
            result = self.run_driver()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("already in use", result.stderr)
        self.assertEqual(self.event_list(), [])

    def test_stale_runtime_from_a_hard_stop_is_replaced(self):
        stale = self.state / "stock/runtime/xchg"
        stale.mkdir(parents=True)
        (stale / "env-old.sh").write_text("export NGC_API_KEY=old-secret\n")
        (self.state / "stock/console.in").write_text("not a fifo")
        result = self.run_driver()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        shares = [e[1] for e in self.event_list() if e[0] == "share"]
        self.assertEqual(len(shares), 1)
        self.assertNotIn("env-old.sh", shares[0])
        self.assertFalse((self.state / "stock/runtime").exists())

    def test_output_is_required_unless_checking(self):
        result = self.run_driver("--source", str(self.repo))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("--output", result.stderr)
        self.assertEqual(self.boots(), [])


if __name__ == "__main__":
    unittest.main()
