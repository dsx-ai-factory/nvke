#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""The checks that keep an image on one Ubuntu snapshot, without apt or network."""
import importlib.util
from pathlib import Path
import subprocess
import sys
import unittest

SCRIPTS = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("check_ubuntu_snapshot", SCRIPTS / "check-ubuntu-snapshot.py")
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)

BASE = "https://snapshot.ubuntu.com/ubuntu"
SNAPSHOT_ID = "20261119T000000Z"
U = f"{BASE}/{SNAPSHOT_ID}"
LOCAL = "/usr/lib/nvke/debs"


def pkg(name, arch="arm64", version="1.0", uris=()):
    return CHECK.Installed(name, arch, version, tuple(uris))


class SourcesTest(unittest.TestCase):
    def test_normalize_adds_slash_and_lowercases_host(self):
        self.assertEqual(CHECK.normalize_uri("HTTPS://Snapshot.Ubuntu.com/ubuntu/2026X"),
                         "https://snapshot.ubuntu.com/ubuntu/2026X/")

    def test_bad_sources_accepts_snapshot_and_local_repo(self):
        self.assertEqual(CHECK.bad_sources([U, U + "/", "file:/usr/lib/nvke/debs/"], U, LOCAL), [])

    def test_bad_sources_reports_live_archive_and_other_snapshot(self):
        other = f"{BASE}/20250101T000000Z"
        live = ["http://archive.ubuntu.com/ubuntu/", "http://ports.ubuntu.com/ubuntu-ports/"]
        self.assertEqual(CHECK.bad_sources([U, *live, other], U, LOCAL), sorted([*live, other]))


class VersionsTest(unittest.TestCase):
    def test_superseded_iso_version_is_a_violation(self):
        # Installed from the ISO pool at a version the snapshot no longer
        # indexes: apt shows it as [installed,local], with no download URI.
        libc = pkg("libc6", version="2.43-2ubuntu1")
        self.assertEqual(CHECK.violations([libc], U, LOCAL, frozenset()), [libc])

    def test_snapshot_and_local_versions_pass(self):
        from_snapshot = pkg("bash", uris=[f"{U}/pool/main/b/bash/bash_5.2_arm64.deb"])
        from_local = pkg("spire-agent", uris=["file:/usr/lib/nvke/debs/spire-agent_1.0_arm64.deb"])
        self.assertEqual(CHECK.violations([from_snapshot, from_local], U, LOCAL, frozenset()), [])

    def test_exempt_name_passes_without_uris(self):
        self.assertEqual(CHECK.violations([pkg("nvke-agent", version="1.2.3")], U, LOCAL,
                                          frozenset({"nvke-agent"})), [])

    def test_prefix_match_requires_path_boundary(self):
        lookalike = pkg("bash", uris=[f"{U}0/pool/main/b/bash/bash_5.2_arm64.deb"])
        self.assertEqual(CHECK.violations([lookalike], U, LOCAL, frozenset()), [lookalike])

    def test_violations_sort_by_name_then_arch(self):
        found = CHECK.violations([pkg("zlib1g"), pkg("libc6", "arm64"), pkg("libc6", "all")],
                                 U, LOCAL, frozenset())
        self.assertEqual([(p.name, p.arch) for p in found],
                         [("libc6", "all"), ("libc6", "arm64"), ("zlib1g", "arm64")])

    def test_digest_is_order_independent_and_arch_sensitive(self):
        a, b = pkg("bash", version="5.2"), pkg("libc6", version="2.43")
        self.assertEqual(CHECK.package_set_digest([a, b]), CHECK.package_set_digest([b, a]))
        self.assertNotEqual(CHECK.package_set_digest([a, b]),
                            CHECK.package_set_digest([a, pkg("libc6", "amd64", "2.43")]))


class SnapshotIdTest(unittest.TestCase):
    def test_snapshot_id_problems_lists_failing_urls(self):
        seen = []

        def fetch(url):
            seen.append(url)
            return 401 if "noble-security" in url else 200

        problems = CHECK.snapshot_id_problems(fetch, BASE, SNAPSHOT_ID)
        self.assertEqual(problems, [f"{U}/dists/noble-security/InRelease"])
        self.assertEqual(len(seen), 10)
        self.assertIn(f"{U}/dists/resolute-updates/main/binary-arm64/Packages.xz", seen)

    def test_cli_id_rejects_malformed_id(self):
        result = subprocess.run(
            [sys.executable, str(SCRIPTS / "check-ubuntu-snapshot.py"), "id",
             "--base-url", BASE, "--snapshot-id", "2026-11-19"],
            capture_output=True, text=True, check=False,
        )
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("YYYYMMDDTHHMMSSZ", result.stderr)


if __name__ == "__main__":
    unittest.main()
