#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Finalize one Packer QCOW2 and emit its deterministic build manifest."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile
from typing import Any, Sequence
from urllib.parse import urlparse


API_VERSION = "nke.nvidia.com/os-image-build-manifest/v1"
KIND = "OSImageBuildManifest"
MEDIA_TYPE = "application/vnd.qemu.qcow2"
MANIFEST_SUFFIX = ".build-manifest.v1.json"
ARCHES = ("amd64", "arm64")
VARIANTS = ("fde", "non-fde")
ACCELERATORS = ("kvm", "tcg")
ISO_NAME_RE = re.compile(
    r"^ubuntu-(?P<version>[0-9]+(?:\.[0-9]+)+)-live-server-"
    r"(?P<arch>amd64|arm64)\.iso$"
)
OUTPUT_NAME_RE = re.compile(
    r"^nke-node-(?P<variant>fde|non-fde)-ubuntu-"
    r"[0-9]+(?:\.[0-9]+)+-(?P<arch>amd64|arm64)\.qcow2$"
)
PACKER_VERSION_RE = re.compile(r"^Packer v(?P<version>[0-9][0-9A-Za-z.+-]*)$")
QEMU_IMG_VERSION_RE = re.compile(
    r"^qemu-img version (?P<version>[0-9][0-9A-Za-z.+:~-]*)"
)
GIT_OBJECT_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")


class FinalizerError(RuntimeError):
    """A fail-closed finalization or verification error."""


@dataclass(frozen=True)
class VerificationExpectations:
    """Identity selected by the caller's build or publication lane."""

    variant: str
    architecture: str
    source_commit: str
    source_tree: str
    accelerator: str

    def validate(self) -> None:
        if self.variant not in VARIANTS:
            raise FinalizerError(f"unsupported expected variant {self.variant!r}")
        if self.architecture not in ARCHES:
            raise FinalizerError(
                f"unsupported expected architecture {self.architecture!r}"
            )
        if self.accelerator not in ACCELERATORS:
            raise FinalizerError(
                f"unsupported required accelerator {self.accelerator!r}"
            )
        if (
            not isinstance(self.source_commit, str)
            or GIT_OBJECT_RE.fullmatch(self.source_commit) is None
        ):
            raise FinalizerError("expected source commit is not a full Git object ID")
        if (
            not isinstance(self.source_tree, str)
            or GIT_OBJECT_RE.fullmatch(self.source_tree) is None
        ):
            raise FinalizerError("expected source tree is not a full Git object ID")


def run_checked(argv: Sequence[str], cwd: Path | None = None) -> str:
    try:
        result = subprocess.run(
            argv,
            cwd=cwd,
            check=False,
            capture_output=True,
            text=True,
        )
    except OSError as exc:
        raise FinalizerError(f"could not execute {argv[0]!r}: {exc}") from exc
    if result.returncode != 0:
        detail = result.stderr.strip() or result.stdout.strip() or "no diagnostic"
        raise FinalizerError(
            f"command {argv[0]!r} exited {result.returncode}: {detail}"
        )
    return result.stdout


def first_nonempty_line(output: str, tool: str) -> str:
    for line in output.splitlines():
        value = line.strip()
        if value:
            return value
    raise FinalizerError(f"{tool} returned no version")


def tool_versions(packer: str, qemu_img: str) -> dict[str, dict[str, str]]:
    packer_line = first_nonempty_line(run_checked([packer, "version"]), "packer")
    packer_match = PACKER_VERSION_RE.fullmatch(packer_line)
    if packer_match is None:
        raise FinalizerError(f"unrecognized Packer version output: {packer_line!r}")

    qemu_line = first_nonempty_line(run_checked([qemu_img, "--version"]), "qemu-img")
    qemu_match = QEMU_IMG_VERSION_RE.match(qemu_line)
    if qemu_match is None:
        raise FinalizerError(f"unrecognized qemu-img version output: {qemu_line!r}")

    return {
        "packer": {"version": packer_match.group("version")},
        "qemuImg": {"version": qemu_match.group("version")},
    }


def source_observation(source_root: Path) -> dict[str, str | bool]:
    commit = run_checked(["git", "rev-parse", "HEAD"], cwd=source_root).strip()
    tree = run_checked(["git", "rev-parse", "HEAD^{tree}"], cwd=source_root).strip()
    if GIT_OBJECT_RE.fullmatch(commit) is None:
        raise FinalizerError(f"git returned an invalid commit object: {commit!r}")
    if GIT_OBJECT_RE.fullmatch(tree) is None:
        raise FinalizerError(f"git returned an invalid tree object: {tree!r}")
    status_output = run_checked(
        ["git", "status", "--porcelain=v1", "--untracked-files=normal"],
        cwd=source_root,
    )
    if status_output:
        raise FinalizerError(
            "source checkout is dirty; B1 can identify only an exact clean Git tree"
        )
    return {"commit": commit, "dirty": False, "tree": tree}


def expected_image_name(variant: str, arch: str, iso_url: str) -> str:
    iso_name = Path(urlparse(iso_url).path).name
    match = ISO_NAME_RE.fullmatch(iso_name)
    if match is None:
        raise FinalizerError(
            "ISO URL must end in ubuntu-<version>-live-server-<arch>.iso"
        )
    iso_arch = match.group("arch")
    if iso_arch != arch:
        raise FinalizerError(
            f"ISO architecture {iso_arch!r} does not match requested architecture {arch!r}"
        )
    return f"nke-node-{variant}-ubuntu-{match.group('version')}-{arch}.qcow2"


def require_single_image(output_dir: Path, expected_name: str) -> Path:
    try:
        images = sorted(output_dir.glob("*.qcow2"))
    except OSError as exc:
        raise FinalizerError(f"could not inspect output directory: {exc}") from exc
    if len(images) != 1:
        raise FinalizerError(
            f"expected exactly one QCOW2 in {output_dir.name!r}, found {len(images)}"
        )
    image = images[0]
    if image.name != expected_name:
        raise FinalizerError(
            f"expected QCOW2 {expected_name!r}, found {image.name!r}"
        )
    require_regular_file(image, "QCOW2")
    return image


def require_regular_file(path: Path, description: str) -> None:
    try:
        mode = path.lstat().st_mode
    except OSError as exc:
        raise FinalizerError(f"could not inspect {description} {path.name!r}: {exc}") from exc
    if not stat.S_ISREG(mode):
        raise FinalizerError(f"{description} {path.name!r} is not a regular file")


def qemu_metadata(image: Path, qemu_img: str) -> dict[str, int | str]:
    output = run_checked([qemu_img, "info", "--output=json", str(image)])
    try:
        info = json.loads(output)
    except json.JSONDecodeError as exc:
        raise FinalizerError(f"qemu-img returned invalid JSON: {exc}") from exc
    if not isinstance(info, dict):
        raise FinalizerError("qemu-img metadata is not a JSON object")
    if info.get("format") != "qcow2":
        raise FinalizerError(
            f"qemu-img reported format {info.get('format')!r}, expected 'qcow2'"
        )
    virtual_size = info.get("virtual-size")
    if isinstance(virtual_size, bool) or not isinstance(virtual_size, int):
        raise FinalizerError("qemu-img virtual-size is not an integer")
    if virtual_size <= 0:
        raise FinalizerError("qemu-img virtual-size must be positive")
    if info.get("encrypted"):
        raise FinalizerError(
            "final QCOW2 container encryption is unsupported; FDE must be guest LUKS"
        )
    for field in ("backing-filename", "full-backing-filename"):
        if info.get(field):
            raise FinalizerError(
                f"final QCOW2 must be self-contained, but qemu-img reported {field}"
            )
    return {"format": "qcow2", "virtualSizeBytes": virtual_size}


def hash_file(path: Path) -> tuple[str, int]:
    digest = hashlib.sha256()
    size = 0
    try:
        with path.open("rb") as stream:
            while chunk := stream.read(1024 * 1024):
                digest.update(chunk)
                size += len(chunk)
    except OSError as exc:
        raise FinalizerError(f"could not hash {path.name!r}: {exc}") from exc
    return f"sha256:{digest.hexdigest()}", size


def fsync_file(path: Path) -> None:
    try:
        with path.open("rb") as stream:
            os.fsync(stream.fileno())
    except OSError as exc:
        raise FinalizerError(f"could not sync {path.name!r}: {exc}") from exc


def stage_payload(path: Path, payload: bytes) -> Path:
    try:
        fd, temporary_name = tempfile.mkstemp(
            prefix=f".{path.name}.", suffix=".tmp", dir=path.parent
        )
    except OSError as exc:
        raise FinalizerError(f"could not stage {path.name!r}: {exc}") from exc
    temporary = Path(temporary_name)
    try:
        os.fchmod(fd, 0o644)
        with os.fdopen(fd, "wb") as stream:
            fd = -1
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        return temporary
    except OSError as exc:
        try:
            temporary.unlink()
        except OSError:
            pass
        raise FinalizerError(f"could not stage {path.name!r}: {exc}") from exc
    finally:
        if fd >= 0:
            try:
                os.close(fd)
            except OSError:
                pass


def fsync_directory(path: Path) -> None:
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0)
    try:
        descriptor = os.open(path, flags)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except OSError as exc:
        raise FinalizerError(f"could not sync output directory: {exc}") from exc


def qemu_temporary_path(image: Path) -> Path:
    descriptor = -1
    try:
        descriptor, name = tempfile.mkstemp(
            prefix=f".{image.name}.final.", suffix=".qcow2", dir=image.parent
        )
        os.close(descriptor)
        descriptor = -1
        temporary = Path(name)
        temporary.unlink()
        return temporary
    except OSError as exc:
        raise FinalizerError(f"could not reserve final QCOW2 path: {exc}") from exc
    finally:
        if descriptor >= 0:
            try:
                os.close(descriptor)
            except OSError:
                pass


def replace_file(source: Path, destination: Path, description: str) -> None:
    try:
        os.replace(source, destination)
    except OSError as exc:
        raise FinalizerError(f"could not replace {description}: {exc}") from exc


def manifest_bytes(manifest: dict[str, Any]) -> bytes:
    return (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode("utf-8")


def unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise FinalizerError(f"manifest contains duplicate field {key!r}")
        result[key] = value
    return result


def exact_keys(value: Any, expected: set[str], location: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise FinalizerError(f"manifest {location} must be an object")
    actual = set(value)
    if not all(isinstance(key, str) for key in actual):
        raise FinalizerError(f"manifest {location} field names must be strings")
    if actual != expected:
        raise FinalizerError(
            f"manifest {location} fields must be {sorted(expected)}, got {sorted(actual)}"
        )
    return value


def load_manifest(path: Path) -> dict[str, Any]:
    try:
        raw = path.read_text(encoding="utf-8")
        value = json.loads(raw, object_pairs_hook=unique_object)
    except OSError as exc:
        raise FinalizerError(f"could not read {path.name!r}: {exc}") from exc
    except UnicodeError as exc:
        raise FinalizerError(f"manifest {path.name!r} is not UTF-8: {exc}") from exc
    except json.JSONDecodeError as exc:
        raise FinalizerError(f"manifest {path.name!r} is invalid JSON: {exc}") from exc
    return exact_keys(
        value,
        {"apiVersion", "build", "kind", "output", "source", "tools"},
        "root",
    )


def validate_manifest_shape(manifest: dict[str, Any]) -> None:
    if manifest["apiVersion"] != API_VERSION:
        raise FinalizerError(f"unsupported manifest apiVersion {manifest['apiVersion']!r}")
    if manifest["kind"] != KIND:
        raise FinalizerError(f"unsupported manifest kind {manifest['kind']!r}")

    build = exact_keys(
        manifest["build"],
        {"accelerator", "architecture", "target", "variant"},
        "build",
    )
    if build["architecture"] not in ARCHES:
        raise FinalizerError("manifest build.architecture is unsupported")
    if build["variant"] not in VARIANTS:
        raise FinalizerError("manifest build.variant is unsupported")
    if build["accelerator"] not in ACCELERATORS:
        raise FinalizerError("manifest build.accelerator is unsupported")
    if build["target"] != f"os-image-{build['variant']}":
        raise FinalizerError("manifest build.target does not match build.variant")

    source = exact_keys(manifest["source"], {"commit", "dirty", "tree"}, "source")
    if not isinstance(source["commit"], str) or GIT_OBJECT_RE.fullmatch(source["commit"]) is None:
        raise FinalizerError("manifest source.commit is not a full Git object ID")
    if not isinstance(source["tree"], str) or GIT_OBJECT_RE.fullmatch(source["tree"]) is None:
        raise FinalizerError("manifest source.tree is not a full Git object ID")
    if source["dirty"] is not False:
        raise FinalizerError("manifest source.dirty must be false")

    tools = exact_keys(manifest["tools"], {"packer", "qemuImg"}, "tools")
    packer = exact_keys(tools["packer"], {"version"}, "tools.packer")
    if (
        not isinstance(packer["version"], str)
        or re.fullmatch(r"[0-9][0-9A-Za-z.+-]*", packer["version"]) is None
    ):
        raise FinalizerError("manifest tools.packer.version is invalid")
    qemu_img = exact_keys(tools["qemuImg"], {"version"}, "tools.qemuImg")
    if (
        not isinstance(qemu_img["version"], str)
        or re.fullmatch(r"[0-9][0-9A-Za-z.+:~-]*", qemu_img["version"]) is None
    ):
        raise FinalizerError("manifest tools.qemuImg.version is invalid")

    output = exact_keys(
        manifest["output"],
        {"fileName", "format", "mediaType", "sha256", "sizeBytes", "virtualSizeBytes"},
        "output",
    )
    file_name = output["fileName"]
    if not isinstance(file_name, str) or Path(file_name).name != file_name:
        raise FinalizerError("manifest output.fileName must be a basename")
    output_match = OUTPUT_NAME_RE.fullmatch(file_name)
    if output_match is None:
        raise FinalizerError("manifest output.fileName is not a supported NKE image name")
    if output_match.group("variant") != build["variant"]:
        raise FinalizerError("manifest output.fileName does not match build.variant")
    if output_match.group("arch") != build["architecture"]:
        raise FinalizerError("manifest output.fileName does not match build.architecture")
    if output["format"] != "qcow2" or output["mediaType"] != MEDIA_TYPE:
        raise FinalizerError("manifest output format or media type is unsupported")
    if not isinstance(output["sha256"], str) or SHA256_RE.fullmatch(output["sha256"]) is None:
        raise FinalizerError("manifest output.sha256 is invalid")
    for field in ("sizeBytes", "virtualSizeBytes"):
        item = output[field]
        if isinstance(item, bool) or not isinstance(item, int) or item <= 0:
            raise FinalizerError(f"manifest output.{field} must be a positive integer")


def verify(
    image: Path,
    manifest_path: Path,
    qemu_img: str,
    expected: VerificationExpectations,
) -> None:
    expected.validate()
    if manifest_path.parent != image.parent:
        raise FinalizerError("build manifest must be adjacent to the QCOW2")
    if manifest_path.name != image.name + MANIFEST_SUFFIX:
        raise FinalizerError("build manifest filename does not match the QCOW2")
    require_regular_file(image, "QCOW2")
    require_regular_file(manifest_path, "build manifest")
    manifest = load_manifest(manifest_path)
    validate_manifest_shape(manifest)
    build = manifest["build"]
    source = manifest["source"]
    expected_fields = (
        ("build.variant", build["variant"], expected.variant),
        ("build.architecture", build["architecture"], expected.architecture),
        ("build.accelerator", build["accelerator"], expected.accelerator),
        ("source.commit", source["commit"], expected.source_commit),
        ("source.tree", source["tree"], expected.source_tree),
    )
    for field, actual, wanted in expected_fields:
        if actual != wanted:
            raise FinalizerError(
                f"manifest {field} {actual!r} does not match expected {wanted!r}"
            )
    output = manifest["output"]
    if output["fileName"] != image.name:
        raise FinalizerError("manifest output.fileName does not match the QCOW2")
    digest, size = hash_file(image)
    if output["sha256"] != digest or output["sizeBytes"] != size:
        raise FinalizerError("manifest digest or size does not match the QCOW2 bytes")
    metadata = qemu_metadata(image, qemu_img)
    if output["format"] != metadata["format"]:
        raise FinalizerError("manifest format does not match qemu-img metadata")
    if output["virtualSizeBytes"] != metadata["virtualSizeBytes"]:
        raise FinalizerError("manifest virtual size does not match qemu-img metadata")


def finalize(args: argparse.Namespace) -> tuple[Path, Path]:
    output_dir = args.output_dir.resolve()
    expected_name = expected_image_name(args.variant, args.arch, args.iso_url)
    image = require_single_image(output_dir, expected_name)
    source = source_observation(args.source_root.resolve())
    tools = tool_versions(args.packer, args.qemu_img)

    temporary = qemu_temporary_path(image)
    manifest_temporary: Path | None = None
    try:
        run_checked(
            [
                args.qemu_img,
                "convert",
                "-p",
                "-c",
                "-O",
                "qcow2",
                str(image),
                str(temporary),
            ]
        )
        try:
            os.chmod(temporary, 0o644)
        except OSError as exc:
            raise FinalizerError(f"could not set final QCOW2 mode: {exc}") from exc
        metadata = qemu_metadata(temporary, args.qemu_img)
        digest, size = hash_file(temporary)
        if size <= 0:
            raise FinalizerError("final QCOW2 is empty")
        fsync_file(temporary)

        manifest = {
            "apiVersion": API_VERSION,
            "build": {
                "accelerator": args.accelerator,
                "architecture": args.arch,
                "target": f"os-image-{args.variant}",
                "variant": args.variant,
            },
            "kind": KIND,
            "output": {
                "fileName": image.name,
                "format": metadata["format"],
                "mediaType": MEDIA_TYPE,
                "sha256": digest,
                "sizeBytes": size,
                "virtualSizeBytes": metadata["virtualSizeBytes"],
            },
            "source": source,
            "tools": tools,
        }
        validate_manifest_shape(manifest)
        manifest_path = image.with_name(image.name + MANIFEST_SUFFIX)
        manifest_temporary = stage_payload(manifest_path, manifest_bytes(manifest))
        replace_file(temporary, image, "final QCOW2")
        replace_file(manifest_temporary, manifest_path, "build manifest")
        manifest_temporary = None
        fsync_directory(output_dir)
        verify(
            image,
            manifest_path,
            args.qemu_img,
            VerificationExpectations(
                variant=args.variant,
                architecture=args.arch,
                source_commit=source["commit"],
                source_tree=source["tree"],
                accelerator=args.accelerator,
            ),
        )
        return image, manifest_path
    finally:
        try:
            temporary.unlink()
        except OSError:
            pass
        if manifest_temporary is not None:
            try:
                manifest_temporary.unlink()
            except OSError:
                pass


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    subcommands = result.add_subparsers(dest="command", required=True)

    finalize_parser = subcommands.add_parser(
        "finalize", help="compress one exact Packer output and emit its manifest"
    )
    finalize_parser.add_argument("--variant", choices=VARIANTS, required=True)
    finalize_parser.add_argument("--arch", choices=ARCHES, required=True)
    finalize_parser.add_argument("--accelerator", choices=ACCELERATORS, required=True)
    finalize_parser.add_argument("--iso-url", required=True)
    finalize_parser.add_argument("--output-dir", type=Path, required=True)
    finalize_parser.add_argument("--source-root", type=Path, required=True)
    finalize_parser.add_argument("--packer", default="packer")
    finalize_parser.add_argument("--qemu-img", default="qemu-img")

    verify_parser = subcommands.add_parser(
        "verify", help="verify an adjacent manifest against exact QCOW2 bytes"
    )
    verify_parser.add_argument("--image", type=Path, required=True)
    verify_parser.add_argument("--manifest", type=Path, required=True)
    verify_parser.add_argument("--expected-variant", choices=VARIANTS, required=True)
    verify_parser.add_argument("--expected-arch", choices=ARCHES, required=True)
    verify_parser.add_argument("--expected-source-commit", required=True)
    verify_parser.add_argument("--expected-source-tree", required=True)
    verify_parser.add_argument(
        "--required-accelerator", choices=ACCELERATORS, required=True
    )
    verify_parser.add_argument("--qemu-img", default="qemu-img")
    return result


def main(argv: Sequence[str] | None = None) -> int:
    args = parser().parse_args(argv)
    try:
        if args.command == "finalize":
            image, manifest = finalize(args)
            print(f"finalized {image.name}")
            print(f"manifest {manifest.name}")
        else:
            image = Path(os.path.abspath(args.image))
            manifest = Path(os.path.abspath(args.manifest))
            verify(
                image,
                manifest,
                args.qemu_img,
                VerificationExpectations(
                    variant=args.expected_variant,
                    architecture=args.expected_arch,
                    source_commit=args.expected_source_commit,
                    source_tree=args.expected_source_tree,
                    accelerator=args.required_accelerator,
                ),
            )
            print(f"verified {args.image.name}")
    except (FinalizerError, OSError) as exc:
        print(f"finalize-qcow2: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
