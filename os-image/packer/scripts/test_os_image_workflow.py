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
README = Path(__file__).resolve().parents[2] / "README.md"


class OSImageWorkflowTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = WORKFLOW.read_text(encoding="utf-8")
        cls.readme = README.read_text(encoding="utf-8")

    def step(self, start: str, end: str) -> str:
        return self.workflow.split(start, 1)[1].split(end, 1)[0]

    def test_workflow_collects_make_finalized_bytes_without_reconversion(self):
        self.assertNotIn("qemu-img convert", self.workflow)
        collect = self.step(
            "- name: Collect the finalized qcow2s",
            "- name: Push the qcow2s to GHCR",
        )
        lane_start = 'if [[ "${{ matrix.build_fde }}" == "true" ]]; then'
        self.assertIn("EXPECTED_SOURCE_COMMIT: ${{ github.sha }}", collect)
        self.assertIn("mkdir -p dist", collect)
        self.assertIn(lane_start, collect)
        fde_lane, non_fde_lane = collect.split(lane_start, 1)[1].split("fi", 1)
        self.assertIn("collect-qcow2-fde", fde_lane)
        self.assertIn('echo "fde=${fde_name}" >> "$GITHUB_OUTPUT"', fde_lane)
        self.assertNotIn("collect-qcow2-non-fde", fde_lane)
        self.assertIn("collect-qcow2-non-fde", non_fde_lane)
        self.assertIn(
            'echo "non_fde=${non_fde_name}" >> "$GITHUB_OUTPUT"',
            non_fde_lane,
        )
        self.assertEqual(
            collect.count(
                "make --no-print-directory --silent -C os-image/packer"
            ),
            2,
        )
        self.assertEqual(collect.count("ARCH=${{ matrix.arch }}"), 2)
        self.assertEqual(collect.count('QCOW2_DIST_DIR="$PWD/dist"'), 2)
        self.assertNotIn("cp --", collect)
        self.assertNotIn("cmp --silent", collect)

    def test_hosted_builds_pin_kvm_after_runner_admission(self):
        fde = self.step(
            "- name: Build the FDE node image",
            "- name: Validate FDE TPM unlock (demo-fde.sh)",
        )
        non_fde = self.step(
            "- name: Build the non-FDE node image",
            "- name: Boot-smoke the non-FDE qcow2",
        )
        for build in (fde, non_fde):
            self.assertIn("ARCH=${{ matrix.arch }} ACCELERATOR=kvm", build)

    def test_no_build_manifest_contract_is_claimed_or_published(self):
        for content in (self.workflow, self.readme):
            self.assertNotIn("build-manifest", content)
            self.assertNotIn("sidecar", content)
        self.assertNotIn("finalize-qcow2.py", self.workflow)

    def test_publication_and_release_payloads_remain_qcow2_only(self):
        publish = self.step(
            "- name: Push the qcow2s to GHCR",
            "- name: Show published manifests",
        )
        release_assets = self.step(
            "- name: Prepare release assets",
            "- name: Upload release assets",
        )
        self.assertIn('"${disk}:application/vnd.qemu.qcow2"', publish)
        self.assertIn('disks=("${DISK_NON_FDE}")', release_assets)
        self.assertIn('disks+=("${DISK_FDE}")', release_assets)


if __name__ == "__main__":
    unittest.main()
