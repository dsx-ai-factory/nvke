# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise provision.sh's release gate without touching the host."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("provision.sh")


class ProvisionSeriesGateTest(unittest.TestCase):
    def invoke(self, root, *, requested, installed, snapshot_uri="https://snapshot.ubuntu.com/ubuntu/20261119T000000Z"):
        release_file = root / "os-release"
        release_file.write_text(f'ID=ubuntu\nVERSION_ID="{installed}"\n')
        tools = root / "tools"
        tools.mkdir()
        # The first sudo call marks the gate as passed; stop there.
        sudo = tools / "sudo"
        sudo.write_text('#!/usr/bin/env bash\necho reached-sudo >&2\nexit 97\n')
        sudo.chmod(0o755)
        env = {**os.environ, "PATH": f"{tools}{os.pathsep}{os.environ['PATH']}",
               "ROOT_FS_LABEL": "root", "UBUNTU_SERIES": requested,
               "OS_RELEASE": str(release_file)}
        env.pop("UBUNTU_SNAPSHOT_URI", None)
        if snapshot_uri is not None:
            env["UBUNTU_SNAPSHOT_URI"] = snapshot_uri
        return subprocess.run(["bash", str(SCRIPT)], env=env, capture_output=True, text=True, check=False)

    def test_missing_snapshot_uri_fails_before_provisioning(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = self.invoke(Path(tmp), requested="26.04", installed="26.04", snapshot_uri=None)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("UBUNTU_SNAPSHOT_URI", result.stderr)
            self.assertNotIn("reached-sudo", result.stderr)

    def test_mismatched_series_fails_before_provisioning(self):
        cases = [("26.04", "24.04"), ("24.04", "26.04")]
        for requested, installed in cases:
            with self.subTest(requested=requested, installed=installed), tempfile.TemporaryDirectory() as tmp:
                result = self.invoke(Path(tmp), requested=requested, installed=installed)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn(f"ubuntu_series is {requested}, but the installed system is Ubuntu {installed}",
                              result.stderr)
                self.assertNotIn("reached-sudo", result.stderr)

    def test_matching_series_proceeds(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = self.invoke(Path(tmp), requested="26.04", installed="26.04")
            self.assertEqual(result.returncode, 97, result.stderr)
            self.assertIn("reached-sudo", result.stderr)


class ProvisionSnapshotChecksTest(unittest.TestCase):
    """Where the snapshot checks sit in provision.sh (#2206)."""

    def setUp(self):
        self.lines = SCRIPT.read_text().splitlines()

    def index(self, needle):
        hits = [i for i, line in enumerate(self.lines) if needle in line and not line.lstrip().startswith("#")]
        self.assertEqual(len(hits), 1, f"{needle!r} at {hits}")
        return hits[0]

    def test_sources_are_checked_before_provisioning_installs_from_them(self):
        sources = self.index("check-ubuntu-snapshot.py sources")
        update = self.lines.index("sudo apt-get update")
        self.assertLess(update, sources)
        self.assertLess(sources, self.index("apt-get install -y doca-openvswitch-switch"))
        self.assertIn('--snapshot-uri "${UBUNTU_SNAPSHOT_URI}"', self.lines[sources])

    def test_versions_are_checked_while_the_indexes_still_exist(self):
        versions = self.index("check-ubuntu-snapshot.py versions")
        self.assertLess(self.index("sudo apt-get clean"), versions)
        self.assertLess(self.index("sudo apt-get -y --purge autoremove"), versions)
        self.assertLess(versions, self.index("sudo rm -rf /var/lib/apt/lists/*"))
        self.assertIn("--exempt nvke-agent", self.lines[versions])

    def test_the_uploaded_check_does_not_stay_in_the_image(self):
        self.assertLess(self.index("check-ubuntu-snapshot.py versions"),
                        self.index("rm -f /var/tmp/check-ubuntu-snapshot.py"))


SNAPSHOT_SOURCES = """Types: deb
URIs: https://snapshot.ubuntu.com/ubuntu/20261119T000000Z
Suites: noble noble-updates noble-backports
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg

Types: deb
URIs: https://snapshot.ubuntu.com/ubuntu/20261119T000000Z
Suites: noble-security
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
"""


class BootSmokeSourcesTest(unittest.TestCase):
    """boot-smoke's apt-sources block, run on fixture /etc/apt trees (#2206)."""

    def run_block(self, files, expected=""):
        text = SCRIPT.with_name("boot-smoke.sh").read_text()
        block = text.split("# BEGIN apt-sources-check", 1)[1].split("# END apt-sources-check", 1)[0]
        block = block.split("\n", 1)[1]  # the rest of the BEGIN line is prose
        with tempfile.TemporaryDirectory() as tmp:
            apt = Path(tmp) / "apt"
            (apt / "sources.list.d").mkdir(parents=True)
            for name, content in files.items():
                (apt / name).write_text(content)
            script = "set -euo pipefail\nexpected_snapshot=$1\n" + block.replace("/etc/apt", str(apt))
            return subprocess.run(["bash", "-c", script, "boot-smoke", expected],
                                  capture_output=True, text=True, check=False)

    def image(self, **extra):
        # What a real image carries: 24.04's comment-only sources.list, the
        # rendered snapshot template, the local repo, curtin's ignored backup.
        files = {"sources.list": "# Ubuntu sources have moved to /etc/apt/sources.list.d/ubuntu.sources\n",
                 "sources.list.d/ubuntu.sources": SNAPSHOT_SOURCES,
                 "sources.list.d/nke-local.list": "deb [trusted=yes] file:/usr/lib/nvke/debs ./\n",
                 "sources.list.d/ubuntu.sources.curtin.orig": "URIs: http://ports.ubuntu.com/ubuntu-ports/\n"}
        files.update(extra)
        return files

    def test_snapshot_and_local_repo_pass(self):
        result = self.run_block(self.image(), "https://snapshot.ubuntu.com/ubuntu/20261119T000000Z")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_live_archive_fails(self):
        live = "deb http://ports.ubuntu.com/ubuntu-ports noble main\n"
        result = self.run_block(self.image(**{"sources.list.d/extra.list": live}))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ports.ubuntu.com", result.stderr)

    def test_any_other_repository_fails(self):
        # Neither the snapshot nor the local repo: every source must be one of the two.
        other = "Types: deb\nURIs: http://mirror.example.org/ubuntu\nSuites: noble\nComponents: main\n"
        result = self.run_block(self.image(**{"sources.list.d/mirror.sources": other}))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("mirror.example.org", result.stderr)

    def test_other_snapshot_id_fails_when_one_is_expected(self):
        result = self.run_block(self.image(), "https://snapshot.ubuntu.com/ubuntu/20200101T000000Z")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("20200101T000000Z", result.stderr)


class PackerUploadsTheCheckTest(unittest.TestCase):
    def test_each_variant_uploads_the_check_and_passes_the_snapshot(self):
        for variant in ("non-fde", "fde"):
            with self.subTest(variant=variant):
                text = (SCRIPT.parent.parent / f"nvke-node-{variant}.pkr.hcl").read_text()
                upload = text.index('destination = "/var/tmp/check-ubuntu-snapshot.py"')
                provision = text.index('script           = "${local.project_root}/scripts/provision.sh"')
                self.assertLess(upload, provision)
                self.assertIn('"UBUNTU_SNAPSHOT_URI=${local.ubuntu_snapshot_uri}"', text[upload:provision + 1])


if __name__ == "__main__":
    unittest.main()
