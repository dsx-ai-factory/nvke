#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Convert an NVKR release export into a stack artifacts file.

`nvkr release export --reference <release>` writes a nvkr.dev/release-description/v1
document: the outputs of one published release, each by reference and digest,
with the release's own identity around them. The NVKE install reads
nvke.nvidia.com/stack-artifacts/v1 instead: the artifacts alone, with an optional
source block and an optional OCI layout per artifact. This script maps one onto
the other and nothing more. It does not verify the release: run `nvkr release
verify` on the same reference first.

    hack/nvkr-run-to-stack-artifacts.py [--run-dir DIR] [--out FILE] [--name-map OLD=NEW ...] RELEASE_JSON

Every output becomes an artifact with its name, kind, digest, reference, os,
arch, chartVersion and imageIndex; the release reference and the declared
qualification become the source block; the rest of the export is dropped. With
--run-dir, the directory of a completed NVKR Run (or any directory of OCI
layouts: DIR, DIR/*, DIR/packages/*) is searched for a layout whose index lists
each artifact's digest, and the artifact gets it as its layout; two layouts
for one digest is an error, none means the artifact is pulled from its
reference. --name-map renames an output whose name differs from the one the
deploy path binds. --out writes the file atomically with layouts relative to
it; without --out the file goes to stdout with absolute layouts. A filtered
export, as `nvkr release browse --output` writes, is refused.

Standard library only, so it runs on a stock python3 and can be handed to
NVKR as it is. Exit status 1 with one line per problem on stderr.
"""

import argparse
import json
import os
import re
import sys
import tempfile

SCHEMA_IN = "nvkr.dev/release-description/v1"
SCHEMA_OUT = "nvke.nvidia.com/stack-artifacts/v1"
KEPT = ("name", "kind", "digest", "reference", "os", "arch", "chartVersion", "imageIndex")
DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")


class Problem(Exception):
    """A reason the export cannot be converted, worded for the operator."""


def load(path):
    try:
        with open(path, encoding="utf-8") as handle:
            data = handle.read()
    except OSError as err:
        raise Problem(f"{path}: {err.strerror}") from err
    if not data.strip():
        raise Problem(f"{path}: the file is empty")
    try:
        doc = json.loads(data)
    except json.JSONDecodeError as err:
        raise Problem(f"{path}: not a JSON document: {err}") from err
    if not isinstance(doc, dict):
        raise Problem(f"{path}: the file does not hold a JSON object")
    return doc


def validate(doc):
    schema = doc.get("schemaVersion")
    if schema != SCHEMA_IN:
        raise Problem(
            f"schemaVersion is {schema!r}, expected {SCHEMA_IN}; this converter reads the file "
            "`nvkr release export --reference <release>` writes, not the output of `nvkr release verify`"
        )
    reference = doc.get("reference") or ""
    root = doc.get("rootDigest") or ""
    if not reference:
        raise Problem("reference is empty")
    if not DIGEST.match(root):
        raise Problem(f"rootDigest is not sha256:<64 hex>: {root!r}")
    if not reference.endswith("@" + root):
        raise Problem(f"reference {reference} does not end in rootDigest {root}")
    selection = (doc.get("inspectionDetails") or {}).get("outputSelection") or ""
    selected = doc.get("selectedOutput") or ""
    if (selection and selection != "all") or selected:
        raise Problem(f"the description is filtered to {selected or selection!r}; export the whole release with nvkr release export")
    outputs = doc.get("outputs")
    if not isinstance(outputs, list):
        raise Problem("outputs is not a list")
    total = doc.get("totalOutputs") or 0
    if total and total != len(outputs):
        raise Problem(f"totalOutputs is {total} but {len(outputs)} outputs are listed; export the whole release")
    if not outputs:
        raise Problem("outputs is empty")
    for i, out in enumerate(outputs):
        if not isinstance(out, dict) or not out.get("name") or not out.get("kind"):
            raise Problem(f"outputs[{i}]: name and kind are required")
        if not out.get("digest") and not out.get("reference"):
            raise Problem(f"output {out['name']}: a digest or a reference carrying one is required")
        if not out.get("digest") and not DIGEST.match(str(out["reference"]).rpartition("@")[2]):
            raise Problem(f"output {out['name']}: reference {out['reference']} carries no sha256 digest")
    return reference, doc.get("declaredQualification") or ""


def rename(outputs, name_map):
    mapping = {}
    for entry in name_map:
        old, sep, new = entry.partition("=")
        if not sep or not old or not new:
            raise Problem(f"--name-map {entry!r} is not OLD=NEW")
        mapping[old] = new
    names = {out["name"] for out in outputs}
    for old in mapping:
        if old not in names:
            raise Problem(f"--name-map {old}={mapping[old]}: the export has no output named {old}")
    for out in outputs:
        out["name"] = mapping.get(out["name"], out["name"])
    seen = set()
    for out in outputs:
        if out["name"] in seen:
            raise Problem(f"duplicate artifact name after renaming: {out['name']}")
        seen.add(out["name"])


def layouts_under(run_dir):
    """Every candidate layout directory: run_dir, its children, packages/'s children."""
    if not os.path.isdir(run_dir):
        raise Problem(f"--run-dir {run_dir} is not a directory")
    candidates = [run_dir]
    for parent in (run_dir, os.path.join(run_dir, "packages")):
        if os.path.isdir(parent):
            for entry in sorted(os.listdir(parent)):
                path = os.path.join(parent, entry)
                if os.path.isdir(path):
                    candidates.append(path)
    found = {}
    for candidate in candidates:
        index = os.path.join(candidate, "index.json")
        if not os.path.isfile(index):
            continue
        try:
            with open(index, encoding="utf-8") as handle:
                manifests = json.load(handle).get("manifests", [])
        except (OSError, json.JSONDecodeError, AttributeError) as err:
            raise Problem(f"{index}: {err}") from err
        for manifest in manifests:
            digest = manifest.get("digest") if isinstance(manifest, dict) else None
            if digest:
                found.setdefault(digest, []).append(candidate)
    return found


def project_image_index(index, name):
    """Keep of an export's image index what the reader knows: the required
    platforms and each child's platform and manifest digest. NVKR writes more
    per child (size, build digest, component, build subject), which the
    reader refuses as unknown fields."""
    if not isinstance(index, dict) or not isinstance(index.get("children"), list) or not index["children"]:
        raise Problem(f"output {name}: imageIndex must hold a non-empty children list")
    if "requiredPlatforms" in index and not isinstance(index["requiredPlatforms"], list):
        raise Problem(f"output {name}: imageIndex.requiredPlatforms must be a list")
    children = []
    for child in index["children"]:
        if not isinstance(child, dict) or not child.get("platform") or not child.get("manifestDigest"):
            raise Problem(f"output {name}: every imageIndex child needs platform and manifestDigest")
        children.append({"platform": child["platform"], "manifestDigest": child["manifestDigest"]})
    out = {"children": children}
    if index.get("requiredPlatforms"):
        out["requiredPlatforms"] = index["requiredPlatforms"]
    return out


def convert(doc, run_dir=None, name_map=(), out_path=None):
    reference, qualification = validate(doc)
    outputs = [{k: out[k] for k in KEPT if k in out and out[k] not in (None, "", [], {})} for out in doc["outputs"]]
    for out in outputs:
        if "digest" not in out:
            out["digest"] = out["reference"].rpartition("@")[2]
        if "imageIndex" in out:
            out["imageIndex"] = project_image_index(out["imageIndex"], out["name"])
    rename(outputs, name_map)
    if run_dir is not None:
        found = layouts_under(run_dir)
        base = os.path.dirname(os.path.abspath(out_path)) if out_path else None
        for out in outputs:
            dirs = found.get(out["digest"], [])
            if len(dirs) > 1:
                raise Problem(f"artifact {out['name']}: several layouts under {run_dir} list digest {out['digest']}: {', '.join(dirs)}")
            if dirs:
                layout = os.path.abspath(dirs[0])
                out["layout"] = os.path.relpath(layout, base) if base else layout
    result = {"schemaVersion": SCHEMA_OUT}
    source = {"reference": reference}
    if qualification:
        source["declaredQualification"] = qualification
    result["source"] = source
    result["artifacts"] = outputs
    return result


def write(result, out_path):
    """Write the file through a temp file in its directory and a rename."""
    directory = os.path.dirname(os.path.abspath(out_path)) or "."
    fd, tmp = tempfile.mkstemp(prefix=".stack-artifacts.", suffix=".tmp", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(result, handle, indent=1)
            handle.write("\n")
        os.replace(tmp, out_path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def main(argv=None):
    parser = argparse.ArgumentParser(description="Convert an NVKR release export into a stack artifacts file.")
    parser.add_argument("release", metavar="RELEASE_JSON", help="the nvkr.dev/release-description/v1 file nvkr release export writes")
    parser.add_argument("--run-dir", metavar="DIR", help="a completed NVKR Run, or a directory of OCI layouts, to take layouts from")
    parser.add_argument("--out", metavar="FILE", help="write the file here, atomically, with layouts relative to it; default: stdout, absolute layouts")
    parser.add_argument("--name-map", metavar="OLD=NEW", action="append", default=[], help="rename an output; repeatable")
    args = parser.parse_args(argv)
    try:
        doc = load(args.release)
        result = convert(doc, run_dir=args.run_dir, name_map=args.name_map, out_path=args.out)
    except Problem as err:
        print(f"nvkr-run-to-stack-artifacts: {err}", file=sys.stderr)
        return 1
    source = result["source"]
    declared = source.get("declaredQualification", "no qualification")
    print(
        f"nvkr-run-to-stack-artifacts: source {source['reference']} (declared {declared}); "
        f"nothing here verifies it; run: nvkr release verify --reference {source['reference']}",
        file=sys.stderr,
    )
    if args.out:
        try:
            write(result, args.out)
        except OSError as err:
            print(f"nvkr-run-to-stack-artifacts: {args.out}: {err.strerror}", file=sys.stderr)
            return 1
    else:
        json.dump(result, sys.stdout, indent=1)
        sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
