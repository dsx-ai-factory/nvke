# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Reconciling a syft CycloneDX SBOM with the dpkg database it was read from."""

import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


SCRIPTS = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("finish_image_sbom", SCRIPTS / "finish-image-sbom.py")
FINISH = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FINISH)


def stanza(name, status, version="1.0", arch="amd64"):
    lines = [f"Package: {name}", f"Status: {status}", f"Architecture: {arch}"]
    if version is not None:
        lines.append(f"Version: {version}")
    lines += ["Description: test package", " with a continuation line"]
    return "\n".join(lines)


def component(name, version="1.0", arch="amd64", licenses=True):
    item = {
        "bom-ref": f"ref-{name}-{arch}",
        "type": "library",
        "name": name,
        "version": version,
        "purl": f"pkg:deb/ubuntu/{name}@{version}?arch={arch}&distro=ubuntu-24.04",
        "properties": [{"name": "syft:package:type", "value": "deb"}],
    }
    if licenses:
        item["licenses"] = [{"license": {"id": "MIT"}}]
    return item


def status_of(item):
    return [p["value"] for p in item.get("properties", []) if p["name"] == FINISH.STATUS_PROPERTY]


class FinishImageSBOMTest(unittest.TestCase):
    def finish(self, stanzas, components, dependencies=None):
        sbom = {"bomFormat": "CycloneDX", "specVersion": "1.6", "components": components}
        if dependencies is not None:
            sbom["dependencies"] = dependencies
        covered, total, problems = FINISH.finish(sbom, FINISH.parse_status("\n\n".join(stanzas) + "\n"))
        return sbom, covered, total, problems

    def test_records_the_dpkg_state_of_every_package_that_is_on_the_image(self):
        cases = {
            "installed": "install ok installed",
            "held": "hold ok installed",
            "unpacked": "install ok unpacked",
            "half-configured": "install ok half-configured",
            "half-installed": "install reinstreq half-installed",
            "triggers-pending": "install ok triggers-pending",
        }
        for name, status in cases.items():
            with self.subTest(name=name):
                sbom, _, total, problems = self.finish([stanza(name, status)], [component(name)])
                self.assertEqual(problems, [])
                self.assertEqual(total, 1)
                self.assertEqual(status_of(sbom["components"][0]), [status])

    def test_drops_packages_dpkg_does_not_count_as_installed(self):
        stanzas = [
            stanza("kept", "install ok installed"),
            stanza("removed", "deinstall ok config-files"),
            stanza("never", "purge ok not-installed", version=None),
        ]
        components = [component("kept"), component("removed"), component("never", version="UNKNOWN")]
        dependencies = [
            {"ref": "ref-kept-amd64", "dependsOn": ["ref-removed-amd64", "ref-never-amd64"]},
            {"ref": "ref-removed-amd64", "dependsOn": []},
        ]
        sbom, _, total, problems = self.finish(stanzas, components, dependencies)
        self.assertEqual(problems, [])
        self.assertEqual(total, 1)
        self.assertEqual([c["name"] for c in sbom["components"]], ["kept"])
        self.assertEqual(sbom["dependencies"], [{"ref": "ref-kept-amd64", "dependsOn": []}])

    def test_marks_a_missing_license_and_reports_coverage(self):
        stanzas = [stanza("licensed", "install ok installed"), stanza("bare", "install ok installed")]
        sbom, covered, total, problems = self.finish(stanzas, [component("licensed"), component("bare", licenses=False)])
        self.assertEqual(problems, [])
        self.assertEqual((covered, total), (1, 2))
        bare = next(c for c in sbom["components"] if c["name"] == "bare")
        self.assertEqual(bare["licenses"], [{"license": {"name": "NOASSERTION"}}])
        self.assertIn({"name": FINISH.COVERAGE_PROPERTY, "value": "1/2"}, sbom["metadata"]["properties"])

    def test_matches_packages_by_architecture(self):
        stanzas = [stanza("libc6", "install ok installed"), stanza("libc6", "install ok installed", arch="i386")]
        components = [component("libc6"), component("libc6", arch="i386")]
        _, _, total, problems = self.finish(stanzas, components)
        self.assertEqual(problems, [])
        self.assertEqual(total, 2)

    def test_reports_every_disagreement_with_the_dpkg_database(self):
        stanzas = [
            stanza("missing", "hold ok installed"),
            stanza("drifted", "install ok installed", version="2.0"),
            stanza("other-arch", "install ok installed", arch="arm64"),
        ]
        components = [component("drifted", version="1.9"), component("extra"), component("other-arch")]
        _, _, _, problems = self.finish(stanzas, components)
        self.assertCountEqual(problems, [
            "extra:amd64 is in the SBOM but not in the dpkg database",
            "other-arch:amd64 is in the SBOM but not in the dpkg database",
            "missing:amd64 (hold ok installed) is in the dpkg database but not in the SBOM",
            "other-arch:arm64 (install ok installed) is in the dpkg database but not in the SBOM",
            "drifted:amd64 is 2.0 in the dpkg database but 1.9 in the SBOM",
        ])

    def test_reports_a_package_listed_twice(self):
        stanzas = [stanza("bash", "install ok installed")]
        _, covered, total, problems = self.finish(stanzas, [component("bash"), component("bash", version="0.9")])
        self.assertEqual(problems, ["bash:amd64 appears more than once in the SBOM"])
        self.assertEqual((covered, total), (1, 1))

    def test_leaves_components_that_are_not_debs_alone(self):
        os_component = {"bom-ref": "os", "type": "operating-system", "name": "ubuntu", "version": "24.04"}
        sbom, _, total, problems = self.finish([stanza("bash", "install ok installed")], [os_component, component("bash")])
        self.assertEqual(problems, [])
        self.assertEqual(total, 1)
        self.assertEqual(sbom["components"][0], {"bom-ref": "os", "type": "operating-system", "name": "ubuntu", "version": "24.04"})

    def test_rejects_a_malformed_status_database(self):
        cases = {
            "no status field": "Package: bash\nVersion: 1.0\n",
            "no package field": "Status: install ok installed\n",
            "short status": "Package: bash\nStatus: install installed\n",
            "line without a colon": "Package: bash\nStatus: install ok installed\nnonsense\n",
            "leading continuation": " orphan\nPackage: bash\nStatus: install ok installed\n",
        }
        for name, text in cases.items():
            with self.subTest(name=name), self.assertRaises(ValueError):
                FINISH.parse_status(text)

    def test_main_rewrites_the_sbom_only_when_it_agrees(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            status = root / "status"
            status.write_text(stanza("bash", "hold ok installed") + "\n", encoding="utf-8")
            sbom = root / "image.cdx.json"
            summary = root / "summary.md"

            sbom.write_text(json.dumps({"components": [component("bash", licenses=False)]}), encoding="utf-8")
            with patch.dict("os.environ", {"GITHUB_STEP_SUMMARY": str(summary)}), patch("sys.stdout", io.StringIO()):
                self.assertEqual(FINISH.main(["--status", str(status), "--sbom", str(sbom)]), 0)
            written = json.loads(sbom.read_text(encoding="utf-8"))
            self.assertEqual(status_of(written["components"][0]), ["hold ok installed"])
            self.assertIn("0 of 1 packages", summary.read_text(encoding="utf-8"))

            original = json.dumps({"components": [component("zsh")]})
            sbom.write_text(original, encoding="utf-8")
            with patch("sys.stderr", io.StringIO()) as stderr:
                self.assertEqual(FINISH.main(["--status", str(status), "--sbom", str(sbom)]), 1)
            self.assertIn("bash:amd64 (hold ok installed) is in the dpkg database but not in the SBOM", stderr.getvalue())
            self.assertEqual(sbom.read_text(encoding="utf-8"), original)


if __name__ == "__main__":
    unittest.main()
