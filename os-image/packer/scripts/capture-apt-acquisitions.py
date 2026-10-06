#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Export captured APT archives and repository metadata, not transport attestations.

The existing python3-apt guest package owns repository metadata parsing. dpkg-deb
owns Debian archive metadata. No inferred URL or installed-package inventory is
accepted as evidence of acquired bytes. The output is a descriptive capture;
its consumer must hash every member and authenticate its Run/source association.
"""

import argparse
import hashlib
import io
from itertools import islice
import json
import os
from pathlib import Path
import stat
import subprocess
import tarfile
import tempfile
from urllib.parse import urlsplit


SCHEMA = "nkd.dev/package-acquisitions/v1"
MAX_PACKAGES = 16384
MAX_PACKAGE_BYTES = 1 << 30
MAX_TOTAL_BYTES = 7 << 30


def sha256_stream(source):
    """Hash bounded read chunks using hashlib APIs available on Python 3.10."""
    digest = hashlib.sha256()
    for chunk in iter(lambda: source.read(1 << 20), b""):
        digest.update(chunk)
    return digest.hexdigest()


def package_metadata(path):
    """Read Debian metadata with the owning dpkg tool, not a local parser."""
    # dpkg validates Debian package syntax. This is fixed-output transport,
    # not a second implementation of the Debian control-file grammar.
    try:
        output = subprocess.run(
            ["dpkg-deb", "--show", "--showformat=${Package}\t${Version}\t${Architecture}\n", str(path)],
            check=True, text=True, capture_output=True, timeout=30,
        ).stdout
    except subprocess.CalledProcessError as error:
        raise ValueError(f"dpkg-deb could not read captured package: {error.stderr.strip()}") from error
    fields = output.rstrip("\n").split("\t")
    if len(fields) != 3 or any(not field or any(character.isspace() for character in field) for field in fields):
        raise ValueError("dpkg-deb returned invalid package metadata")
    return fields


def repository_uris(cache, name, version, architecture, digest, size):
    """Select exact source-reported package locations without claiming origin."""
    key = name if architecture == "all" else f"{name}:{architecture}"
    subject = f"{name}={version}:{architecture}"
    if key not in cache:
        raise ValueError(f"captured package {subject} is absent from repository metadata")
    candidates = [item for item in cache[key].versions
                  if item.version == version and item.architecture == architecture
                  # Installed-only versions have no repository locations and
                  # python3-apt can raise SystemError when reading their hash.
                  and item.uris and item.sha256 == digest and item.size == size]
    locations = sorted({uri for item in candidates for uri in item.uris})
    if not locations:
        raise ValueError(f"captured package {subject} has no exact repository digest/size match")
    if len(locations) > 32:
        raise ValueError(f"captured package {subject} exceeds the 32 repository location limit")
    for uri in locations:
        # Never include a complete URI in errors: it may contain credentials.
        try:
            parsed = urlsplit(uri)
        except ValueError:
            # urllib's original error may include the malformed hostname.
            raise ValueError(f"captured package {subject} has unsafe repository metadata locations: malformed URI") from None
        reason = None
        if parsed.scheme not in ("http", "https"):
            reason = f"unsupported scheme {parsed.scheme!r}"
        elif not parsed.hostname:
            reason = "missing hostname"
        elif parsed.username is not None or parsed.password is not None:
            reason = "embedded credentials"
        elif parsed.query or parsed.fragment or "?" in uri:
            reason = "query or fragment"
        elif any(character.isspace() for character in uri):
            reason = "whitespace"
        if reason:
            raise ValueError(f"captured package {subject} has unsafe repository metadata locations: {reason}")
    return locations


def capture(root, output, architecture, cache):
    """Publish a bounded deterministic byte capture without replacing output."""
    if architecture not in ("amd64", "arm64"):
        raise ValueError("architecture must be amd64 or arm64")
    root = Path(root)
    output = Path(output)
    if root.is_symlink() or (root / "packages").is_symlink():
        raise ValueError("capture root must not be a symbolic link")
    for item in root.iterdir():
        if item.name != "packages":
            raise ValueError("capture root contains undeclared entries")
    files = sorted(islice((root / "packages").iterdir(), MAX_PACKAGES + 1))
    if not files or len(files) > MAX_PACKAGES:
        raise ValueError("capture requires a bounded nonempty package set")
    entries = []
    sources = {}
    total = 0
    for path in files:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        try:
            info = os.fstat(descriptor)
            if not stat.S_ISREG(info.st_mode) or not 0 < info.st_size <= MAX_PACKAGE_BYTES:
                raise ValueError("captured package is not a bounded regular file")
        except BaseException:
            os.close(descriptor)
            raise
        with os.fdopen(descriptor, "rb") as package:
            total += info.st_size
            if total > MAX_TOTAL_BYTES:
                raise ValueError("captured package closure exceeds its byte budget")
            digest = sha256_stream(package)
        if digest in sources:
            continue
        sources[digest] = path
        name, version, package_architecture = package_metadata(path)
        if package_architecture not in (architecture, "all"):
            raise ValueError(f"captured package {name} has the wrong architecture")
        entries.append({"name": name, "version": version, "architecture": package_architecture,
                        "digest": f"sha256:{digest}", "size": info.st_size,
                        "path": f"packages/{digest}.deb",
                        "repositoryUris": repository_uris(cache, name, version, package_architecture, digest, info.st_size)})
    entries.sort(key=lambda entry: entry["digest"])
    manifest = {"schemaVersion": SCHEMA, "architecture": architecture,
                "scope": "apt-pre-install-pkgs", "packages": entries}
    raw = json.dumps(manifest, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()
    if len(raw) > 8 << 20:
        raise ValueError("capture manifest exceeds its byte budget")
    # A failed capture cannot leave an accepted output. No-replace publication
    # preserves an existing caller output and the completed capture is synced.
    descriptor, temporary = tempfile.mkstemp(prefix=".apt-capture-", dir=output.parent)
    published = False
    try:
        with os.fdopen(descriptor, "wb") as destination:
            with tarfile.open(fileobj=destination, mode="w", format=tarfile.USTAR_FORMAT) as archive:
                info = tarfile.TarInfo("manifest.json")
                info.mode, info.size = 0o444, len(raw)
                archive.addfile(info, io.BytesIO(raw))
                for entry in entries:
                    package_descriptor = os.open(sources[entry["digest"].removeprefix("sha256:")], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                    try:
                        package_info = os.fstat(package_descriptor)
                        if not stat.S_ISREG(package_info.st_mode) or package_info.st_size != entry["size"]:
                            raise ValueError("captured package changed before export")
                    except BaseException:
                        os.close(package_descriptor)
                        raise
                    with os.fdopen(package_descriptor, "rb") as package:
                        info = tarfile.TarInfo(entry["path"])
                        info.mode, info.size = 0o444, entry["size"]
                        archive.addfile(info, package)
            destination.flush()
            os.fchmod(destination.fileno(), 0o644)
            os.fsync(destination.fileno())
        # Hash the bytes actually written, not a second read of the source:
        # a source could change during copying and revert before that read.
        with tarfile.open(temporary, mode="r:") as exported:
            for entry in entries:
                with exported.extractfile(entry["path"]) as package:
                    if f"sha256:{sha256_stream(package)}" != entry["digest"]:
                        raise ValueError("captured package changed during export")
        os.link(temporary, output)
        published = True
        directory = os.open(output.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    except BaseException:
        if published:
            output.unlink()
        raise
    finally:
        os.unlink(temporary)
    return manifest


def main():
    """Run the capture in the guest using its installed python3-apt package."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--architecture", choices=("amd64", "arm64"), required=True)
    arguments = parser.parse_args()
    # This dependency is already installed by NKE autoinstall. Import only in
    # the guest operation so contract tests can exercise error handling on macOS.
    import apt
    capture(arguments.root, arguments.output, arguments.architecture, apt.Cache())


if __name__ == "__main__":
    main()
