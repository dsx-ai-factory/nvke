# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise provision.sh's release gate without touching the host."""

import os
from pathlib import Path
import shutil
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


class ProvisionRunsTheImageTestsTest(unittest.TestCase):
    """Where provision.sh runs image-tests/test_ubuntu_snapshot.py (#2206)."""

    def setUp(self):
        self.lines = SCRIPT.read_text().splitlines()

    def index(self, needle):
        hits = [i for i, line in enumerate(self.lines) if needle in line and not line.lstrip().startswith("#")]
        self.assertEqual(len(hits), 1, f"{needle!r} at {hits}")
        return hits[0]

    def test_image_tests_run_after_cleanup_while_the_indexes_still_exist(self):
        tests = self.index("python3 /var/tmp/test_ubuntu_snapshot.py")
        self.assertLess(self.index("sudo apt-get -y --purge autoremove"), tests)
        self.assertLess(tests, self.index("sudo rm -rf /var/lib/apt/lists/*"))

    def test_the_image_tests_do_not_stay_in_the_image(self):
        self.assertLess(self.index("python3 /var/tmp/test_ubuntu_snapshot.py"),
                        self.index("rm -f /var/tmp/test_ubuntu_snapshot.py"))


SNAPSHOT = "https://snapshot.ubuntu.com/ubuntu/20261119T000000Z"
SNAPSHOT_SOURCES = f"""Types: deb
URIs: {SNAPSHOT}
Suites: noble noble-updates noble-backports
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg

Types: deb
URIs: {SNAPSHOT}
Suites: noble-security
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
"""


def python_apt():
    return bool(shutil.which("python3")) and subprocess.run(
        ["python3", "-c", "import apt_pkg"], capture_output=True, check=False).returncode == 0


# Skipped where python3-apt is missing (macOS), but never in CI, where a
# missing python3-apt has to fail rather than quietly drop these tests.
@unittest.skipUnless(python_apt() or os.environ.get("CI"), "the block reads the sources with python3-apt")
class BootSmokeSourcesTest(unittest.TestCase):
    """boot-smoke's apt-sources block, with this host's python3-apt reading fixture sources (#2206)."""

    def run_block(self, files, expected=""):
        text = SCRIPT.with_name("boot-smoke.sh").read_text()
        block = text.split("# BEGIN apt-sources-check", 1)[1].split("# END apt-sources-check", 1)[0]
        block = block.split("\n", 1)[1]  # the rest of the BEGIN line is prose
        with tempfile.TemporaryDirectory() as tmp:
            etc = Path(tmp) / "apt"
            (etc / "sources.list.d").mkdir(parents=True)
            for name, content in files.items():
                (etc / name).write_text(content)
            config = Path(tmp) / "apt.conf"
            config.write_text(f'Dir::Etc::sourcelist "{etc}/sources.list";\n'
                              f'Dir::Etc::sourceparts "{etc}/sources.list.d";\n')
            script = "set -euo pipefail\nexpected_snapshot=$1\n" + block
            return subprocess.run(["bash", "-c", script, "boot-smoke", expected],
                                  env={**os.environ, "APT_CONFIG": str(config)},
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

    def assertFailsNaming(self, result, uri):
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(uri, result.stderr)

    def test_snapshot_and_local_repo_pass(self):
        for expected in (SNAPSHOT, ""):
            with self.subTest(expected=expected):
                result = self.run_block(self.image(), expected)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_disabled_stanza_passes(self):
        disabled = "Enabled: no\nTypes: deb\nURIs: http://ports.ubuntu.com/ubuntu-ports\nSuites: noble\nComponents: main\n"
        result = self.run_block(self.image(**{"sources.list.d/off.sources": disabled}), SNAPSHOT)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_live_archive_fails(self):
        live = "deb http://ports.ubuntu.com/ubuntu-ports noble main\n"
        self.assertFailsNaming(self.run_block(self.image(**{"sources.list.d/extra.list": live})),
                               "ports.ubuntu.com")

    def test_live_archive_on_a_continued_uris_line_fails(self):
        continued = (f"Types: deb\nURIs: {SNAPSHOT}\n http://ports.ubuntu.com/ubuntu-ports\n"
                     "Suites: noble\nComponents: main\n")
        self.assertFailsNaming(self.run_block(self.image(**{"sources.list.d/extra.sources": continued}), SNAPSHOT),
                               "ports.ubuntu.com")

    def test_any_other_repository_fails(self):
        # Neither the snapshot nor the local repo: every source must be one of the two.
        other = "Types: deb\nURIs: http://mirror.example.org/ubuntu\nSuites: noble\nComponents: main\n"
        self.assertFailsNaming(self.run_block(self.image(**{"sources.list.d/mirror.sources": other})),
                               "mirror.example.org")

    def test_other_snapshot_id_fails_when_one_is_expected(self):
        self.assertFailsNaming(self.run_block(self.image(), "https://snapshot.ubuntu.com/ubuntu/20200101T000000Z"),
                               "20261119T000000Z")

    def test_no_snapshot_source_fails(self):
        # The image must keep the snapshot as its Ubuntu source, not just avoid others.
        for expected in (SNAPSHOT, ""):
            with self.subTest(expected=expected):
                self.assertFailsNaming(self.run_block(self.image(**{"sources.list.d/ubuntu.sources": ""}), expected),
                                       "no apt source is the Ubuntu snapshot")

    def test_second_snapshot_beside_the_expected_one_fails(self):
        older = "deb https://snapshot.ubuntu.com/ubuntu/20261118T000000Z noble main\n"
        self.assertFailsNaming(self.run_block(self.image(**{"sources.list.d/older.list": older}), SNAPSHOT),
                               "20261118T000000Z")


class PackerUploadsTheImageTestsTest(unittest.TestCase):
    def test_each_variant_uploads_the_image_tests_and_passes_the_snapshot(self):
        for variant in ("non-fde", "fde"):
            with self.subTest(variant=variant):
                text = (SCRIPT.parent.parent / f"nvke-node-{variant}.pkr.hcl").read_text()
                upload = text.index('destination = "/var/tmp/test_ubuntu_snapshot.py"')
                provision = text.index('script           = "${local.project_root}/scripts/provision.sh"')
                self.assertLess(upload, provision)
                self.assertIn('"UBUNTU_SNAPSHOT_URI=${local.ubuntu_snapshot_uri}"', text[upload:provision + 1])


if __name__ == "__main__":
    unittest.main()
