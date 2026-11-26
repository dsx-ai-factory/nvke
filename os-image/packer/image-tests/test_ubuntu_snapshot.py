#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Image tests: every Ubuntu package comes from the build's snapshot (#2206).

These test the image, not this repository. provision.sh runs them inside the
build VM, while apt's indexes still exist, so an image that strays from the
snapshot fails the bake instead of reaching a node. They are temporary: the
release verify step (#2205) will check the same thing, and replaces them.

They have caught two real failures so far:

- With `updates: security`, 14 packages stayed at the installer ISO's
  versions, which the snapshot does not serve.
- On 24.04.5 arm64, curtin kept the live ports.ubuntu.com source, and the
  installer installed packages from it.

In the guest: UBUNTU_SNAPSHOT_URI=<base>/<ID> python3 test_ubuntu_snapshot.py
"""
import os
import unittest

import apt
import apt_pkg

SNAPSHOT = os.environ["UBUNTU_SNAPSHOT_URI"].rstrip("/") + "/"
LOCAL_REPO = "file:/usr/lib/nvke/debs/"
# Installed by dpkg from its own .deb, so no repository serves it.
NOT_FROM_A_REPOSITORY = {"nvke-agent"}


class UbuntuSnapshotTest(unittest.TestCase):
    maxDiff = None  # print every stray source or package, not a truncated diff

    def test_apt_reads_only_the_snapshot_and_the_local_repo(self):
        apt_pkg.init()
        sources = apt_pkg.SourceList()
        sources.read_main_list()
        uris = {index.uri.rstrip("/") + "/" for index in sources.list}
        self.assertEqual(sorted(uris - {SNAPSHOT, LOCAL_REPO}), [])

    def test_every_installed_version_is_served_by_the_snapshot_or_the_local_repo(self):
        stray = sorted(
            f"{pkg.shortname}:{pkg.installed.architecture}={pkg.installed.version}"
            for pkg in apt.Cache()
            if pkg.is_installed and pkg.shortname not in NOT_FROM_A_REPOSITORY
            and not any(uri.startswith((SNAPSHOT, LOCAL_REPO)) for uri in pkg.installed.uris))
        self.assertEqual(stray, [])


if __name__ == "__main__":
    unittest.main(verbosity=2)
