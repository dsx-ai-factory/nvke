# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Contract tests for the source-owned QCOW2 finalizer."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest


SCRIPT = Path(__file__).with_name("finalize-qcow2.py")
MANIFEST_SUFFIX = ".build-manifest.v1.json"


class FinalizerFixture:
    def __init__(self, root: Path):
        self.root = root
        self.source = root / "source"
        self.output = root / "output"
        self.tools = root / "tools"
        self.qemu_log = root / "qemu.log"
        self.source.mkdir()
        self.output.mkdir()
        self.tools.mkdir()
        (self.source / "tracked.txt").write_text("tracked\n", encoding="utf-8")
        (self.source / ".gitignore").write_text("ignored/\n", encoding="utf-8")
        self.run(["git", "init", "-q"], cwd=self.source)
        self.run(["git", "add", "tracked.txt", ".gitignore"], cwd=self.source)
        self.run(
            [
                "git",
                "-c",
                "user.name=Finalizer Test",
                "-c",
                "user.email=finalizer@example.invalid",
                "-c",
                "commit.gpgsign=false",
                "commit",
                "-qm",
                "fixture",
            ],
            cwd=self.source,
        )
        self.commit = self.run(
            ["git", "rev-parse", "HEAD"], cwd=self.source
        ).stdout.strip()
        self.tree = self.run(
            ["git", "rev-parse", "HEAD^{tree}"], cwd=self.source
        ).stdout.strip()
        self.packer = self.tools / "packer"
        self.packer.write_text(
            "#!/bin/sh\nprintf '%s\\n' 'Packer v1.14.1'\n", encoding="utf-8"
        )
        self.packer.chmod(0o755)
        self.qemu_img = self.tools / "qemu-img"
        self.qemu_img.write_text(
            textwrap.dedent(
                """\
                #!/usr/bin/env python3
                import json
                import os
                from pathlib import Path
                import sys

                log = os.environ.get("FAKE_QEMU_LOG")
                if log:
                    with open(log, "a", encoding="utf-8") as stream:
                        stream.write(json.dumps(sys.argv[1:]) + "\\n")
                if sys.argv[1:] == ["--version"]:
                    print("qemu-img version 9.2.4 (fixture)")
                    raise SystemExit(0)
                if sys.argv[1:3] == ["info", "--output=json"]:
                    info = {
                        "format": os.environ.get("FAKE_QEMU_FORMAT", "qcow2"),
                        "virtual-size": 1073741824,
                    }
                    if os.environ.get("FAKE_QEMU_BACKING"):
                        info["backing-filename"] = "base.qcow2"
                    if os.environ.get("FAKE_QEMU_ENCRYPTED"):
                        info["encrypted"] = True
                    print(json.dumps(info))
                    raise SystemExit(0)
                print("unexpected qemu-img arguments", file=sys.stderr)
                raise SystemExit(9)
                """
            ),
            encoding="utf-8",
        )
        self.qemu_img.chmod(0o755)

    @staticmethod
    def run(argv, *, cwd=None, env=None):
        return subprocess.run(
            argv,
            cwd=cwd,
            env=env,
            check=True,
            capture_output=True,
            text=True,
        )

    def image(self, variant: str, version: str, arch: str, payload: bytes) -> Path:
        image = self.output / f"nke-node-{variant}-ubuntu-{version}-{arch}.qcow2"
        image.write_bytes(payload)
        return image

    def commit_source_change(self) -> tuple[str, str]:
        (self.source / "tracked.txt").write_text("different tree\n", encoding="utf-8")
        self.run(["git", "add", "tracked.txt"], cwd=self.source)
        self.run(
            [
                "git",
                "-c",
                "user.name=Finalizer Test",
                "-c",
                "user.email=finalizer@example.invalid",
                "-c",
                "commit.gpgsign=false",
                "commit",
                "-qm",
                "second fixture",
            ],
            cwd=self.source,
        )
        commit = self.run(
            ["git", "rev-parse", "HEAD"], cwd=self.source
        ).stdout.strip()
        tree = self.run(
            ["git", "rev-parse", "HEAD^{tree}"], cwd=self.source
        ).stdout.strip()
        return commit, tree

    def invoke(
        self,
        *,
        variant="fde",
        arch="amd64",
        iso_arch=None,
        version="26.04.1",
        accelerator="kvm",
        extra_env=None,
    ):
        env = {
            **os.environ,
            "FAKE_QEMU_LOG": str(self.qemu_log),
            "LUKS_PASSPHRASE": "must-not-appear-in-manifest",
            **(extra_env or {}),
        }
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "finalize",
                "--variant",
                variant,
                "--arch",
                arch,
                "--accelerator",
                accelerator,
                "--iso-url",
                f"https://example.invalid/ubuntu-{version}-live-server-{iso_arch or arch}.iso",
                "--output-dir",
                str(self.output),
                "--source-root",
                str(self.source),
                "--packer",
                str(self.packer),
                "--qemu-img",
                str(self.qemu_img),
            ],
            env=env,
            check=False,
            capture_output=True,
            text=True,
        )

    def check_source(self):
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "check-source",
                "--source-root",
                str(self.source),
            ],
            check=False,
            capture_output=True,
            text=True,
        )

    def verify(
        self,
        image: Path,
        manifest: Path,
        *,
        expected_variant="fde",
        expected_arch="amd64",
        expected_source_commit=None,
        expected_source_tree=None,
        required_accelerator="kvm",
    ):
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "verify",
                "--image",
                str(image),
                "--manifest",
                str(manifest),
                "--expected-variant",
                expected_variant,
                "--expected-arch",
                expected_arch,
                "--expected-source-commit",
                self.commit if expected_source_commit is None else expected_source_commit,
                "--expected-source-tree",
                self.tree if expected_source_tree is None else expected_source_tree,
                "--required-accelerator",
                required_accelerator,
                "--qemu-img",
                str(self.qemu_img),
            ],
            env={**os.environ, "FAKE_QEMU_LOG": str(self.qemu_log)},
            check=False,
            capture_output=True,
            text=True,
        )


class FinalizeQCOW2Test(unittest.TestCase):
    def test_finalizes_supported_outputs_and_emits_exact_identity(self):
        cases = (
            ("fde", "amd64", "kvm"),
            ("non-fde", "arm64", "tcg"),
        )
        for variant, arch, accelerator in cases:
            with self.subTest(variant=variant, arch=arch), tempfile.TemporaryDirectory() as tmp:
                fixture = FinalizerFixture(Path(tmp))
                payload = f"fixture-{variant}-{arch}".encode()
                image = fixture.image(variant, "26.04.1", arch, payload)
                image_inode = image.stat().st_ino

                result = fixture.invoke(
                    variant=variant, arch=arch, accelerator=accelerator
                )

                self.assertEqual(result.returncode, 0, result.stderr)
                manifest_path = image.with_name(image.name + MANIFEST_SUFFIX)
                manifest_text = manifest_path.read_text(encoding="utf-8")
                manifest = json.loads(manifest_text)
                self.assertEqual(image.read_bytes(), payload)
                self.assertEqual(image.stat().st_ino, image_inode)
                self.assertEqual(
                    manifest["apiVersion"],
                    "nke.nvidia.com/os-image-build-manifest/v1",
                )
                self.assertEqual(
                    manifest["build"],
                    {
                        "accelerator": accelerator,
                        "architecture": arch,
                        "target": f"os-image-{variant}",
                        "variant": variant,
                    },
                )
                self.assertEqual(manifest["output"]["fileName"], image.name)
                self.assertEqual(
                    manifest["output"]["sha256"],
                    "sha256:" + hashlib.sha256(payload).hexdigest(),
                )
                self.assertEqual(manifest["output"]["sizeBytes"], len(payload))
                self.assertEqual(
                    manifest["output"]["virtualSizeBytes"], 1073741824
                )
                self.assertEqual(manifest["tools"]["packer"]["version"], "1.14.1")
                self.assertEqual(manifest["tools"]["qemuImg"]["version"], "9.2.4")
                self.assertRegex(manifest["source"]["commit"], r"^[0-9a-f]{40}$")
                self.assertRegex(manifest["source"]["tree"], r"^[0-9a-f]{40}$")
                self.assertFalse(manifest["source"]["dirty"])
                self.assertNotIn(str(Path(tmp)), manifest_text)
                self.assertNotIn("must-not-appear-in-manifest", manifest_text)
                verified = fixture.verify(
                    image,
                    manifest_path,
                    expected_variant=variant,
                    expected_arch=arch,
                    required_accelerator=accelerator,
                )
                self.assertEqual(verified.returncode, 0, verified.stderr)

                qemu_calls = [
                    json.loads(line)
                    for line in fixture.qemu_log.read_text(encoding="utf-8").splitlines()
                ]
                self.assertFalse(
                    any(call[0] == "convert" for call in qemu_calls),
                    "the manifest finalizer must not recompress Packer's output",
                )

    def test_source_preflight_accepts_clean_source_and_rejects_dirty_source(self):
        with tempfile.TemporaryDirectory() as tmp:
            fixture = FinalizerFixture(Path(tmp))

            clean = fixture.check_source()

            self.assertEqual(clean.returncode, 0, clean.stderr)
            self.assertIn(fixture.commit, clean.stdout)
            self.assertIn(fixture.tree, clean.stdout)

            ignored = fixture.source / "ignored" / "cache-entry"
            ignored.parent.mkdir()
            ignored.write_text("ignored\n", encoding="utf-8")
            ignored_result = fixture.check_source()

            self.assertEqual(ignored_result.returncode, 0, ignored_result.stderr)

            (fixture.source / "untracked.txt").write_text(
                "dirty\n", encoding="utf-8"
            )
            dirty = fixture.check_source()

            self.assertNotEqual(dirty.returncode, 0)
            self.assertIn("source checkout is dirty", dirty.stderr)

    def test_manifest_is_deterministic_for_identical_observations(self):
        with tempfile.TemporaryDirectory() as tmp:
            fixture = FinalizerFixture(Path(tmp))
            image = fixture.image("fde", "26.04.1", "amd64", b"same-bytes")
            first = fixture.invoke()
            self.assertEqual(first.returncode, 0, first.stderr)
            manifest = image.with_name(image.name + MANIFEST_SUFFIX)
            first_bytes = manifest.read_bytes()

            second = fixture.invoke()
            self.assertEqual(second.returncode, 0, second.stderr)
            self.assertEqual(manifest.read_bytes(), first_bytes)

    def test_rejects_ambiguous_or_wrong_packer_outputs(self):
        cases = ("no-output", "wrong-name", "extra-output", "iso-arch-mismatch")
        for case in cases:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as tmp:
                fixture = FinalizerFixture(Path(tmp))
                kwargs = {}
                if case == "wrong-name":
                    fixture.image("non-fde", "26.04.1", "amd64", b"wrong")
                elif case == "extra-output":
                    fixture.image("fde", "26.04.1", "amd64", b"one")
                    (fixture.output / "extra.qcow2").write_bytes(b"two")
                elif case == "iso-arch-mismatch":
                    fixture.image("fde", "26.04.1", "amd64", b"one")
                    kwargs = {"iso_arch": "arm64"}

                result = fixture.invoke(**kwargs)

                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(list(fixture.output.glob(f"*{MANIFEST_SUFFIX}")))

    def test_validation_failures_preserve_original_image(self):
        cases = (
            ("wrong-format", {"FAKE_QEMU_FORMAT": "raw"}),
            ("backing-file", {"FAKE_QEMU_BACKING": "1"}),
            ("encrypted-container", {"FAKE_QEMU_ENCRYPTED": "1"}),
        )
        for name, extra_env in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as tmp:
                fixture = FinalizerFixture(Path(tmp))
                image = fixture.image("fde", "26.04.1", "amd64", b"original")

                result = fixture.invoke(extra_env=extra_env)

                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(image.read_bytes(), b"original")
                self.assertFalse(
                    image.with_name(image.name + MANIFEST_SUFFIX).exists()
                )

    def test_dirty_source_fails_before_writing_manifest(self):
        with tempfile.TemporaryDirectory() as tmp:
            fixture = FinalizerFixture(Path(tmp))
            image = fixture.image("fde", "26.04.1", "amd64", b"original")
            (fixture.source / "untracked.txt").write_text("dirty\n", encoding="utf-8")

            result = fixture.invoke()

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("source checkout is dirty", result.stderr)
            self.assertEqual(image.read_bytes(), b"original")
            self.assertFalse(image.with_name(image.name + MANIFEST_SUFFIX).exists())
            self.assertFalse(fixture.qemu_log.exists())

    def test_verify_rejects_changed_final_bytes(self):
        with tempfile.TemporaryDirectory() as tmp:
            fixture = FinalizerFixture(Path(tmp))
            image = fixture.image("fde", "26.04.1", "amd64", b"original")
            result = fixture.invoke()
            self.assertEqual(result.returncode, 0, result.stderr)
            manifest = image.with_name(image.name + MANIFEST_SUFFIX)
            image.write_bytes(b"changed")

            verified = fixture.verify(image, manifest)

            self.assertNotEqual(verified.returncode, 0)
            self.assertIn("digest or size", verified.stderr)

    def test_verify_rejects_invalid_source_identity_without_traceback(self):
        cases = (("commit", 7, "source.commit"), ("dirty", True, "source.dirty"))
        for field, value, diagnostic in cases:
            with self.subTest(field=field), tempfile.TemporaryDirectory() as tmp:
                fixture = FinalizerFixture(Path(tmp))
                image = fixture.image("fde", "26.04.1", "amd64", b"original")
                result = fixture.invoke()
                self.assertEqual(result.returncode, 0, result.stderr)
                manifest_path = image.with_name(image.name + MANIFEST_SUFFIX)
                manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
                manifest["source"][field] = value
                manifest_path.write_text(json.dumps(manifest), encoding="utf-8")

                verified = fixture.verify(image, manifest_path)

                self.assertNotEqual(verified.returncode, 0)
                self.assertIn(diagnostic, verified.stderr)
                self.assertNotIn("Traceback", verified.stderr)

    def test_verify_binds_build_metadata_to_output_name(self):
        cases = (
            ("architecture", "arm64", "build.architecture"),
            ("variant", "non-fde", "build.variant"),
        )
        for field, value, diagnostic in cases:
            with self.subTest(field=field), tempfile.TemporaryDirectory() as tmp:
                fixture = FinalizerFixture(Path(tmp))
                image = fixture.image("fde", "26.04.1", "amd64", b"original")
                result = fixture.invoke()
                self.assertEqual(result.returncode, 0, result.stderr)
                manifest_path = image.with_name(image.name + MANIFEST_SUFFIX)
                manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
                manifest["build"][field] = value
                if field == "variant":
                    manifest["build"]["target"] = "os-image-non-fde"
                manifest_path.write_text(json.dumps(manifest), encoding="utf-8")

                verified = fixture.verify(image, manifest_path)

                self.assertNotEqual(verified.returncode, 0)
                self.assertIn(diagnostic, verified.stderr)

    def test_verify_rejects_valid_pair_from_another_matrix_lane(self):
        cases = (
            (
                "variant",
                {"variant": "non-fde"},
                {"expected_variant": "fde"},
                "build.variant",
            ),
            (
                "architecture",
                {"arch": "arm64"},
                {"expected_arch": "amd64"},
                "build.architecture",
            ),
            (
                "accelerator",
                {"accelerator": "tcg"},
                {"required_accelerator": "kvm"},
                "build.accelerator",
            ),
        )
        for name, build, expected, diagnostic in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as tmp:
                fixture = FinalizerFixture(Path(tmp))
                variant = build.get("variant", "fde")
                arch = build.get("arch", "amd64")
                accelerator = build.get("accelerator", "kvm")
                image = fixture.image(variant, "26.04.1", arch, b"valid-pair")
                result = fixture.invoke(
                    variant=variant, arch=arch, accelerator=accelerator
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                manifest = image.with_name(image.name + MANIFEST_SUFFIX)

                verified = fixture.verify(
                    image,
                    manifest,
                    expected_variant=expected.get("expected_variant", variant),
                    expected_arch=expected.get("expected_arch", arch),
                    required_accelerator=expected.get(
                        "required_accelerator", accelerator
                    ),
                )

                self.assertNotEqual(verified.returncode, 0)
                self.assertIn(diagnostic, verified.stderr)

    def test_verify_rejects_valid_pair_from_another_source_identity(self):
        with tempfile.TemporaryDirectory() as tmp:
            fixture = FinalizerFixture(Path(tmp))
            image = fixture.image("fde", "26.04.1", "amd64", b"valid-pair")
            result = fixture.invoke()
            self.assertEqual(result.returncode, 0, result.stderr)
            manifest = image.with_name(image.name + MANIFEST_SUFFIX)
            other_commit, other_tree = fixture.commit_source_change()

            wrong_source = fixture.verify(
                image,
                manifest,
                expected_source_commit=other_commit,
                expected_source_tree=other_tree,
            )
            wrong_tree = fixture.verify(
                image,
                manifest,
                expected_source_tree=other_tree,
            )

            self.assertNotEqual(wrong_source.returncode, 0)
            self.assertIn("source.commit", wrong_source.stderr)
            self.assertNotEqual(wrong_tree.returncode, 0)
            self.assertIn("source.tree", wrong_tree.stderr)

    def test_verify_rejects_symlink_inputs(self):
        for symlink_kind in ("image", "manifest"):
            with self.subTest(symlink=symlink_kind), tempfile.TemporaryDirectory() as tmp:
                fixture = FinalizerFixture(Path(tmp))
                image = fixture.image("fde", "26.04.1", "amd64", b"original")
                result = fixture.invoke()
                self.assertEqual(result.returncode, 0, result.stderr)
                manifest = image.with_name(image.name + MANIFEST_SUFFIX)
                selected = image if symlink_kind == "image" else manifest
                target = selected.with_name(selected.name + ".real")
                selected.rename(target)
                selected.symlink_to(target.name)

                verified = fixture.verify(image, manifest)

                self.assertNotEqual(verified.returncode, 0)
                self.assertIn("not a regular file", verified.stderr)


if __name__ == "__main__":
    unittest.main()
