#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Keep a node image on one Ubuntu snapshot (#2206).

The image takes its Ubuntu packages from snapshot.ubuntu.com at one fixed
snapshot ID, so two builds of the same commit install the same versions. This
script checks that from both sides:

  sources   in the guest: every apt source is that snapshot or the image's
            local NVKE repository, and nothing names the live archive.
  versions  in the guest, while the indexes are still present: every installed
            package's version is downloadable from the snapshot or the local
            repository. A version left over from the installer ISO that the
            snapshot does not carry fails here. Prints a digest of the package
            set, so two builds can be compared by one line of their logs.
  id        on the host: a candidate snapshot ID serves both Ubuntu series and
            both architectures, before it becomes the committed default.

Transitional: the release lock (#2204) and verify (#2205) replace it.
"""
import argparse
import hashlib
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Callable, Iterable, NamedTuple

SERIES = ("noble", "resolute")
SUITES = ("", "-updates", "-security")
ARCHITECTURES = ("amd64", "arm64")
SNAPSHOT_ID = re.compile(r"^[0-9]{8}T[0-9]{6}Z$")
DEFAULT_LOCAL_REPO = "/usr/lib/nvke/debs"


class Installed(NamedTuple):
    name: str
    arch: str
    version: str
    uris: tuple


def _canonical(uri: str) -> str:
    """Lower-case the scheme and host; `file:///x` and `file:/x` become one form."""
    parts = urllib.parse.urlsplit(uri.strip())
    scheme, host = parts.scheme.lower(), parts.netloc.lower()
    return f"{scheme}://{host}{parts.path}" if host else f"{scheme}:{parts.path}"


def normalize_uri(uri: str) -> str:
    """A repository base URI in canonical form, ending in exactly one slash."""
    return _canonical(uri).rstrip("/") + "/"


def _allowed(snapshot_uri: str, local_repo: str) -> tuple:
    return normalize_uri(snapshot_uri), normalize_uri("file:" + local_repo)


def bad_sources(uris: Iterable[str], snapshot_uri: str, local_repo: str) -> list:
    """The configured source URIs that are neither the snapshot nor the local repo."""
    allowed = _allowed(snapshot_uri, local_repo)
    return sorted({uri for uri in uris if normalize_uri(uri) not in allowed})


def violations(pkgs: Iterable[Installed], snapshot_uri: str, local_repo: str,
               exempt: frozenset) -> list:
    """Installed packages whose version neither the snapshot nor the local repo serves."""
    allowed = _allowed(snapshot_uri, local_repo)
    found = [p for p in pkgs if p.name not in exempt
             and not any(_canonical(uri).startswith(allowed) for uri in p.uris)]
    return sorted(found, key=lambda p: (p.name, p.arch))


def _package_lines(pkgs: Iterable[Installed]) -> list:
    return sorted(f"{p.name}:{p.arch}={p.version}" for p in pkgs)


def package_set_digest(pkgs: Iterable[Installed]) -> str:
    return hashlib.sha256("\n".join(_package_lines(pkgs)).encode()).hexdigest()


def snapshot_id_problems(fetch: Callable[[str], int], base_url: str, snapshot_id: str) -> list:
    """URLs a build needs from this snapshot that do not answer 200."""
    root = f"{base_url.rstrip('/')}/{snapshot_id}/dists"
    urls = [f"{root}/{series}{suite}/InRelease" for series in SERIES for suite in SUITES]
    urls += [f"{root}/{series}-updates/main/binary-{arch}/Packages.xz"
             for series in SERIES for arch in ARCHITECTURES]
    return [url for url in urls if fetch(url) != 200]


def _head(url: str) -> int:
    request = urllib.request.Request(url, method="HEAD")
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            return response.status
    except urllib.error.HTTPError as err:
        return err.code
    except (urllib.error.URLError, OSError):
        return 0


def _cmd_sources(args) -> int:
    import apt_pkg  # guest only: python3-apt is in the image, not on the host

    apt_pkg.init()
    sources = apt_pkg.SourceList()
    sources.read_main_list()
    uris = [index.uri for index in sources.list]
    bad = bad_sources(uris, args.snapshot_uri, args.local_repo)
    for uri in bad:
        print(f"not the snapshot or the local repository: {uri}", file=sys.stderr)
    if bad:
        return 1
    print(f"apt sources: {len(set(uris))}, all {args.snapshot_uri} or file:{args.local_repo}")
    return 0


def _cmd_versions(args) -> int:
    import apt  # guest only

    cache = apt.Cache()
    pkgs = [Installed(p.shortname, p.installed.architecture, p.installed.version,
                      tuple(p.installed.uris))
            for p in cache if p.is_installed]
    print(f"package set sha256: {package_set_digest(pkgs)} ({len(pkgs)} packages)")
    if args.list:
        print("\n".join(_package_lines(pkgs)))
    bad = violations(pkgs, args.snapshot_uri, args.local_repo, frozenset(args.exempt))
    for p in bad:
        print(f"not in snapshot: {p.name}:{p.arch}={p.version}", file=sys.stderr)
    return 1 if bad else 0


def _cmd_id(args) -> int:
    problems = snapshot_id_problems(_head, args.base_url, args.snapshot_id)
    for url in problems:
        print(f"not served: {url}", file=sys.stderr)
    if problems:
        return 1
    print(f"{args.snapshot_id} serves {', '.join(SERIES)} on {', '.join(ARCHITECTURES)}")
    return 0


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("sources", "versions"):
        guest = sub.add_parser(name)
        guest.add_argument("--snapshot-uri", required=True)
        guest.add_argument("--local-repo", default=DEFAULT_LOCAL_REPO)
        if name == "versions":
            guest.add_argument("--exempt", action="append", default=[])
            guest.add_argument("--list", action="store_true")
    snapshot = sub.add_parser("id")
    snapshot.add_argument("--base-url", required=True)
    snapshot.add_argument("--snapshot-id", required=True)
    args = parser.parse_args(argv)
    if args.command == "id" and not SNAPSHOT_ID.match(args.snapshot_id):
        parser.error(f"--snapshot-id must be YYYYMMDDTHHMMSSZ, not {args.snapshot_id!r}")
    return {"sources": _cmd_sources, "versions": _cmd_versions, "id": _cmd_id}[args.command](args)


if __name__ == "__main__":
    sys.exit(main())
