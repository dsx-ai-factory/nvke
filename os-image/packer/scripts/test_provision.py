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
    def invoke(self, root, *, requested, installed):
        release_file = root / "os-release"
        release_file.write_text(f'ID=ubuntu\nVERSION_ID="{installed}"\n')
        tools = root / "tools"
        tools.mkdir()
        # The first sudo call marks the gate as passed; stop there.
        sudo = tools / "sudo"
        sudo.write_text('#!/usr/bin/env bash\necho reached-sudo >&2\nexit 97\n')
        sudo.chmod(0o755)
        return subprocess.run(
            ["bash", str(SCRIPT)],
            env={**os.environ, "PATH": f"{tools}{os.pathsep}{os.environ['PATH']}",
                 "ROOT_FS_LABEL": "root", "UBUNTU_SERIES": requested,
                 "OS_RELEASE": str(release_file)},
            capture_output=True, text=True, check=False,
        )

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


if __name__ == "__main__":
    unittest.main()
