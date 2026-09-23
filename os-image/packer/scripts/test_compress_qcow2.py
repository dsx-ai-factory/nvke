# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Contract tests for the single source-owned QCOW2 conversion."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest


SCRIPT = Path(__file__).with_name("compress-qcow2.sh")


class CompressQCOW2Test(unittest.TestCase):
    @staticmethod
    def make_qemu_img(path: Path) -> Path:
        tool = path / "qemu-img"
        tool.write_text(
            textwrap.dedent(
                """\
                #!/usr/bin/env python3
                import json
                import os
                from pathlib import Path
                import sys

                with open(os.environ["QEMU_LOG"], "a", encoding="utf-8") as stream:
                    stream.write(json.dumps(sys.argv[1:]) + "\\n")

                if sys.argv[1:5] == ["convert", "-c", "-O", "qcow2"]:
                    source = Path(sys.argv[5])
                    destination = Path(sys.argv[6])
                    destination.write_bytes(b"compressed:" + source.read_bytes())
                    raise SystemExit(0)
                if sys.argv[1:3] == ["info", "--output=json"]:
                    print(os.environ["QEMU_INFO"])
                    raise SystemExit(0)
                print("unexpected qemu-img arguments", file=sys.stderr)
                raise SystemExit(9)
                """
            ),
            encoding="utf-8",
        )
        tool.chmod(0o755)
        return tool

    def invoke(self, root: Path, metadata):
        tools = root / "tools"
        tools.mkdir()
        self.make_qemu_img(tools)
        image = root / "image.qcow2"
        image.write_bytes(b"original")
        log = root / "qemu.log"
        info = metadata if isinstance(metadata, str) else json.dumps(metadata)
        result = subprocess.run(
            [str(SCRIPT), str(image)],
            env={
                **os.environ,
                "PATH": f"{tools}{os.pathsep}{os.environ['PATH']}",
                "PYTHON": sys.executable,
                "QEMU_INFO": info,
                "QEMU_LOG": str(log),
            },
            check=False,
            capture_output=True,
            text=True,
        )
        calls = [
            json.loads(line) for line in log.read_text(encoding="utf-8").splitlines()
        ]
        return result, image, calls

    def test_validates_the_converted_qcow2_before_replacing_the_source(self):
        metadata = {
            "format": "qcow2",
            "virtual-size": 4096,
            "encrypted": False,
            "backing-filename": "",
            "full-backing-filename": "",
        }
        with tempfile.TemporaryDirectory() as tmp:
            result, image, calls = self.invoke(Path(tmp), metadata)

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(image.read_bytes(), b"compressed:original")
            self.assertFalse(image.with_name("image.compressed.qcow2").exists())
            self.assertEqual([call[0] for call in calls], ["convert", "info"])
            self.assertEqual(calls[1][-1], str(image.with_name("image.compressed.qcow2")))

    def test_rejects_unsafe_or_invalid_metadata_without_replacing_source(self):
        cases = (
            ("not an object", [], "not a JSON object"),
            ("wrong format", {"format": "raw", "virtual-size": 4096}, "format"),
            ("missing size", {"format": "qcow2"}, "positive integer"),
            ("boolean size", {"format": "qcow2", "virtual-size": True}, "positive integer"),
            ("zero size", {"format": "qcow2", "virtual-size": 0}, "positive integer"),
            (
                "encrypted",
                {"format": "qcow2", "virtual-size": 4096, "encrypted": True},
                "container encryption",
            ),
            (
                "non-boolean encryption marker",
                {"format": "qcow2", "virtual-size": 4096, "encrypted": 0},
                "container encryption",
            ),
            (
                "backing file",
                {"format": "qcow2", "virtual-size": 4096, "backing-filename": "base.qcow2"},
                "backing-filename",
            ),
            (
                "full backing file",
                {
                    "format": "qcow2",
                    "virtual-size": 4096,
                    "full-backing-filename": "/tmp/base.qcow2",
                },
                "full-backing-filename",
            ),
            (
                "null backing file",
                {"format": "qcow2", "virtual-size": 4096, "backing-filename": None},
                "backing-filename",
            ),
            ("invalid JSON", "not-json", "invalid JSON"),
        )
        for name, metadata, diagnostic in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as tmp:
                result, image, calls = self.invoke(Path(tmp), metadata)

                self.assertNotEqual(result.returncode, 0)
                self.assertIn(diagnostic, result.stderr)
                self.assertNotIn("Traceback", result.stderr)
                self.assertEqual(image.read_bytes(), b"original")
                self.assertFalse(image.with_name("image.compressed.qcow2").exists())
                self.assertEqual([call[0] for call in calls], ["convert", "info"])


if __name__ == "__main__":
    unittest.main()
