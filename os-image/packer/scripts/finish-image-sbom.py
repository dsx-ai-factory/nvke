#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Reconcile a node image's CycloneDX SBOM with the dpkg database it came from.

Adds each package's dpkg status, drops packages dpkg does not count as
installed, and fails if the SBOM and dpkg disagree. See docs/artifact-publication.md.
"""

import argparse
import json
import os
from pathlib import Path
import sys
from urllib.parse import parse_qs, urlsplit


STATUS_PROPERTY = "nvke:dpkg:status"
COVERAGE_PROPERTY = "nvke:license-coverage"
UNKNOWN_LICENSE = "NOASSERTION"
# dpkg states the SBOM excludes: dpkg does not count these as installed.
ABSENT_STATES = {"not-installed", "config-files"}


def parse_status(text):
    """Return one dict per dpkg status stanza, keyed by lower-cased field name."""
    entries = []
    for number, stanza in enumerate(text.split("\n\n"), start=1):
        fields = {}
        last = None
        for line in stanza.splitlines():
            if not line.strip():
                continue
            if line[0] in " \t":
                if last is None:
                    raise ValueError(f"stanza {number}: continuation line before any field")
                continue
            name, sep, value = line.partition(":")
            if not sep:
                raise ValueError(f"stanza {number}: malformed line {line!r}")
            last = name.strip().lower()
            fields[last] = value.strip()
        if not fields:
            continue
        for required in ("package", "status"):
            if required not in fields:
                raise ValueError(f"stanza {number}: no {required.capitalize()} field")
        status = fields["status"].split()
        if len(status) != 3:
            raise ValueError(f"stanza {number} ({fields['package']}): Status is not 'want flag state'")
        fields["state"] = status[2]
        entries.append(fields)
    return entries


def deb_identity(component):
    """Return (name, arch) for a deb component, or None for anything else."""
    purl = component.get("purl", "")
    if not purl.startswith("pkg:deb/"):
        return None
    arch = parse_qs(urlsplit(purl).query).get("arch", [""])[0]
    return component["name"], arch


def finish(sbom, entries):
    """Rewrite sbom in place from entries; return (covered, total, problems)."""
    by_identity = {(entry["package"], entry.get("architecture", "")): entry for entry in entries}
    kept, removed, problems = [], set(), []
    covered = 0
    for component in sbom.get("components", []):
        identity = deb_identity(component)
        if identity is None:
            kept.append(component)
            continue
        entry = by_identity.get(identity)
        if entry is None:
            problems.append(f"{identity[0]}:{identity[1]} is in the SBOM but not in the dpkg database")
            kept.append(component)
            continue
        if entry["state"] in ABSENT_STATES:
            removed.add(component.get("bom-ref"))
            continue
        component.setdefault("properties", []).append({"name": STATUS_PROPERTY, "value": entry["status"]})
        if component.get("licenses"):
            covered += 1
        else:
            component["licenses"] = [{"license": {"name": UNKNOWN_LICENSE}}]
        kept.append(component)
    sbom["components"] = kept

    if "dependencies" in sbom:
        dependencies = []
        for dependency in sbom["dependencies"]:
            if dependency.get("ref") in removed:
                continue
            if "dependsOn" in dependency:
                dependency["dependsOn"] = [ref for ref in dependency["dependsOn"] if ref not in removed]
            dependencies.append(dependency)
        sbom["dependencies"] = dependencies

    listed = {}
    for component in kept:
        identity = deb_identity(component)
        if identity is not None:
            listed[identity] = component.get("version", "")
    for identity, entry in sorted(by_identity.items()):
        if entry["state"] in ABSENT_STATES:
            continue
        name = f"{identity[0]}:{identity[1]}"
        if identity not in listed:
            problems.append(f"{name} ({entry['status']}) is in the dpkg database but not in the SBOM")
        elif listed[identity] != entry.get("version", ""):
            problems.append(f"{name} is {entry.get('version', '')} in the dpkg database but {listed[identity]} in the SBOM")

    total = len(listed)
    metadata = sbom.setdefault("metadata", {})
    metadata.setdefault("properties", []).append({"name": COVERAGE_PROPERTY, "value": f"{covered}/{total}"})
    return covered, total, problems


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--status", required=True, type=Path, help="the image's /var/lib/dpkg/status")
    parser.add_argument("--sbom", required=True, type=Path, help="CycloneDX JSON from syft, rewritten in place")
    args = parser.parse_args(argv)

    try:
        entries = parse_status(args.status.read_text(encoding="utf-8"))
    except (OSError, ValueError) as error:
        print(f"::error::reading {args.status}: {error}", file=sys.stderr)
        return 1
    sbom = json.loads(args.sbom.read_text(encoding="utf-8"))
    covered, total, problems = finish(sbom, entries)
    if problems:
        for problem in problems:
            print(f"::error::{problem}", file=sys.stderr)
        return 1
    args.sbom.write_text(json.dumps(sbom, indent=2) + "\n", encoding="utf-8")

    share = f"{100 * covered / total:.1f}%" if total else "n/a"
    line = f"License coverage for {args.sbom.name}: {covered} of {total} packages ({share}); the rest are {UNKNOWN_LICENSE}"
    print(line)
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as handle:
            handle.write(f"{line}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
