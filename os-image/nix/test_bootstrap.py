#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Exercise bootstrap ordering and process ownership without a VM or Nix daemon."""
import fcntl
import json
import os
import signal
from pathlib import Path
import subprocess
import tempfile
import time
import unittest

SCRIPT = Path(__file__).with_name("bootstrap-packages.sh")


class BootstrapTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.events = self.root / "events"
        self.env = os.environ | {
            "PATH": f"{self.tools}:{os.environ['PATH']}",
            "TEST_ROOT": str(self.root),
            "TEST_EVENTS": str(self.events),
            "arch": "arm64",
            "guest_system": "aarch64-linux",
            "host_system": "aarch64-darwin",
            "runner_drv": "/nix/store/package-runner.drv",
            "bootstrap_drv": "/nix/store/bootstrap.drv",
            "bootstrap_program": "run-nixos-vm",
            "bootstrap_host_key": "ssh-ed25519 PUBLIC_TEST_KEY",
            "NKE_BUILDER_STATE_DIR": str(self.root / "state with spaces"),
            "NKE_BUILDER_BOOT_TIMEOUT": "5",
        }
        self.script("nix-store", '#!/bin/sh\nprintf "%s\\n" "$2"\n')
        # SSH is mocked and returns immediately; exercise shutdown ordering
        # without depending on the host's GNU coreutils installation.
        self.script("timeout", '#!/bin/sh\nshift\nexec "$@"\n')
        self.script("flock", '''#!/usr/bin/env python3
import fcntl, sys
fcntl.flock(int(sys.argv[-1]), fcntl.LOCK_EX | fcntl.LOCK_NB)
''')
        self.script("nix", '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
args = sys.argv[1:]
root = Path(os.environ['TEST_ROOT'])
with open(os.environ['TEST_EVENTS'], 'a') as f:
    f.write(json.dumps(['nix', *args]) + '\\n')
if '--dry-run' in args:
    if os.environ.get('PLAN_FAIL'):
        print('cache query failed', file=sys.stderr)
        sys.exit(9)
    if os.environ.get('CACHED'):
        print('this path will be fetched:\\n  /nix/store/cached-linux-package', file=sys.stderr)
    else:
        print('this derivation will be built:\\n  /nix/store/missing.drv', file=sys.stderr)
elif args[:2] == ['derivation', 'show']:
    print(json.dumps({'derivations': {'missing.drv': {'system': os.environ.get('MISSING_SYSTEM', 'aarch64-linux')}}}))
elif args[:3] == ['config', 'show', 'builders']:
    print(os.environ.get('BUILDERS', ''))
elif args[:2] == ['store', 'info']:
    print(json.dumps({'trusted': not bool(os.environ.get('UNTRUSTED'))}))
elif args[0] == 'build':
    bootstrap = '/nix/store/bootstrap.drv^out' in args
    if os.environ.get('BUILD_FAIL') and not bootstrap:
        sys.exit(7)
    print(root / ('bootstrap' if bootstrap else 'runner'))
else:
    sys.exit('unexpected nix command: ' + repr(args))
''')
        self.script("ssh", '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
root = Path(os.environ['TEST_ROOT'])
if sys.argv[-1] == 'poweroff':
    (root / 'stop').touch()
    with open(os.environ['TEST_EVENTS'], 'a') as f: f.write(json.dumps(['poweroff']) + '\\n')
else:
    sys.exit(0 if (root / 'started').exists() and not os.environ.get('BOOT_FAIL') else 1)
''')
        # Who listens on the bootstrap port: nobody before launch, then the VM.
        # SQUAT_* puts another process there before or after the launch.
        self.script("lsof", '''#!/usr/bin/env python3
import os
from pathlib import Path
root = Path(os.environ['TEST_ROOT'])
started = (root / 'started').exists()
if os.environ.get('SQUAT_BEFORE') or (started and os.environ.get('SQUAT_AFTER')):
    print(4242)
elif started:
    print((root / 'pid').read_text())
''')
        for package in ("bootstrap", "runner"):
            (self.root / package / "bin").mkdir(parents=True)
        # The handler is installed before the markers, so the driver cannot signal
        # this mock while the default SIGTERM disposition would still kill it.
        self.script("run-nixos-vm", '''#!/usr/bin/env python3
import json, os, signal, time
from pathlib import Path
root = Path(os.environ['TEST_ROOT'])
def stop(*args):
    (root / 'terminated').touch()
    raise SystemExit(0)
signal.signal(signal.SIGTERM, signal.SIG_IGN if os.environ.get('IGNORE_TERM') else stop)
(root / 'pid').write_text(str(os.getpid()))
disk = os.environ.get('NIX_DISK_IMAGE', '')
with open(root / 'disks', 'a') as f: f.write(disk + '\\n')
if disk: Path(disk).touch()
(root / 'started').touch()
with open(os.environ['TEST_EVENTS'], 'a') as f: f.write(json.dumps(['started']) + '\\n')
while root.exists() and not (root / 'stop').exists(): time.sleep(0.02)
''', self.root / "bootstrap" / "bin")
        self.script("build-packages-arm64", '''#!/usr/bin/env python3
import json, os, sys
with open(os.environ['TEST_EVENTS'], 'a') as f: f.write(json.dumps(['packages', *sys.argv[1:]]) + '\\n')
''', self.root / "runner" / "bin")

    def script(self, name, source, directory=None):
        p = (directory or self.tools) / name
        p.write_text(source)
        p.chmod(0o755)

    def args(self, *extra):
        return ["bash", str(SCRIPT), "--source", "/checkout with spaces",
                "--output", "/artifacts", *extra]

    def run_build(self, *extra, timeout=15, **env):
        result = subprocess.run(
            self.args(*extra),
            env=self.env | env, capture_output=True, text=True, timeout=timeout,
        )
        events = [json.loads(line) for line in self.events.read_text().splitlines()] if self.events.exists() else []
        return result, events

    def test_missing_client_public_key_is_reconstructed(self):
        state = Path(self.env["NKE_BUILDER_STATE_DIR"]) / "bootstrap"
        state.mkdir(parents=True)
        key = state / "client-key"
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)], check=True)
        key.with_suffix(".pub").unlink()
        result, _ = self.run_build()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(key.with_suffix(".pub").is_file())

    def test_signal_after_launch_reaps_bootstrap(self):
        script = self.root / "interrupted-bootstrap.sh"
        script.write_text(SCRIPT.read_text().replace(
            "\n  vm_pid=$!\n",
            '\n  printf "%s" "$!" >"$TEST_ROOT/launched-pid"\n  kill -TERM "$$"\n  vm_pid=$!\n',
        ))
        result = subprocess.run(["bash", str(script), "--check"], env=self.env,
                                capture_output=True, text=True, timeout=15)
        pid = int((self.root / "launched-pid").read_text())
        try:
            self.assertEqual(result.returncode, 143, result.stderr)
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)
        finally:
            try:
                os.killpg(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass

    def test_cached_outputs_skip_bootstrap(self):
        result, events = self.run_build(CACHED="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn(['started'], events)
        self.assertEqual(events[-1], ['packages', '--source', '/checkout with spaces', '--output', '/artifacts'])

    def test_only_host_builds_skip_bootstrap(self):
        result, events = self.run_build(MISSING_SYSTEM="aarch64-darwin")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn(['started'], events)
        self.assertEqual(events[-1][0], 'packages')

    def test_missing_linux_work_bootstraps_before_realization_and_stops_before_packages(self):
        result, events = self.run_build()
        self.assertEqual(result.returncode, 0, result.stderr)
        build = next(i for i, e in enumerate(events) if e[:2] == ['nix', 'build'] and '--dry-run' not in e and '/nix/store/package-runner.drv^out' in e)
        self.assertLess(events.index(['started']), build)
        self.assertLess(build, events.index(['poweroff']))
        self.assertEqual(events[-1][0], 'packages')
        args = events[build]
        self.assertIn('--builders', args)
        self.assertIn('127.0.0.1', args[args.index('--builders') + 1])
        self.assertIn('state%20with%20spaces', args[args.index('--builders') + 1])
        self.assertIn(' kvm,benchmark,big-parallel ', args[args.index('--builders') + 1])
        self.assertFalse((self.root / 'terminated').exists())

    def test_check_realizes_outputs_without_running_the_package_vm(self):
        for cached, bootstrapped in (("1", False), ("", True)):
            with self.subTest(cached=cached):
                self.events.unlink(missing_ok=True)
                result, events = self.run_build("--check", CACHED=cached)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertTrue(any(e[:2] == ['nix', 'build'] and '--dry-run' not in e
                                    and '/nix/store/package-runner.drv^out' in e for e in events))
                self.assertFalse(any(e[0] == 'packages' for e in events))
                self.assertEqual(['started'] in events, bootstrapped)
                self.assertEqual(['poweroff'] in events, bootstrapped)

    def test_native_linux_does_not_bootstrap(self):
        result, events = self.run_build(host_system="aarch64-linux")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn(['started'], events)

    def test_build_failure_stops_bootstrap_and_does_not_run_packages(self):
        result, events = self.run_build(BUILD_FAIL="1")
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertIn(['poweroff'], events)
        self.assertFalse(any(e[0] == 'packages' for e in events))

    def test_failed_plan_is_not_treated_as_a_cache_miss(self):
        result, events = self.run_build(PLAN_FAIL="1")
        self.assertEqual(result.returncode, 9, result.stderr)
        self.assertIn('cache query failed', result.stderr)
        self.assertNotIn(['started'], events)

    def test_untrusted_user_fails_before_bootstrap(self):
        result, events = self.run_build(UNTRUSTED="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('trusted', result.stderr)
        self.assertNotIn(['started'], events)

    def test_boot_timeout_stops_owned_vm(self):
        result, events = self.run_build(BOOT_FAIL="1", NKE_BUILDER_BOOT_TIMEOUT="2")
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((self.root / 'terminated').exists(), result.stderr)
        self.assertFalse(any(e[0] == 'packages' for e in events))

    def test_unresponsive_vm_is_killed_within_the_bound(self):
        start = time.monotonic()
        result, events = self.run_build(BOOT_FAIL="1", NKE_BUILDER_BOOT_TIMEOUT="2",
                                        IGNORE_TERM="1", timeout=40)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / 'terminated').exists())
        self.assertLess(time.monotonic() - start, 25, result.stderr)
        with self.assertRaises(ProcessLookupError):
            os.kill(int((self.root / 'pid').read_text()), 0)
        self.assertFalse(any(e[0] == 'packages' for e in events))

    def test_bootstrap_disk_is_throwaway(self):
        # A persistent disk outlives a hard stop half-written; a later boot
        # then fails on its own corrupt Nix database. Each run gets a fresh one.
        state = Path(os.path.realpath(self.env["NKE_BUILDER_STATE_DIR"])) / "bootstrap"
        for extra in ({}, {"BUILD_FAIL": "1"}):
            with self.subTest(**extra):
                for marker in ('disks', 'started', 'stop'):
                    (self.root / marker).unlink(missing_ok=True)
                self.events.unlink(missing_ok=True)
                result, events = self.run_build(**extra)
                self.assertIn(['started'], events, result.stderr)
                disks = (self.root / 'disks').read_text().splitlines()
                self.assertEqual(len(disks), 1, disks)
                disk = Path(disks[0])
                self.assertNotEqual(disk, state / 'root.qcow2')
                self.assertIn(state, disk.parents)
                self.assertNotEqual(disk.parent, state)
                self.assertFalse(disk.exists(), disk)
                self.assertFalse(disk.parent.exists(), disk.parent)

    def test_configured_builder_for_the_guest_skips_bootstrap(self):
        # nix-darwin's linux-builder, say: it works for untrusted users too, so
        # neither the trust check nor a temporary VM may get in its way.
        machines = self.root / "machines"
        machines.write_text("# nix-darwin\n\nssh-ng://builder@linux-builder x86_64-linux,aarch64-linux"
                            " /etc/nix/builder_ed25519 4 1 kvm,benchmark,big-parallel - KEY\n")
        for builders in ("ssh-ng://builder@linux-builder aarch64-linux /etc/nix/builder_ed25519 4 - - - KEY",
                         "ssh://other x86_64-linux ; ssh-ng://b aarch64-linux",
                         f"@{machines}"):
            with self.subTest(builders=builders):
                self.events.unlink(missing_ok=True)
                result, events = self.run_build(BUILDERS=builders, UNTRUSTED="1")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn(['started'], events)
                self.assertFalse(any(e[:3] == ['nix', 'store', 'info'] for e in events))
                build = next(e for e in events if e[:2] == ['nix', 'build'] and '--dry-run' not in e
                             and '/nix/store/package-runner.drv^out' in e)
                self.assertNotIn('--builders', build)
                self.assertEqual(events[-1][0], 'packages')

    def test_builder_for_another_system_still_bootstraps(self):
        for builders in ("ssh://other x86_64-linux - 4", f"@{self.root / 'absent'}"):
            with self.subTest(builders=builders):
                for marker in ('started', 'stop'):
                    (self.root / marker).unlink(missing_ok=True)
                self.events.unlink(missing_ok=True)
                result, events = self.run_build(BUILDERS=builders)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(['started'], events)

    def test_port_already_listening_fails_before_launch(self):
        result, events = self.run_build(SQUAT_BEFORE="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("already listening", result.stderr)
        self.assertNotIn(['started'], events)

    def test_listener_other_than_the_vm_stops_before_building(self):
        # The guest's host key is public, so SSH alone cannot tell the VM from
        # another local process on the port; that process must get no build.
        result, events = self.run_build(SQUAT_AFTER="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not the bootstrap VM", result.stderr)
        self.assertFalse(any(e[:2] == ['nix', 'build'] and '--builders' in e for e in events))
        self.assertNotIn(['poweroff'], events)
        self.assertTrue((self.root / 'terminated').exists(), result.stderr)
        self.assertFalse(any(e[0] == 'packages' for e in events))

    def test_stale_runtime_from_a_hard_stop_is_removed(self):
        stale = Path(self.env["NKE_BUILDER_STATE_DIR"]) / "bootstrap" / "runtime.STALE123"
        stale.mkdir(parents=True)
        (stale / "root.qcow2").write_text("half-written")
        result, _ = self.run_build(CACHED="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(stale.exists())

    def test_concurrent_run_fails_fast(self):
        state = Path(self.env["NKE_BUILDER_STATE_DIR"]) / "bootstrap"
        state.mkdir(parents=True)
        with open(state / "build.lock", "w") as held:
            fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result, events = self.run_build()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("already in use", result.stderr)
        self.assertNotIn(['started'], events)

    def test_killed_driver_leaves_the_lock_with_the_running_vm(self):
        self.addCleanup(lambda: (self.root / 'stop').touch())
        lock = self.root / "state with spaces" / "bootstrap" / "build.lock"
        proc = subprocess.Popen(self.args(), env=self.env | {"BOOT_FAIL": "1",
                                "NKE_BUILDER_BOOT_TIMEOUT": "60"})
        self.addCleanup(proc.wait)
        for _ in range(500):
            if (self.root / 'started').exists(): break
            time.sleep(0.02)
        proc.kill()
        proc.wait(timeout=10)
        with open(lock) as held, self.assertRaises(BlockingIOError):
            fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
        (self.root / 'stop').touch()
        for _ in range(250):
            with open(lock) as free:
                try:
                    fcntl.flock(free, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    time.sleep(0.04)
        else:
            self.fail("the lock outlived the VM")


if __name__ == '__main__':
    unittest.main()
