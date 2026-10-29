# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Check Packer's agent package staging without building the other bundles."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class AgentPackageTest(unittest.TestCase):
    def test_stages_agent_deb_for_each_architecture(self):
        script = Path(__file__).with_name("fetch-artifacts.sh")
        for arch in ("amd64", "arm64"):
            with self.subTest(arch=arch), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                tools = root / "tools"
                tools.mkdir()
                make = tools / "make"
                make.write_text('''#!/usr/bin/env bash
set -eu
if [[ "$2" == */agent ]]; then
  [[ "$3" == "deb-${ARCH}" ]] || { echo "expected agent Debian build" >&2; exit 1; }
  for arg in "$@"; do
    case "$arg" in
      DEB_OUTPUT=*) printf '%s' "${ARCH}" > "${arg#DEB_OUTPUT=}" ;;
    esac
  done
fi
''')
                make.chmod(0o755)
                machine = {"amd64": "x86_64", "arm64": "aarch64"}[arch]
                uname = tools / "uname"
                uname.write_text(f'#!/bin/sh\ncase "$1" in -s) echo Linux;; -m) echo {machine};; esac\n')
                uname.chmod(0o755)
                artifacts = root / "artifacts"
                result = subprocess.run(
                    ["bash", str(script)],
                    env={**os.environ, "PATH": f"{tools}:{os.environ['PATH']}",
                         "ARCH": arch, "ARTIFACTS_DIR": str(artifacts),
                         "NKE_TARGET_UBUNTU_SERIES": "26.04",
                         "SOURCE_ROOT": str(script.parents[3]),
                         "NGC_API_KEY": "", "NKE_PACKAGE_BUILDER": "host"},
                    capture_output=True, text=True, check=False,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual((artifacts / "nvke-agent.deb").read_text(), arch)
                self.assertTrue((artifacts / "nvke-agent-spire.conf").is_file())
                self.assertFalse((artifacts / "nvke-agent").exists())
                self.assertFalse((artifacts / "nvke-agent.service").exists())

    def test_rejects_source_root_that_does_not_own_the_script(self):
        script = Path(__file__).with_name("fetch-artifacts.sh")
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source = root / "source"
            source.mkdir()
            artifacts = root / "artifacts"

            result = subprocess.run(
                ["bash", str(script)],
                env={
                    **os.environ,
                    "ARCH": "amd64",
                    "ARTIFACTS_DIR": str(artifacts),
                    "NKE_TARGET_UBUNTU_SERIES": "26.04",
                    "SOURCE_ROOT": str(source),
                },
                capture_output=True,
                text=True,
                check=False,
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("does not own", result.stderr)
            self.assertFalse(artifacts.exists())


if __name__ == "__main__":
    unittest.main()
