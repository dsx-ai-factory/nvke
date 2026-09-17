# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Static publication-boundary checks for the OS-image workflow."""

from pathlib import Path
import unittest


WORKFLOW = (
    Path(__file__).resolve().parents[3]
    / ".github"
    / "workflows"
    / "os-image-artifact.yml"
)


class OSImageWorkflowTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = WORKFLOW.read_text(encoding="utf-8")

    def step(self, start: str, end: str) -> str:
        return self.workflow.split(start, 1)[1].split(end, 1)[0]

    def test_workflow_collects_make_finalized_bytes_without_reconversion(self):
        self.assertNotIn("qemu-img convert", self.workflow)
        collect = self.step(
            "- name: Validate and collect the finalized qcow2s",
            "- name: Push the qcow2s to GHCR",
        )
        self.assertIn("finalize-qcow2.py verify", collect)
        self.assertIn("EXPECTED_SOURCE_COMMIT: ${{ github.sha }}", collect)
        self.assertIn('actual_source_commit="$(git rev-parse HEAD)"', collect)
        self.assertIn('"${actual_source_commit}" != \\', collect)
        self.assertIn('"${EXPECTED_SOURCE_COMMIT}" ]]; then', collect)
        self.assertIn("expected_source_tree=\"$(git rev-parse \\", collect)
        self.assertIn('"${EXPECTED_SOURCE_COMMIT}^{tree}")"', collect)
        self.assertIn('--expected-variant "${variant}"', collect)
        self.assertIn('--expected-arch "${ARCH}"', collect)
        self.assertIn('--expected-source-commit "${EXPECTED_SOURCE_COMMIT}"', collect)
        self.assertIn('--expected-source-tree "${expected_source_tree}"', collect)
        self.assertIn("--required-accelerator kvm", collect)
        self.assertIn('cp -- "${src}" "dist/${name}"', collect)
        self.assertIn('cmp --silent "${src}" "dist/${name}"', collect)

    def test_build_manifest_remains_an_unpublished_sidecar_in_b1(self):
        publish = self.step(
            "- name: Push the qcow2s to GHCR",
            "- name: Show published manifests",
        )
        release_assets = self.step(
            "- name: Prepare release assets",
            "- name: Upload release assets",
        )
        self.assertNotIn("build-manifest", publish)
        self.assertNotIn("build-manifest", release_assets)
        self.assertIn('"${disk}:application/vnd.qemu.qcow2"', publish)


if __name__ == "__main__":
    unittest.main()
