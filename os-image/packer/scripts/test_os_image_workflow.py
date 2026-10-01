# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Static publication-boundary checks for the OS-image workflow."""

from pathlib import Path
import re
import unittest


WORKFLOW = (
    Path(__file__).resolve().parents[3]
    / ".github"
    / "workflows"
    / "os-image-artifact.yml"
)
CACHE_WORKFLOW = WORKFLOW.with_name("os-image-cache.yml")
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
            "- name: Collect the finalized qcow2",
            "- name: Push the qcow2 to GHCR",
        )
        self.assertIn("EXPECTED_SOURCE_COMMIT: ${{ github.sha }}", collect)
        self.assertIn("mkdir -p dist", collect)
        self.assertIn("collect-qcow2-${{ matrix.variant }}", collect)
        self.assertIn('echo "disk=${disk}" >> "$GITHUB_OUTPUT"', collect)
        self.assertEqual(
            collect.count(
                "make --no-print-directory --silent -C os-image/packer"
            ),
            1,
        )
        self.assertEqual(collect.count("ARCH=${{ matrix.arch }}"), 1)
        self.assertEqual(collect.count('QCOW2_DIST_DIR="$PWD/dist"'), 1)
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

    def test_every_name_keyed_on_arch_alone_also_takes_the_ubuntu_flavor(self):
        # Disks and S3 keys never collided (the filename carries the release),
        # but tags and artifact names did: a second flavor would have moved the
        # first's rather than landing beside it.
        for start, end in (
            (
                "- name: Build the FDE node image",
                "- name: Validate FDE TPM unlock (demo-fde.sh)",
            ),
            (
                "- name: Build the non-FDE node image",
                "- name: Boot-smoke the non-FDE qcow2",
            ),
            (
                "- name: Collect the finalized qcow2",
                "- name: Push the qcow2 to GHCR",
            ),
        ):
            self.assertIn(
                "UBUNTU_RELEASE=${{ matrix.ubuntu_release }}",
                self.step(start, end),
            )

        push = self.step(
            "- name: Push the qcow2 to GHCR",
            "- name: Show published manifest",
        )
        self.assertIn('tags="${tags:+${tags},}${alias}${SUFFIX}-${ARCH}"', push)

        release_assets = self.step(
            "- name: Prepare release assets",
            "- name: Upload release assets",
        )
        self.assertIn(
            "BUILD: ${{ matrix.arch }}${{ matrix.tag_suffix }}-${{ matrix.variant }}",
            release_assets,
        )
        self.assertIn("assets/checksums-${BUILD}.txt", release_assets)
        # Several builds share an arch, so the two files the release job merges
        # once are pinned to the default flavor's non-FDE build, not to amd64
        # alone.
        self.assertIn('if [[ "${BUILD}" == "amd64-non-fde" ]]; then', release_assets)

        # upload-artifact rejects two jobs in one run sharing a name.
        for artifact in (
            "install-console-logs-${{ matrix.arch }}${{ matrix.tag_suffix }}-${{ matrix.variant }}",
            "release-assets-${{ matrix.arch }}${{ matrix.tag_suffix }}-${{ matrix.variant }}",
        ):
            self.assertIn(artifact, self.workflow)

    def test_default_ubuntu_flavor_is_unsuffixed_and_matches_the_makefile(self):
        # The unsuffixed leg carries the shared release assets, so it has to be
        # the release the Makefile builds by default.
        makefile = (
            Path(__file__).resolve().parents[1] / "Makefile"
        ).read_text(encoding="utf-8")
        defaults = [
            line.split("?=", 1)[1].strip()
            for line in makefile.splitlines()
            if line.startswith("UBUNTU_RELEASE ?=")
        ]
        self.assertEqual(len(defaults), 1, defaults)
        self.assertIn(
            f'{{ubuntu_release:"{defaults[0]}",tag_suffix:""}}',
            self.workflow,
        )

    def test_publication_and_release_payloads_remain_qcow2_only(self):
        publish = self.step(
            "- name: Push the qcow2 to GHCR",
            "- name: Show published manifest",
        )
        release_assets = self.step(
            "- name: Prepare release assets",
            "- name: Upload release assets",
        )
        self.assertIn('"${DISK}:application/vnd.qemu.qcow2"', publish)
        self.assertIn('sha256sum "${DISK}"', release_assets)

    def test_cache_writer_covers_every_ubuntu_flavor_the_bake_reads(self):
        # The bake reads what os-image-cache.yml writes. A flavor added to one
        # and not the other is a permanent miss, which nothing reports.
        cache = CACHE_WORKFLOW.read_text(encoding="utf-8")
        baked = re.findall(r'ubuntu_release:"([^"]+)"', self.workflow)
        cached = re.search(r"ubuntu_release: \[([^\]]+)\]", cache)
        self.assertTrue(baked)
        self.assertIsNotNone(cached)
        self.assertEqual(
            sorted(baked), sorted(r.strip() for r in cached.group(1).split(","))
        )
        for line in (
            "BUILD_CACHE_REGION: us-east-1",
            "BUILD_CACHE_ISO_PREFIX: nke/os-image/iso",
        ):
            self.assertIn(line, self.workflow)
            self.assertIn(line, cache)


if __name__ == "__main__":
    unittest.main()
