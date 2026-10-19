# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise the NVKR export converter against the fixtures under hack/testdata."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("nvkr-run-to-stack-artifacts.py")
FIXTURES = Path(__file__).with_name("testdata") / "nvkr-release"
ZEROS = "sha256:" + "0" * 64


def run(*args, cwd=None):
    return subprocess.run([sys.executable, "-B", str(SCRIPT), *args], capture_output=True, text=True, check=False, cwd=cwd)


def layout(root, name, digest):
    path = root / name
    path.mkdir(parents=True, exist_ok=True)
    (path / "oci-layout").write_text('{"imageLayoutVersion":"1.0.0"}\n', encoding="utf-8")
    (path / "index.json").write_text(json.dumps({"schemaVersion": 2, "manifests": [{"digest": digest, "size": 2}]}), encoding="utf-8")
    return path


class ConverterTest(unittest.TestCase):
    def test_valid_export_converts(self):
        result = run(str(FIXTURES / "valid.json"))
        self.assertEqual(result.returncode, 0, result.stderr)
        doc = json.loads(result.stdout)
        self.assertEqual(doc["schemaVersion"], "nvke.nvidia.com/stack-artifacts/v1")
        self.assertEqual(doc["source"], {"reference": "ghcr.io/example/recipes/releases@" + ZEROS, "declaredQualification": "pipeline-attested"})
        self.assertEqual(len(doc["artifacts"]), 15)
        cwe = next(a for a in doc["artifacts"] if a["name"] == "cwe")
        self.assertEqual(set(cwe), {"name", "kind", "digest", "reference", "imageIndex"}, "only the fields the reader knows are kept")
        self.assertEqual(set(cwe["imageIndex"]), {"requiredPlatforms", "children"})
        for child in cwe["imageIndex"]["children"]:
            self.assertEqual(set(child), {"platform", "manifestDigest"}, "an export's child fields the reader does not know are dropped")
        self.assertNotIn("buildDigest", cwe)
        chart = next(a for a in doc["artifacts"] if a["name"] == "controller-chart")
        self.assertEqual(chart["chartVersion"], "0.1.1")
        self.assertIn("nothing here verifies it; run: nvkr release verify --reference ghcr.io/example/recipes/releases@", result.stderr)
        self.assertNotIn("layout", json.dumps(doc), "no run dir, no layouts")

    def test_output_shapes_are_refused(self):
        base = json.loads((FIXTURES / "valid.json").read_text())
        cases = {
            "reference without a digest": (lambda d: (d["outputs"][0].pop("digest", None), d["outputs"][0].__setitem__("reference", "ghcr.io/example/cwe:v1")), "carries no sha256 digest"),
            "empty image index children": (lambda d: d["outputs"][0]["imageIndex"].__setitem__("children", []), "non-empty children list"),
            "required platforms not a list": (lambda d: d["outputs"][0]["imageIndex"].__setitem__("requiredPlatforms", "linux/amd64"), "requiredPlatforms must be a list"),
        }
        for name, (mutate, want) in cases.items():
            with self.subTest(case=name):
                doc = json.loads(json.dumps(base))
                mutate(doc)
                with tempfile.TemporaryDirectory() as tmp:
                    path = os.path.join(tmp, "export.json")
                    with open(path, "w") as handle:
                        json.dump(doc, handle)
                    result = run(path)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn(want, result.stderr)

    def test_refusals_name_the_problem(self):
        cases = {
            "filtered.json": 'the description is filtered to \'cwe\'; export the whole release with nvkr release export',
            "release-v4.json": "schemaVersion is 'nvkr.dev/release/v4', expected nvkr.dev/release-description/v1; this converter reads the file",
            "verification-result.json": "schemaVersion is 'nvkr.dev/release-verification-result/v1'",
            "bad-schema.json": "schemaVersion is 'nvkr.dev/release/v2'",
            "not-json.json": "not a JSON document",
            "no-artifacts.json": "outputs is empty",
        }
        for file, want in cases.items():
            with self.subTest(file=file):
                result = run(str(FIXTURES / file))
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn(want, result.stderr)
                self.assertEqual(result.stdout, "", "nothing on stdout when refused")

    def test_total_outputs_and_root_digest(self):
        with tempfile.TemporaryDirectory() as directory:
            doc = json.loads((FIXTURES / "valid.json").read_text(encoding="utf-8"))
            doc["totalOutputs"] = 2
            path = Path(directory) / "r.json"
            path.write_text(json.dumps(doc), encoding="utf-8")
            result = run(str(path))
            self.assertIn("totalOutputs is 2 but 15 outputs are listed", result.stderr)
            doc["totalOutputs"] = 15
            doc["rootDigest"] = "sha256:" + "b" * 64
            path.write_text(json.dumps(doc), encoding="utf-8")
            self.assertIn("does not end in rootDigest", run(str(path)).stderr)
            del doc["reference"]
            path.write_text(json.dumps(doc), encoding="utf-8")
            self.assertIn("reference is empty", run(str(path)).stderr)
            self.assertIn("No such file", run(str(Path(directory) / "absent.json")).stderr)

    def test_run_dir_layouts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cwe = "sha256:" + "a" * 64
            profiles = "sha256:" + "c" * 64
            layout(root / "run" / "packages", "cwe", cwe)
            layout(root / "run", "controller-profiles", profiles)
            result = run("--run-dir", str(root / "run"), str(FIXTURES / "valid.json"))
            self.assertEqual(result.returncode, 0, result.stderr)
            doc = json.loads(result.stdout)
            by_name = {a["name"]: a for a in doc["artifacts"]}
            self.assertEqual(by_name["cwe"]["layout"], str(root / "run" / "packages" / "cwe"), "stdout carries absolute layouts")
            self.assertEqual(by_name["controller-profiles"]["layout"], str(root / "run" / "controller-profiles"))
            self.assertNotIn("layout", by_name["nktp"], "an artifact without a layout keeps its reference only")
            # Two layouts for one digest is refused.
            layout(root / "run" / "packages", "cwe-copy", cwe)
            result = run("--run-dir", str(root / "run"), str(FIXTURES / "valid.json"))
            self.assertEqual(result.returncode, 1)
            self.assertIn("artifact cwe: several layouts under", result.stderr)
            # A missing run dir is refused.
            result = run("--run-dir", str(root / "absent"), str(FIXTURES / "valid.json"))
            self.assertIn("is not a directory", result.stderr)

    def test_out_writes_relative_layouts_atomically(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            layout(root / "run" / "packages", "cwe", "sha256:" + "a" * 64)
            out = root / "site" / "stack-artifacts.json"
            out.parent.mkdir()
            result = run("--run-dir", str(root / "run"), "--out", str(out), str(FIXTURES / "valid.json"))
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, "", "--out writes nothing to stdout")
            doc = json.loads(out.read_text(encoding="utf-8"))
            cwe = next(a for a in doc["artifacts"] if a["name"] == "cwe")
            self.assertEqual(cwe["layout"], os.path.join("..", "run", "packages", "cwe"), "layouts are relative to the file")
            self.assertEqual([p.name for p in out.parent.iterdir()], ["stack-artifacts.json"], "no temp file is left")
            # A refused conversion leaves an existing --out as it was.
            before = out.read_text(encoding="utf-8")
            result = run("--out", str(out), str(FIXTURES / "filtered.json"))
            self.assertEqual(result.returncode, 1)
            self.assertEqual(out.read_text(encoding="utf-8"), before)
            self.assertEqual([p.name for p in out.parent.iterdir()], ["stack-artifacts.json"])

    def test_name_map(self):
        result = run("--name-map", "cwe=controller-image", "--name-map", "nktp=nktp-chart", str(FIXTURES / "valid.json"))
        self.assertEqual(result.returncode, 0, result.stderr)
        names = {a["name"] for a in json.loads(result.stdout)["artifacts"]}
        self.assertIn("controller-image", names)
        self.assertIn("nktp-chart", names)
        self.assertNotIn("cwe", names)
        result = run("--name-map", "absent=x", str(FIXTURES / "valid.json"))
        self.assertEqual(result.returncode, 1)
        self.assertIn("the export has no output named absent", result.stderr)
        result = run("--name-map", "cwe=nktp", str(FIXTURES / "valid.json"))
        self.assertEqual(result.returncode, 1)
        self.assertIn("duplicate artifact name after renaming: nktp", result.stderr)
        result = run("--name-map", "cwe", str(FIXTURES / "valid.json"))
        self.assertIn("is not OLD=NEW", result.stderr)

    def test_digest_from_reference(self):
        with tempfile.TemporaryDirectory() as directory:
            doc = json.loads((FIXTURES / "valid.json").read_text(encoding="utf-8"))
            del doc["outputs"][0]["digest"]
            path = Path(directory) / "r.json"
            path.write_text(json.dumps(doc), encoding="utf-8")
            result = run(str(path))
            self.assertEqual(result.returncode, 0, result.stderr)
            cwe = json.loads(result.stdout)["artifacts"][0]
            self.assertEqual(cwe["digest"], "sha256:" + "a" * 64)
            # Neither a digest nor a reference is refused.
            del doc["outputs"][0]["reference"]
            path.write_text(json.dumps(doc), encoding="utf-8")
            result = run(str(path))
            self.assertEqual(result.returncode, 1)
            self.assertIn("output cwe: a digest or a reference carrying one is required", result.stderr)


if __name__ == "__main__":
    unittest.main()
