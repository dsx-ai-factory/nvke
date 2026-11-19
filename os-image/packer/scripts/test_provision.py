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

    def test_sources_are_checked_before_anything_is_installed_from_them(self):
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
