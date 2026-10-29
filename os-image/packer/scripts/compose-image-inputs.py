#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Compose authenticated component outputs into the NVKE Packer input archive."""

from __future__ import annotations

import argparse
from contextlib import contextmanager
import os
import pathlib
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile


ARCHIVE_BINDINGS = ("calico", "kubeadm-distro", "ovn", "spire-agent", "doca-ovs")
MAX_ARCHIVE_MEMBERS = 16384
MAX_PACKAGES = 16384
MAX_PACKAGE_BYTES = 1 << 30
MAX_TOTAL_PACKAGE_BYTES = 7 << 30
MAX_ARCHIVE_BYTES = MAX_TOTAL_PACKAGE_BYTES + MAX_ARCHIVE_MEMBERS * 1024 + tarfile.RECORDSIZE


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--materials-dir", type=pathlib.Path, required=True)
    parser.add_argument("--source-files-dir", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--source-date-epoch", type=int, required=True)
    parser.add_argument("--architecture", choices=("amd64", "arm64"), required=True)
    return parser.parse_args()


def require_regular(path: pathlib.Path, label: str) -> os.stat_result:
    try:
        info = path.lstat()
    except OSError as exc:
        raise SystemExit(f"{label}: cannot inspect {path}: {exc}") from exc
    if not stat.S_ISREG(info.st_mode):
        raise SystemExit(f"{label}: {path} is not one regular file")
    return info


@contextmanager
def open_regular(path: pathlib.Path, label: str):
    """Bind reads to one descriptor, refusing symlinks and non-regular files."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError as exc:
        raise SystemExit(f"{label}: cannot open {path}: {exc}") from exc
    with os.fdopen(fd, "rb") as source:
        if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
            raise SystemExit(f"{label}: {path} is not one regular file")
        yield source


def validate_archive_framing(source, path: pathlib.Path) -> None:
    """Use stdlib header validation before tarfile's permissive iteration.

    tarfile treats a damaged subsequent header as EOF and expands PAX/GNU
    metadata before yielding a member. This package-only interface needs
    neither extension, so reject them before their payloads can be expanded.
    """
    zero = bytes(tarfile.BLOCKSIZE)
    if os.fstat(source.fileno()).st_size > MAX_ARCHIVE_BYTES:
        raise SystemExit(f"package archive {path} exceeds its raw archive byte budget")
    count = 0
    while True:
        header = source.read(tarfile.BLOCKSIZE)
        if header.startswith((b"\x1f\x8b", b"\x28\xb5\x2f\xfd")):
            raise SystemExit(f"package archive {path} must be an uncompressed tar archive")
        if header == zero:
            if source.read(tarfile.BLOCKSIZE) != zero:
                raise SystemExit(f"package archive {path} lacks two end-of-archive zero blocks")
            while chunk := source.read(1 << 20):
                if source.tell() > MAX_ARCHIVE_BYTES:
                    raise SystemExit(f"package archive {path} exceeds its raw archive byte budget")
                if any(chunk):
                    raise SystemExit(f"package archive {path} has data after end-of-archive")
            source.seek(0)
            return
        try:
            member = tarfile.TarInfo.frombuf(header, "utf-8", "strict")
        except (tarfile.HeaderError, UnicodeError) as exc:
            raise SystemExit(f"package archive {path} is invalid: {exc}") from exc
        count += 1
        if count > MAX_ARCHIVE_MEMBERS:
            raise SystemExit(f"package archive {path} exceeds its member budget")
        # isreg() also accepts GNU sparse records, which activate a separate
        # metadata parser. Only ordinary file headers belong in this contract.
        if member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE) and not (member.isdir() and member.name in (".", "./")):
            raise SystemExit(f"package archive {path} has unsafe or unexpected member {member.name!r}")
        if member.isdir() and member.size != 0:
            raise SystemExit(f"package archive {path} root directory must have zero size")
        if member.size < 0 or member.size > MAX_PACKAGE_BYTES:
            raise SystemExit(f"package {member.name!r} exceeds its per-package size budget")
        padded_size = (member.size + tarfile.BLOCKSIZE - 1) // tarfile.BLOCKSIZE * tarfile.BLOCKSIZE
        # Seek over content without allocating it, but require its final byte.
        if padded_size:
            source.seek(padded_size - 1, os.SEEK_CUR)
            if not source.read(1):
                raise SystemExit(f"package archive {path} is invalid: unexpected end of data")


def stage_archive_debs(
    path: pathlib.Path, destination: pathlib.Path, names: set[str], total_bytes: int
) -> int:
    """Stream bounded regular members into a private stage, never extract paths."""
    try:
        with open_regular(path, "package archive") as source, \
                tempfile.TemporaryFile(dir=destination.parent) as snapshot:
            if os.fstat(source.fileno()).st_size > MAX_ARCHIVE_BYTES:
                raise SystemExit(f"package archive {path} exceeds its raw archive byte budget")
            copied = 0
            while chunk := source.read(1 << 20):
                copied += len(chunk)
                if copied > MAX_ARCHIVE_BYTES:
                    raise SystemExit(f"package archive {path} exceeds its raw archive byte budget")
                snapshot.write(chunk)
            snapshot.seek(0)
            # Neither path replacement nor in-place input writes can change
            # these private bytes between the two stdlib validation passes.
            validate_archive_framing(snapshot, path)
            archive = tarfile.open(fileobj=snapshot, mode="r:")
            with archive:
                return _stage_archive_debs(archive, path, destination, names, total_bytes)
    except tarfile.TarError as exc:
        raise SystemExit(f"package archive {path} is invalid: {exc}") from exc


def _stage_archive_debs(archive, path, destination, names, total_bytes):
    found = False
    for count, member in enumerate(archive, 1):
        if count > MAX_ARCHIVE_MEMBERS:
            raise SystemExit(f"package archive {path} exceeds its member budget")
        name = member.name.removeprefix("./")
        if member.isdir() and name in ("", "."):
            continue
        # This restricts transport filenames; dpkg-deb validates the package.
        if (not member.isreg() or pathlib.PurePosixPath(name).name != name
                or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+~_:-]*\.deb", name) is None):
            raise SystemExit(f"package archive {path} has unsafe or unexpected member {member.name!r}")
        if name in names:
            raise SystemExit(f"duplicate Debian package filename {name!r}")
        if len(names) >= MAX_PACKAGES:
            raise SystemExit(f"package archive {path} exceeds its package count budget ({MAX_PACKAGES})")
        if member.size == 0:
            raise SystemExit(f"package {name!r} is empty")
        if not 0 < member.size <= MAX_PACKAGE_BYTES:
            raise SystemExit(f"package {name!r} exceeds its per-package size budget ({MAX_PACKAGE_BYTES} bytes)")
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
    if not found:
        raise SystemExit(f"package archive {path} contains no Debian packages")
    return total_bytes


def install_file(
    source: pathlib.Path, destination: pathlib.Path, mode: int, *, max_bytes: int | None = None
) -> int:
    copied = 0
    with open_regular(source, "source file") as src, destination.open("xb") as dst:
        while chunk := src.read(1 << 20):
            if max_bytes is not None and len(chunk) > max_bytes - copied:
                raise SystemExit("source file exceeds its byte budget")
            dst.write(chunk)
            copied += len(chunk)
    destination.chmod(mode)
    return copied


def require_architecture(package: pathlib.Path, architecture: str) -> None:
    result = subprocess.run(
        ["dpkg-deb", "-f", str(package), "Architecture"],
        check=False, capture_output=True, text=True, errors="replace", timeout=30,
    )
    if result.returncode:
        raise SystemExit(f"cannot read Debian package architecture for {package.name}: {result.stderr.strip()}")
    observed = result.stdout.strip()
    if observed not in (architecture, "all"):
        raise SystemExit(f"package {package.name!r} has architecture {observed!r}, expected {architecture!r} or 'all'")


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
    for tool in ("dpkg-deb", "dpkg-scanpackages"):
        if shutil.which(tool) is None:
            raise SystemExit(f"{tool} is required (apt install dpkg-dev)")

    output = args.output.absolute()
    output.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    if output.is_symlink() or (output.exists() and not output.is_file()):
        raise SystemExit(f"refusing unsafe output path {output}")

    with tempfile.TemporaryDirectory(prefix=".image-inputs.", dir=output.parent) as temporary:
        temporary_root = pathlib.Path(temporary)
        stage = temporary_root / "root"
        debs = stage / "debs"
        debs.mkdir(mode=0o755, parents=True)

        agent = args.materials_dir / "agent"
        total_bytes = require_regular(agent, "agent package").st_size
        if total_bytes == 0:
            raise SystemExit("agent package is empty")
        if total_bytes > MAX_PACKAGE_BYTES:
            raise SystemExit("agent package exceeds its size budget")
        if total_bytes > MAX_TOTAL_PACKAGE_BYTES:
            raise SystemExit("image inputs exceed their total package byte budget")
        total_bytes = install_file(
            agent, stage / "nvke-agent.deb", 0o644,
            max_bytes=min(MAX_PACKAGE_BYTES, MAX_TOTAL_PACKAGE_BYTES),
        )
        if total_bytes == 0:
            raise SystemExit("agent package is empty")
        require_architecture(stage / "nvke-agent.deb", args.architecture)
        for name, mode in (
            ("nvke-agent-spire.conf", 0o644),
            ("wait-spiffe-socket.sh", 0o755),
            ("conntrack-tune.service", 0o644),
            ("configure-conntrack.sh", 0o755),
            ("10-nke-conntrack-timeouts.conf", 0o644),
            ("99-disable-network-config.cfg", 0o644),
            ("00-dhcp-all.yaml", 0o644),
        ):
            install_file(args.source_files_dir / name, stage / name, mode)

        names: set[str] = set()
        for binding in ARCHIVE_BINDINGS:
            previous_names = names.copy()
            total_bytes = stage_archive_debs(args.materials_dir / binding, debs, names, total_bytes)
            binding_names = names - previous_names
            if binding == "spire-agent" and len([name for name in binding_names if name.startswith("spire-agent_")]) != 1:
                raise SystemExit("spire-agent binding must contain exactly one spire-agent_*.deb")
            for name in sorted(binding_names):
                require_architecture(debs / name, args.architecture)
        if len(list(debs.glob("spire-agent_*.deb"))) != 1:
            raise SystemExit("image inputs must contain exactly one spire-agent_*.deb across all bindings")
        # provision.sh installs it by name; two versions would make the pick apt's.
        if len(list(debs.glob("doca-openvswitch-switch_*.deb"))) != 1:
            raise SystemExit("image inputs must contain exactly one doca-openvswitch-switch_*.deb across all bindings")
        if len(list(debs.glob("nvke-cni-calico-manifests-*.deb"))) > 1:
            raise SystemExit("calico inputs must contain at most one nvke-cni-calico-manifests-*.deb")

        environment = os.environ.copy()
        environment.update({"LC_ALL": "C", "LANG": "C", "TZ": "UTC"})
        try:
            with (debs / "Packages").open("xb") as index:
                subprocess.run(
                    ["dpkg-scanpackages", "--multiversion", ".", "/dev/null"],
                    cwd=debs,
                    env=environment,
                    check=True,
                    stdin=subprocess.DEVNULL,
                    stdout=index,
                    timeout=300,
                )
        except subprocess.CalledProcessError as exc:
            raise SystemExit(f"dpkg-scanpackages failed with exit status {exc.returncode}") from exc
        (debs / "Packages").chmod(0o644)

        staged_output = temporary_root / "image-inputs.tar"
        write_deterministic_archive(stage, staged_output, args.source_date_epoch)
        os.replace(staged_output, output)


if __name__ == "__main__":
    try:
        main()
    except OSError as exc:
        raise SystemExit(f"cannot compose image inputs: {exc}") from exc
    except subprocess.TimeoutExpired as exc:
        raise SystemExit(f"image input tool {exc.cmd[0]} exceeded its time limit") from exc
