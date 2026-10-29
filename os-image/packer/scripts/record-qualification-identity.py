#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Opt-in build helper; run inside the final build guest, never on a test node.

The caller supplies a unique build ID and configured Ubuntu/architecture. Copy
the output manifest out before publishing the image. This script is not yet
connected to Packer or artifact publication.
"""

import argparse
import json
from pathlib import Path
import platform
import re


def record(build_id, ubuntu, arch, output, identity_file):
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._-]*", build_id):
        raise ValueError("build ID must be a nonempty identifier")
    if ubuntu not in ("24.04", "26.04") or arch not in ("amd64", "arm64"):
        raise ValueError("unsupported Ubuntu version or architecture")
    release = platform.freedesktop_os_release()
    actual_arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if release.get("ID") != "ubuntu" or release.get("VERSION_ID") != ubuntu or actual_arch != arch:
        raise ValueError("build guest does not match the configured Ubuntu version and architecture")
    # Run after the final kernel boot. Hardware qualification checks that the
    # same kernel boots on the physical node.
    manifest = {
        "schema_version": "nvke.os-image.identity/v1",
        "ubuntu": ubuntu,
        "arch": arch,
        "kernel_release": platform.release(),
        "build_id": build_id,
    }
    if output.resolve() == identity_file.resolve():
        raise ValueError("manifest and identity file must be different paths")
    if output.exists() or identity_file.exists():
        raise FileExistsError("build identity or output manifest already exists")
    if not output.parent.is_dir():
        raise ValueError("output manifest directory does not exist")
    identity_file.parent.mkdir(parents=True, exist_ok=True)
    # Do not silently re-stamp an existing image or replace build evidence.
    with identity_file.open("x", encoding="utf-8") as stream:
        stream.write(build_id + "\n")
    identity_file.chmod(0o644)
    with output.open("x", encoding="utf-8") as stream:
        stream.write(json.dumps(manifest, indent=2) + "\n")
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--build-id", required=True)
    parser.add_argument("--ubuntu", required=True)
    parser.add_argument("--arch", required=True)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--identity-file", type=Path, default=Path("/usr/lib/nvke/image-build-id"))
    args = parser.parse_args()
    record(args.build_id, args.ubuntu, args.arch, args.output, args.identity_file)


if __name__ == "__main__":
    main()
