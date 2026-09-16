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
                artifacts = root / "artifacts"
                result = subprocess.run(
                    ["bash", str(script)],
                    env={**os.environ, "PATH": f"{tools}:{os.environ['PATH']}",
                         "ARCH": arch, "ARTIFACTS_DIR": str(artifacts),
                         "NGC_API_KEY": ""},
                    capture_output=True, text=True, check=False,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual((artifacts / "nke-agent.deb").read_text(), arch)
                self.assertTrue((artifacts / "nke-agent-spire.conf").is_file())
                self.assertFalse((artifacts / "nke-agent").exists())
                self.assertFalse((artifacts / "nke-agent.service").exists())


if __name__ == "__main__":
    unittest.main()
