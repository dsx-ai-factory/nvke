# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise a cold bundle build against local release archives."""

import hashlib
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


class BundleTest(unittest.TestCase):
    def test_cold_build_stages_each_architecture(self):
        script = Path(__file__).with_name("bundle.sh")
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            locks = root / "lock"
            locks.mkdir()
            rows = []
            components = (
                "spire-agent",
                "tpm_attestor_agent",
                "get_tpm_pubhash",
                "dsx-imds-node-attestor-agent",
            )
            for arch in ("amd64", "arm64"):
                for component in components:
                    payload = f"{component}/{arch}\n".encode()
                    if component == "spire-agent":
                        name = f"spire-1.14.1-linux-{arch}-musl.tar.gz"
                        release = root / "spiffe/spire/releases/download/v1.14.1"
                        member = "spire-1.14.1/bin/spire-agent"
                    elif component == "dsx-imds-node-attestor-agent":
                        release = root / "dsx-imds-node-attestor-agent" / f"linux-{arch}"
                        release.mkdir(parents=True, exist_ok=True)
                        archive = release / component
                        archive.write_bytes(payload)
                        digest = hashlib.sha256(payload).hexdigest()
                        rows.append(f"artifact {component} {arch} {digest} {archive.as_uri()}\n")
                        continue
                    else:
                        name = f"spire_tpm_plugin_{component}_linux_{arch}_v1.9.0.tar.gz"
                        release = root / "spiffe/spire-tpm-plugin/releases/download/v1.9.0"
                        member = component
                    release.mkdir(parents=True, exist_ok=True)
                    archive = release / name
                    with tarfile.open(archive, "w:gz") as tar:
                        info = tarfile.TarInfo(member)
                        info.size = len(payload)
                        tar.addfile(info, io.BytesIO(payload))
                    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
                    if component == "spire-agent":
                        checksum_name = f"spire-1.14.1-linux-{arch}-musl_sha256sum.txt"
                        (release / checksum_name).write_text(f"{digest}  {name}\n")
                    rows.append(f"artifact {component} {arch} {digest} {archive.as_uri()}\n")
            (locks / "1.14.1+tpm1.9.0.lock").write_text("".join(rows))
            for arch in ("amd64", "arm64"):
                with self.subTest(arch=arch):
                    result = subprocess.run(
                        ["bash", str(script), "--spire-version", "1.14.1",
                         "--tpm-version", "1.9.0", "--arch", arch,
                         "--dist", str(root / "dist"), "--lock-dir", str(locks)],
                        env={**os.environ, "BUNDLE_GITHUB_URL": root.as_uri(),
                             "BUNDLE_ASSETS_URL": root.as_uri()},
                        capture_output=True, text=True, check=False,
                    )
                    self.assertEqual(result.returncode, 0, result.stderr)
                    for component in components:
                        binary = root / "dist" / "1.14.1+tpm1.9.0" / arch / component
                        self.assertEqual(binary.read_text(), f"{component}/{arch}\n")
                        self.assertTrue(os.access(binary, os.X_OK))


if __name__ == "__main__":
    unittest.main()
