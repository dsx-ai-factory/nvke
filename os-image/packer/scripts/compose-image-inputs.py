#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Compose authenticated component outputs into the NKE Packer input archive."""

from __future__ import annotations

import argparse
import os
import pathlib
import shutil
import stat
import subprocess
import tarfile
import tempfile


ARCHIVE_BINDINGS = ("calico", "kubeadm-distro", "ovn", "spire-agent")
MAX_ARCHIVE_MEMBERS = 16384
MAX_PACKAGES = 16384
MAX_PACKAGE_BYTES = 1 << 30
MAX_TOTAL_PACKAGE_BYTES = 7 << 30


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--materials-dir", type=pathlib.Path, required=True)
    parser.add_argument("--source-files-dir", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--source-date-epoch", type=int, required=True)
    return parser.parse_args()


def require_regular(path: pathlib.Path, label: str) -> os.stat_result:
    try:
        info = path.lstat()
    except OSError as exc:
        raise SystemExit(f"{label}: cannot inspect {path}: {exc}") from exc
    if not stat.S_ISREG(info.st_mode):
        raise SystemExit(f"{label}: {path} is not one regular file")
    return info


def stage_archive_debs(
    path: pathlib.Path, destination: pathlib.Path, names: set[str], total_bytes: int
) -> int:
    """Stream bounded regular members into a private stage, never extract paths."""
    require_regular(path, "package archive")
    found = False
    try:
        with tarfile.open(path, mode="r|") as archive:
            for count, member in enumerate(archive, 1):
                if count > MAX_ARCHIVE_MEMBERS:
                    raise SystemExit(f"package archive {path} exceeds its member budget")
                name = member.name.removeprefix("./")
                if member.isdir() and name in ("", "."):
                    continue
                if (
                    not member.isreg()
                    or pathlib.PurePosixPath(name).name != name
                    or not name.endswith(".deb")
                ):
                    raise SystemExit(
                        f"package archive {path} has unsafe or unexpected member {member.name!r}"
                    )
                if name in names:
                    raise SystemExit(f"duplicate Debian package filename {name!r}")
                if len(names) >= MAX_PACKAGES or not 0 < member.size <= MAX_PACKAGE_BYTES:
                    raise SystemExit(f"package archive {path} exceeds its package count or size budget")
                total_bytes += member.size
                if total_bytes > MAX_TOTAL_PACKAGE_BYTES:
                    raise SystemExit("image inputs exceed their total package byte budget")
                source = archive.extractfile(member)
                if source is None:
                    raise SystemExit(f"package archive {path} cannot read {member.name!r}")
                target = destination / name
                with source, target.open("xb") as output:
                    shutil.copyfileobj(source, output, length=1 << 20)
                target.chmod(0o644)
                names.add(name)
                found = True
    except (OSError, tarfile.TarError) as exc:
        raise SystemExit(f"package archive {path} is invalid: {exc}") from exc
    if not found:
        raise SystemExit(f"package archive {path} contains no Debian packages")
    return total_bytes


def install_file(source: pathlib.Path, destination: pathlib.Path, mode: int) -> None:
    require_regular(source, "source file")
    with source.open("rb") as src, destination.open("xb") as dst:
        shutil.copyfileobj(src, dst)
    destination.chmod(mode)


def write_deterministic_archive(source: pathlib.Path, output: pathlib.Path, epoch: int) -> None:
    paths = [source, *sorted(source.rglob("*"), key=lambda path: path.relative_to(source).as_posix())]
    with tarfile.open(output, mode="w", format=tarfile.GNU_FORMAT) as archive:
        for path in paths:
            name = "." if path == source else path.relative_to(source).as_posix()
            info = archive.gettarinfo(str(path), arcname=name)
            if info.isdir():
                info.mode = 0o755
            info.uid = 0
            info.gid = 0
            info.uname = ""
            info.gname = ""
            info.mtime = epoch
            if info.isfile():
                with path.open("rb") as content:
                    archive.addfile(info, content)
            else:
                archive.addfile(info)


def main() -> None:
    args = parse_args()
    if args.source_date_epoch <= 0:
        raise SystemExit("--source-date-epoch must be a positive integer")

    output = args.output.absolute()
    output.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    if output.is_symlink() or (output.exists() and not output.is_file()):
        raise SystemExit(f"refusing unsafe output path {output}")

    with tempfile.TemporaryDirectory(prefix=".image-inputs.", dir=output.parent) as temporary:
        temporary_root = pathlib.Path(temporary)
        stage = temporary_root / "root"
        debs = stage / "debs"
        debs.mkdir(mode=0o755, parents=True)

        install_file(args.materials_dir / "agent", stage / "nke-agent.deb", 0o644)
        install_file(
            args.source_files_dir / "nke-agent-spire.conf",
            stage / "nke-agent-spire.conf",
            0o644,
        )
        install_file(
            args.source_files_dir / "wait-spiffe-socket.sh",
            stage / "wait-spiffe-socket.sh",
            0o755,
        )

        names: set[str] = set()
        total_bytes = 0
        for binding in ARCHIVE_BINDINGS:
            total_bytes = stage_archive_debs(args.materials_dir / binding, debs, names, total_bytes)

        environment = os.environ.copy()
        environment.update({"LC_ALL": "C", "LANG": "C", "TZ": "UTC"})
        index = subprocess.run(
            ["dpkg-scanpackages", "--multiversion", ".", "/dev/null"],
            cwd=debs,
            env=environment,
            check=True,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
        ).stdout
        (debs / "Packages").write_bytes(index)
        (debs / "Packages").chmod(0o644)

        staged_output = temporary_root / "image-inputs.tar"
        write_deterministic_archive(stage, staged_output, args.source_date_epoch)
        os.replace(staged_output, output)


if __name__ == "__main__":
    main()
