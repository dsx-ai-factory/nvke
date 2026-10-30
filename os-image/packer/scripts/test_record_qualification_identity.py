# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Build expectations must be captured independently from qualification hosts."""

import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True

SPEC = importlib.util.spec_from_file_location(
    "identity", Path(__file__).with_name("record-qualification-identity.py")
)
IDENTITY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(IDENTITY)


class IdentityTest(unittest.TestCase):
    def test_records_configured_identity_and_guest_kernel_for_each_architecture(self):
        for arch, machine in (("amd64", "x86_64"), ("arm64", "aarch64")):
            with self.subTest(arch=arch), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                output, stamp = root / "expected.json", root / "image-build-id"
                with patch.object(IDENTITY.platform, "freedesktop_os_release", return_value={"ID": "ubuntu", "VERSION_ID": "26.04"}), \
                     patch.object(IDENTITY.platform, "machine", return_value=machine), \
                     patch.object(IDENTITY.platform, "release", return_value="test-kernel"):
                    IDENTITY.record("build-123", "26.04", arch, output, stamp)
                self.assertEqual(stamp.read_text(), "build-123\n")
                expected = json.loads(output.read_text())
                self.assertEqual(expected["arch"], arch)
                self.assertEqual(expected["kernel_release"], "test-kernel")
                self.assertEqual(expected["build_id"], "build-123")

    def test_wrong_build_guest_fails_without_stamping_it(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with patch.object(IDENTITY.platform, "freedesktop_os_release", return_value={"ID": "ubuntu", "VERSION_ID": "24.04"}), \
                 patch.object(IDENTITY.platform, "machine", return_value="x86_64"):
                with self.assertRaisesRegex(ValueError, "does not match"):
                    IDENTITY.record("build-123", "26.04", "amd64", root / "manifest", root / "stamp")
            self.assertEqual(list(root.iterdir()), [])

    def test_existing_manifest_is_not_replaced_or_stamped(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            output = root / "manifest.json"
            output.write_text("existing evidence\n")
            with patch.object(IDENTITY.platform, "freedesktop_os_release", return_value={"ID": "ubuntu", "VERSION_ID": "24.04"}), \
                 patch.object(IDENTITY.platform, "machine", return_value="x86_64"):
                with self.assertRaises(FileExistsError):
                    IDENTITY.record("build-123", "24.04", "amd64", output, root / "stamp")
            self.assertEqual(output.read_text(), "existing evidence\n")
            self.assertFalse((root / "stamp").exists())


if __name__ == "__main__":
    unittest.main()
