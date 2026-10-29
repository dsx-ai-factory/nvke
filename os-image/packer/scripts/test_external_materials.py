# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the source-owned external-runner material adapters."""

import hashlib
import importlib.util
import io
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


SCRIPTS = Path(__file__).resolve().parent


def write_executable(path: Path, content: str) -> None:
    path.write_text(content, encoding="utf-8")
    path.chmod(0o755)


def write_package_archive(path: Path, package_name: str) -> None:
    payload = f"package:{package_name}\n".encode()
    with tarfile.open(path, "w") as archive:
        member = tarfile.TarInfo(package_name)
        member.mode = 0o644
        member.size = len(payload)
        archive.addfile(member, io.BytesIO(payload))


def require_real_tools(test, *tools):
    missing = [tool for tool in tools if shutil.which(tool) is None]
    if not missing:
        return
    message = f"real test tools are required: {', '.join(missing)}"
    if os.environ.get("NKE_REQUIRE_REAL_TOOLS") == "1":
        test.fail(message)
    test.skipTest(message)


class ComposeImageInputsTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.materials = self.root / "materials"
        self.source_files = self.root / "source-files"
        self.tools = self.root / "tools"
        for directory in (self.materials, self.source_files, self.tools):
            directory.mkdir()
        (self.materials / "agent").write_bytes(b"agent-deb\n")
        for binding, name in (
            ("calico", "calico.deb"),
            ("kubeadm-distro", "kubeadm-distro.deb"),
            ("ovn", "ovn.deb"),
            ("spire-agent", "spire-agent_1.0_all.deb"),
            ("doca-ovs", "doca-openvswitch-switch_1.0_all.deb"),
        ):
            write_package_archive(self.materials / binding, name)
        for name in ("nvke-agent-spire.conf", "wait-spiffe-socket.sh",
                     "conntrack-tune.service", "configure-conntrack.sh",
                     "10-nke-conntrack-timeouts.conf", "99-disable-network-config.cfg",
                     "00-dhcp-all.yaml"):
            shutil.copyfile(SCRIPTS.parent.parent / "files" / name, self.source_files / name)
        write_executable(
            self.tools / "dpkg-scanpackages",
            "#!/bin/sh\nprintf 'Package: fixture\\n'\n",
        )
        write_executable(self.tools / "dpkg-deb", "#!/bin/sh\nprintf 'all\\n'\n")

    def tearDown(self):
        self.temporary.cleanup()

    def run_compose(self, output: Path, umask: int = -1, architecture: str = "amd64"):
        environment = os.environ.copy()
        environment["PATH"] = f"{self.tools}:{environment['PATH']}"
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPTS / "compose-image-inputs.py"),
                "--architecture", architecture,
                "--materials-dir",
                str(self.materials),
                "--source-files-dir",
                str(self.source_files),
                "--output",
                str(output),
                "--source-date-epoch",
                "1787932620",
            ],
            env=environment,
            check=False,
            capture_output=True,
            text=True,
            umask=umask,
        )

    def test_composes_deterministic_archive(self):
        first = self.root / "first.tar"
        second = self.root / "second.tar"
        first_result = self.run_compose(first)
        second_result = self.run_compose(second)
        self.assertEqual(first_result.returncode, 0, first_result.stderr)
        self.assertEqual(second_result.returncode, 0, second_result.stderr)
        self.assertEqual(
            hashlib.sha256(first.read_bytes()).digest(),
            hashlib.sha256(second.read_bytes()).digest(),
        )
        with tarfile.open(first) as archive:
            names = {member.name.removeprefix("./") for member in archive.getmembers()}
        self.assertEqual(
            names,
            {
                ".",
                "debs",
                "debs/Packages",
                "debs/calico.deb",
                "debs/kubeadm-distro.deb",
                "debs/ovn.deb",
                "debs/spire-agent_1.0_all.deb",
                "debs/doca-openvswitch-switch_1.0_all.deb",
                "nvke-agent-spire.conf",
                "nvke-agent.deb",
                "wait-spiffe-socket.sh",
                "conntrack-tune.service",
                "configure-conntrack.sh",
                "10-nke-conntrack-timeouts.conf",
                "99-disable-network-config.cfg",
                "00-dhcp-all.yaml",
            },
        )

    def test_stages_provisioner_files_with_source_bytes_and_modes(self):
        output = self.root / "source-files.tar"
        result = self.run_compose(output)
        self.assertEqual(result.returncode, 0, result.stderr)
        with tarfile.open(output) as archive:
            for name, mode in (
                ("nvke-agent-spire.conf", 0o644),
                ("wait-spiffe-socket.sh", 0o755),
                ("conntrack-tune.service", 0o644),
                ("configure-conntrack.sh", 0o755),
                ("10-nke-conntrack-timeouts.conf", 0o644),
                ("99-disable-network-config.cfg", 0o644),
                ("00-dhcp-all.yaml", 0o644),
            ):
                with self.subTest(name=name):
                    self.assertEqual(archive.getmember(name).mode, mode)
                    self.assertEqual(archive.extractfile(name).read(), (self.source_files / name).read_bytes())

    def test_directory_modes_are_independent_of_umask(self):
        outputs = [self.root / "permissive.tar", self.root / "restrictive.tar"]
        for output, umask in zip(outputs, (0o022, 0o077)):
            with self.subTest(umask=umask):
                result = self.run_compose(output, umask=umask)
                self.assertEqual(result.returncode, 0, result.stderr)
                with tarfile.open(output) as archive:
                    for member in archive.getmembers():
                        if member.isdir():
                            self.assertEqual(member.mode, 0o755)
        self.assertEqual(outputs[0].read_bytes(), outputs[1].read_bytes())

    def test_index_requests_multiversion(self):
        write_executable(
            self.tools / "dpkg-scanpackages",
            "#!/bin/sh\n"
            "test \"$#\" = 3 && test \"$1\" = --multiversion || exit 1\n"
            "printf 'Package: fixture\\n'\n",
        )
        result = self.run_compose(self.root / "multiversion.tar")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_real_index_covers_multiple_versions(self):
        require_real_tools(self, "dpkg-deb", "dpkg-scanpackages")
        (self.tools / "dpkg-scanpackages").unlink()
        (self.tools / "dpkg-deb").unlink()
        for number, binding in enumerate(("agent", "calico", "kubeadm-distro", "ovn", "spire-agent", "doca-ovs")):
            name = "fixture-shared" if number in (1, 2) else {
                "spire-agent": "spire-agent",
                "doca-ovs": "doca-openvswitch-switch",
            }.get(binding, f"fixture-{binding}")
            package = self.root / f"package-{number}"
            control = package / "DEBIAN"
            control.mkdir(parents=True)
            (control / "control").write_text(
                f"Package: {name}\nVersion: {number}.0\nArchitecture: all\n"
                "Maintainer: Fixture <fixture@example.invalid>\n"
                "Description: External material adapter test\n",
                encoding="utf-8",
            )
            deb = self.root / f"{name}_{number}.0_all.deb"
            subprocess.run(
                ["dpkg-deb", "--build", "--root-owner-group", str(package), str(deb)],
                check=True, capture_output=True,
            )
            if binding == "agent":
                shutil.copyfile(deb, self.materials / binding)
            else:
                with tarfile.open(self.materials / binding, "w", format=tarfile.USTAR_FORMAT) as archive:
                    archive.add(deb, arcname=deb.name)
        output = self.root / "real-index.tar"
        result = self.run_compose(output)
        self.assertEqual(result.returncode, 0, result.stderr)
        with tarfile.open(output) as archive:
            index = archive.extractfile("debs/Packages").read().decode()
        self.assertEqual(index.count("Package: fixture-shared\n"), 2)
        for number in (1, 2):
            self.assertIn(f"Filename: ./fixture-shared_{number}.0_all.deb\n", index)

    def test_rejects_duplicate_package_names(self):
        write_package_archive(self.materials / "ovn", "calico.deb")
        result = self.run_compose(self.root / "duplicate.tar")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("duplicate Debian package filename", result.stderr)

    def test_rejects_archive_links(self):
        with tarfile.open(self.materials / "ovn", "w") as archive:
            member = tarfile.TarInfo("ovn.deb")
            member.type = tarfile.SYMTYPE
            member.linkname = "/etc/passwd"
            archive.addfile(member)
        result = self.run_compose(self.root / "unsafe.tar")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsafe or unexpected member", result.stderr)

    def test_rejects_oversized_member_before_reading_payload(self):
        with (self.materials / "ovn").open("wb") as archive:
            member = tarfile.TarInfo("large.deb")
            member.size = (1 << 30) + 1
            archive.write(member.tobuf())
        output = self.root / "oversized.tar"
        result = self.run_compose(output)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("per-package size budget", result.stderr)
        self.assertFalse(output.exists())

    def test_streaming_stage_checks_all_budgets(self):
        spec = importlib.util.spec_from_file_location("compose_image_inputs", SCRIPTS / "compose-image-inputs.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        for limit, value, total, expected in (
            ("MAX_ARCHIVE_MEMBERS", 0, 0, "member budget"),
            ("MAX_ARCHIVE_BYTES", 100, 0, "raw archive byte budget"),
            ("MAX_PACKAGES", 0, 0, "package count budget"),
            ("MAX_TOTAL_PACKAGE_BYTES", 1, 0, "total package byte budget"),
        ):
            with self.subTest(limit=limit):
                previous = getattr(module, limit)
                setattr(module, limit, value)
                stage = self.root / limit
                stage.mkdir()
                try:
                    with self.assertRaisesRegex(SystemExit, expected):
                        module.stage_archive_debs(self.materials / "calico", stage, set(), total)
                    self.assertEqual(list(stage.iterdir()), [])
                finally:
                    setattr(module, limit, previous)

    def test_agent_and_combined_byte_budgets_preserve_output(self):
        spec = importlib.util.spec_from_file_location("compose_image_inputs", SCRIPTS / "compose-image-inputs.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        agent = self.materials / "agent"
        payload = agent.read_bytes()
        archive_bytes = 0
        for binding in module.ARCHIVE_BINDINGS:
            with tarfile.open(self.materials / binding) as archive:
                archive_bytes += sum(member.size for member in archive)
        for name, agent_payload, package_limit, total_limit, expected, copies_agent in (
            ("empty agent", b"", 1024, 1024, "agent package is empty", False),
            ("oversized agent", payload, len(payload) - 1, 1024, "agent package exceeds its size budget", False),
            ("agent exceeds total", payload, 1024, len(payload) - 1, "total package byte budget", False),
            ("combined total", payload, 1024, archive_bytes, "total package byte budget", True),
        ):
            for existing in (False, True):
                with self.subTest(name=name, existing=existing):
                    agent.write_bytes(agent_payload)
                    output = self.root / "budget.tar"
                    if existing:
                        output.write_bytes(b"prior output")
                    arguments = SimpleNamespace(architecture="amd64", materials_dir=self.materials, source_files_dir=self.source_files,
                                                output=output, source_date_epoch=1787932620)
                    with patch.object(module, "parse_args", return_value=arguments), \
                            patch.object(module.shutil, "which", return_value="fixture"), \
                            patch.object(module, "require_architecture"), \
                            patch.object(module, "MAX_PACKAGE_BYTES", package_limit), \
                            patch.object(module, "MAX_TOTAL_PACKAGE_BYTES", total_limit), \
                            patch.object(module, "install_file", wraps=module.install_file) as install, \
                            patch.object(module.subprocess, "run") as index:
                        with self.assertRaisesRegex(SystemExit, expected):
                            module.main()
                        self.assertEqual(install.call_count, 8 if copies_agent else 0)
                        index.assert_not_called()
                    if existing:
                        self.assertEqual(output.read_bytes(), b"prior output")
                        output.unlink()
                    else:
                        self.assertFalse(output.exists())
                    self.assertFalse(list(self.root.glob(".image-inputs.*")))

    def test_agent_copy_budget_uses_actual_bytes_after_size_check(self):
        spec = importlib.util.spec_from_file_location("compose_image_inputs", SCRIPTS / "compose-image-inputs.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        agent = self.materials / "agent"
        initial = b"agent-deb\n"
        archive_bytes = 0
        for binding in module.ARCHIVE_BINDINGS:
            with tarfile.open(self.materials / binding) as archive:
                archive_bytes += sum(member.size for member in archive)
        for name, changed, package_limit, total_limit, expected in (
            ("grows past package limit", initial + b"more", len(initial), 1024, "byte budget"),
            ("grows past total limit", initial + b"more", 1024, len(initial), "byte budget"),
            ("grows past combined limit", initial + b"x", 1024, len(initial) + archive_bytes, "total package byte budget"),
            ("shrinks to empty", b"", 1024, 1024, "agent package is empty"),
            ("shrinks within combined limit", b"a", 1024, 1 + archive_bytes, None),
            ("grows within limits", initial + b"more", 1024, 1024, None),
        ):
            with self.subTest(name=name):
                agent.write_bytes(initial)
                output = self.root / "changed-agent.tar"
                output.write_bytes(b"prior output")
                arguments = SimpleNamespace(architecture="amd64", materials_dir=self.materials, source_files_dir=self.source_files,
                                            output=output, source_date_epoch=1787932620)
                original_install = module.install_file

                def install_after_change(source, destination, mode, **options):
                    if source == agent:
                        agent.write_bytes(changed)
                    return original_install(source, destination, mode, **options)

                def write_index(command, **options):
                    self.assertNotEqual(options["stdout"], subprocess.PIPE)
                    self.assertEqual(options["timeout"], 300)
                    options["stdout"].write(b"index\n")

                with patch.object(module, "parse_args", return_value=arguments), \
                        patch.object(module.shutil, "which", return_value="fixture"), \
                        patch.object(module, "require_architecture"), \
                        patch.object(module, "MAX_PACKAGE_BYTES", package_limit), \
                        patch.object(module, "MAX_TOTAL_PACKAGE_BYTES", total_limit), \
                        patch.object(module, "install_file", side_effect=install_after_change), \
                        patch.object(module.subprocess, "run", side_effect=write_index) as index:
                    if expected is not None:
                        with self.assertRaisesRegex(SystemExit, expected):
                            module.main()
                        index.assert_not_called()
                        self.assertEqual(output.read_bytes(), b"prior output")
                    else:
                        module.main()
                        index.assert_called_once()
                        with tarfile.open(output) as archive:
                            self.assertEqual(archive.extractfile("nvke-agent.deb").read(), changed)
                            self.assertEqual(archive.extractfile("debs/Packages").read(), b"index\n")
                self.assertFalse(list(self.root.glob(".image-inputs.*")))

    def test_install_file_bounds_each_chunk(self):
        spec = importlib.util.spec_from_file_location("compose_image_inputs", SCRIPTS / "compose-image-inputs.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        source = self.root / "large-agent"
        source.write_bytes(b"a" * (2 << 20))
        destination = self.root / "bounded-agent"
        with self.assertRaisesRegex(SystemExit, "byte budget"):
            module.install_file(source, destination, 0o644, max_bytes=(1 << 20) + 1)
        self.assertEqual(destination.stat().st_size, 1 << 20)

    def test_rejects_damaged_archive_endings(self):
        path = self.materials / "ovn"
        original = path.read_bytes()
        for name, payload in (
            ("corrupt second header", original[:1024] + b"invalid" + bytes(505) + bytes(1024)),
            ("member-boundary truncation", original[:1024]),
            ("garbage tail", original + b"garbage"),
            ("one end marker", original[:1536]),
        ):
            with self.subTest(name=name):
                path.write_bytes(payload)
                output = self.root / "corrupt.tar"
                output.write_bytes(b"prior output")
                result = self.run_compose(output)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("package archive", result.stderr)
                self.assertEqual(output.read_bytes(), b"prior output")

    def test_rejects_root_directory_with_payload(self):
        with tarfile.open(self.materials / "ovn", "w") as archive:
            first = tarfile.TarInfo("first.deb")
            first.size = 1
            archive.addfile(first, io.BytesIO(b"x"))
            root = tarfile.TarInfo(".")
            root.type = tarfile.DIRTYPE
            root.size = 512
            archive.addfile(root, io.BytesIO(b"x" * 512))
            last = tarfile.TarInfo("last.deb")
            last.size = 1
            archive.addfile(last, io.BytesIO(b"x"))
        result = self.run_compose(self.root / "root-directory.tar")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("root directory must have zero size", result.stderr)

    def test_archive_snapshot_survives_in_place_source_mutation(self):
        spec = importlib.util.spec_from_file_location("compose_image_inputs", SCRIPTS / "compose-image-inputs.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        path = self.materials / "ovn"
        with tarfile.open(path, "w") as archive:
            for name in ("first.deb", "last.deb"):
                member = tarfile.TarInfo(name)
                member.size = 1
                archive.addfile(member, io.BytesIO(b"x"))
        original_validation = module.validate_archive_framing

        def mutate_after_validation(content, original_path):
            original_validation(content, original_path)
            with original_path.open("r+b") as writable:
                writable.seek(1024)
                writable.write(b"invalid" + bytes(505) + bytes(1024))
                writable.truncate()

        stage = self.root / "snapshot-stage"
        stage.mkdir()
        names = set()
        with patch.object(module, "validate_archive_framing", side_effect=mutate_after_validation):
            total = module.stage_archive_debs(path, stage, names, 0)
        self.assertEqual(total, 2)
        self.assertEqual(names, {"first.deb", "last.deb"})
        self.assertEqual({entry.name for entry in stage.iterdir()}, names)

    def test_rejects_unsupported_members_and_names(self):
        for name, kind, size in (
            ("nested/ovn.deb", tarfile.REGTYPE, 1),
            ("ovn.deb", tarfile.LNKTYPE, 0),
            ("ovn.deb", tarfile.REGTYPE, 0),
            (".deb", tarfile.REGTYPE, 1),
            ("-rf .deb", tarfile.REGTYPE, 1),
            ("x\nPackage: evil\n.deb", tarfile.REGTYPE, 1),
            ("pax", tarfile.XHDTYPE, 200 << 20),
            ("longname", tarfile.GNUTYPE_LONGNAME, 200 << 20),
            ("sparse.deb", tarfile.GNUTYPE_SPARSE, 1),
        ):
            with self.subTest(name=name, kind=kind):
                member = tarfile.TarInfo(name)
                member.type = kind
                member.size = size
                with (self.materials / "ovn").open("wb") as archive:
                    archive.write(member.tobuf())
                    if size < 512:
                        archive.write(b"x" * size + bytes((-size) % 512) + bytes(1024))
                result = self.run_compose(self.root / "bad-member.tar")
                self.assertNotEqual(result.returncode, 0)
                expected = "is empty" if size == 0 and kind == tarfile.REGTYPE else "unsafe or unexpected member"
                self.assertIn(expected, result.stderr)

    def test_rejects_gzip_archive(self):
        import gzip
        path = self.materials / "ovn"
        path.write_bytes(gzip.compress(path.read_bytes()))
        result = self.run_compose(self.root / "gzip.tar")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("must be an uncompressed tar archive", result.stderr)

    def test_checks_package_architecture(self):
        for target in ("amd64", "arm64"):
            for architecture in ("amd64", "all", "arm64"):
                with self.subTest(target=target, architecture=architecture):
                    write_executable(self.tools / "dpkg-deb", f"#!/bin/sh\nprintf '{architecture}\\n'\n")
                    result = self.run_compose(self.root / "architecture.tar", architecture=target)
                    status = 0 if architecture in (target, "all") else 1
                    self.assertEqual(result.returncode, status, result.stderr)
                    if status:
                        self.assertIn(f"has architecture '{architecture}'", result.stderr)

    def test_rejects_multiple_calico_manifest_packages(self):
        with tarfile.open(self.materials / "calico", "w") as archive:
            for name in ("nvke-cni-calico-manifests-1.deb", "nvke-cni-calico-manifests-2.deb"):
                member = tarfile.TarInfo(name)
                member.size = 1
                archive.addfile(member, io.BytesIO(b"x"))
        result = self.run_compose(self.root / "calico.tar")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("at most one nvke-cni-calico-manifests-*.deb", result.stderr)

    def test_rejects_mixed_architecture_repository_package(self):
        write_executable(self.tools / "dpkg-deb", "#!/bin/sh\ncase \"$2\" in */ovn.deb) echo arm64;; *) echo all;; esac\n")
        result = self.run_compose(self.root / "mixed-architecture.tar")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("package 'ovn.deb' has architecture 'arm64'", result.stderr)

    def test_spire_agent_binding_cardinality(self):
        for filenames in (("other.deb",), ("spire-agent_1_all.deb", "spire-agent_2_all.deb")):
            with self.subTest(filenames=filenames):
                with tarfile.open(self.materials / "spire-agent", "w") as archive:
                    for name in filenames:
                        member = tarfile.TarInfo(name)
                        member.size = 1
                        archive.addfile(member, io.BytesIO(b"x"))
                result = self.run_compose(self.root / "spire.tar")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("exactly one spire-agent_*.deb", result.stderr)

    def test_rejects_spire_agent_in_another_binding(self):
        write_package_archive(self.materials / "ovn", "spire-agent_2.0_all.deb")
        result = self.run_compose(self.root / "ambiguous-spire.tar")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("exactly one spire-agent_*.deb across all bindings", result.stderr)

    def test_doca_openvswitch_switch_cardinality(self):
        for filenames in (("other.deb",), ("doca-openvswitch-switch_1_all.deb", "doca-openvswitch-switch_2_all.deb")):
            with self.subTest(filenames=filenames):
                with tarfile.open(self.materials / "doca-ovs", "w") as archive:
                    for name in filenames:
                        member = tarfile.TarInfo(name)
                        member.size = 1
                        archive.addfile(member, io.BytesIO(b"x"))
                result = self.run_compose(self.root / "doca.tar")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("exactly one doca-openvswitch-switch_*.deb", result.stderr)

    def test_missing_files_and_unsafe_outputs(self):
        for relative in ("materials/ovn", "materials/agent", "source-files/nvke-agent-spire.conf",
                         "source-files/wait-spiffe-socket.sh", "source-files/conntrack-tune.service",
                         "source-files/configure-conntrack.sh", "source-files/10-nke-conntrack-timeouts.conf",
                         "source-files/99-disable-network-config.cfg", "source-files/00-dhcp-all.yaml"):
            with self.subTest(relative=relative):
                path = self.root / relative
                saved = path.read_bytes()
                path.unlink()
                result = self.run_compose(self.root / "missing.tar")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("cannot", result.stderr)
                path.write_bytes(saved)
        for kind in ("directory", "symlink"):
            with self.subTest(kind=kind):
                output = self.root / kind
                if kind == "directory":
                    output.mkdir()
                else:
                    output.symlink_to(self.materials / "agent")
                result = self.run_compose(output)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("refusing unsafe output", result.stderr)

    def test_tool_preflight_and_failed_index(self):
        spec = importlib.util.spec_from_file_location("compose_image_inputs", SCRIPTS / "compose-image-inputs.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        arguments = SimpleNamespace(architecture="amd64", materials_dir=self.materials, source_files_dir=self.source_files,
                                    output=self.root / "preflight.tar", source_date_epoch=1787932620)
        with patch.object(module, "parse_args", return_value=arguments), \
                patch.object(module.shutil, "which", side_effect=lambda tool: None if tool == "dpkg-scanpackages" else "fixture"):
            with self.assertRaisesRegex(SystemExit, "dpkg-scanpackages is required"):
                module.main()
        self.assertFalse(arguments.output.exists())
        write_executable(self.tools / "dpkg-scanpackages", "#!/bin/sh\nprintf 'partial index\\n'\nexit 7\n")
        for existing in (False, True):
            with self.subTest(existing=existing):
                output = self.root / "failed-index.tar"
                if existing:
                    output.write_bytes(b"prior output")
                result = self.run_compose(output)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("dpkg-scanpackages failed with exit status 7", result.stderr)
                self.assertNotIn("Traceback", result.stderr)
                if existing:
                    self.assertEqual(output.read_bytes(), b"prior output")
                else:
                    self.assertFalse(output.exists())
                self.assertFalse(list(self.root.glob(".image-inputs.*")))

    def test_non_utf8_package_metadata_fails_without_traceback(self):
        for status in (0, 7):
            with self.subTest(status=status):
                write_executable(self.tools / "dpkg-deb",
                    f"#!/bin/sh\nprintf '\\377\\376'\nprintf '\\377\\376' >&2\nexit {status}\n")
                result = self.run_compose(self.root / "invalid-metadata.tar")
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("Traceback", result.stderr)
                self.assertIn("architecture" if status == 0 else "cannot read Debian package", result.stderr)

    def test_missing_real_tools_skip_locally_and_fail_when_required(self):
        for required in ("0", "1"):
            with self.subTest(required=required), \
                    patch.dict(os.environ, {"NKE_REQUIRE_REAL_TOOLS": required}), \
                    patch.object(shutil, "which", return_value=None):
                expected = self.failureException if required == "1" else unittest.SkipTest
                with self.assertRaisesRegex(expected, "real test tools are required: dpkg-deb"):
                    require_real_tools(self, "dpkg-deb")

    def test_regular_descriptor_rejects_swapped_symlink_and_fifo(self):
        spec = importlib.util.spec_from_file_location("compose_image_inputs", SCRIPTS / "compose-image-inputs.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        for kind in ("symlink", "fifo"):
            with self.subTest(kind=kind):
                source = self.root / kind
                if kind == "symlink":
                    source.symlink_to(self.materials / "agent")
                else:
                    os.mkfifo(source)
                with self.assertRaises(SystemExit):
                    module.install_file(source, self.root / f"{kind}-output", 0o644)


class PatchInstallerISOTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.source = self.root / "source.iso"
        self.source.write_bytes(b"source-iso\n")
        write_executable(
            self.tools / "7z",
            """#!/bin/sh
set -eu
for argument in "$@"; do
  case "$argument" in -o*) output=${argument#-o};; esac
done
mkdir -p "$output"
printf 'set timeout=30\nmenuentry fixture {}\n' >"$output/grub.cfg"
""",
        )
        write_executable(
            self.tools / "xorriso",
            """#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
  case "$1" in
    -indev) input=$2; shift 2;;
    -outdev) output=$2; shift 2;;
    -update) patched=$2; shift 3;;
    *) shift;;
  esac
done
cp "$input" "$output"
cat "$patched" >>"$output"
""",
        )

    def tearDown(self):
        self.temporary.cleanup()

    def run_patch(self, source: Path, output: Path):
        environment = os.environ.copy()
        environment["PATH"] = f"{self.tools}:{environment['PATH']}"
        return subprocess.run(
            [str(SCRIPTS / "patch-installer-iso.sh"), str(source), str(output)],
            env=environment,
            cwd=self.root,
            check=False,
            capture_output=True,
            text=True,
        )

    def test_patches_caller_supplied_iso(self):
        output = self.root / "output" / "installer.iso"
        result = self.run_patch(self.source, output)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output.read_bytes(), b"source-iso\nset timeout=300\nmenuentry fixture {}\n")

    def test_make_iso_preparation_delegates_with_isolated_python_override(self):
        calls = self.root / "python-calls"
        python = self.tools / "selected-python"
        write_executable(python, f"#!/bin/sh\nprintf '%s\\n' \"$*\" >> '{calls}'\nexec '{sys.executable}' \"$@\"\n")
        write_executable(self.tools / "python3", "#!/bin/sh\nexit 99\n")
        (self.root / "shutil.py").write_text("raise RuntimeError('shadowed stdlib')\n", encoding="utf-8")
        digest = hashlib.sha256(self.source.read_bytes()).hexdigest()
        write_executable(self.tools / "curl", f"#!/bin/sh\nprintf '{digest}  source.iso\\n'\n")
        environment = os.environ.copy()
        environment.update({"PATH": f"{self.tools}:{environment['PATH']}", "PYTHON": str(python),
                            "PYTHONPATH": str(self.root), "ISO_URL": "https://example.invalid/source.iso",
                            "ISO_CHECKSUM": "file:https://example.invalid/SHA256SUMS", "OUT_DIR": str(self.root)})
        result = subprocess.run([str(SCRIPTS / "prepare-installer-iso.sh")], env=environment,
                                cwd=self.root, capture_output=True, text=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root / "source.iso.patched.iso").read_bytes(),
                         b"source-iso\nset timeout=300\nmenuentry fixture {}\n")
        commands = calls.read_text().splitlines()
        self.assertEqual(len(commands), 2)
        self.assertTrue(all(command.startswith("-I - ") for command in commands), commands)

    def test_rejects_missing_or_multiple_grub_timeouts(self):
        for content, count in (("menuentry fixture {}\n", 0), ("set timeout=30\nset timeout=5\n", 2)):
            with self.subTest(count=count):
                write_executable(self.tools / "7z", "#!/bin/sh\n"
                    "for arg in \"$@\"; do case \"$arg\" in -o*) output=${arg#-o};; esac; done\n"
                    f"printf '{content}' >\"$output/grub.cfg\"\n")
                output = self.root / "bad-grub.iso"
                output.write_bytes(b"prior output")
                result = self.run_patch(self.source, output)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(f"expected one GRUB timeout, found {count}", result.stderr)
                self.assertEqual(output.read_bytes(), b"prior output")

    def test_rejects_unsafe_outputs(self):
        for kind in ("directory", "symlink"):
            with self.subTest(kind=kind):
                output = self.root / kind
                if kind == "directory":
                    output.mkdir()
                else:
                    output.symlink_to(self.source)
                result = self.run_patch(self.source, output)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("refusing unsafe output", result.stderr)

    def test_termination_does_not_publish_output(self):
        write_executable(self.tools / "xorriso", "#!/bin/sh\nkill -TERM \"$PPID\"\n")
        output = self.root / "terminated.iso"
        output.write_bytes(b"prior output")
        result = self.run_patch(self.source, output)
        self.assertEqual(result.returncode, 143, result.stderr)
        self.assertEqual(output.read_bytes(), b"prior output")
        self.assertFalse(list(self.root.glob(".installer.*")))

    def test_snapshot_is_shared_and_directory_swap_is_rejected(self):
        output = self.root / "swap.iso"
        write_executable(self.tools / "xorriso", "#!/bin/sh\nset -eu\n"
            "while [ \"$#\" -gt 0 ]; do case \"$1\" in -indev) input=$2; shift 2;; "
            "-outdev) output=$2; shift 2;; *) shift;; esac; done\n"
            f"printf 'changed source' > '{self.source}'\n"
            "cp \"$input\" \"$output\"\n"
            f"mkdir '{output}'\n")
        result = self.run_patch(self.source, output)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(list(output.iterdir()), [])

    def test_snapshot_survives_source_change_before_successful_publication(self):
        seven_zip_input = self.root / "seven-zip-input"
        xorriso_input = self.root / "xorriso-input"
        write_executable(self.tools / "7z", "#!/bin/sh\nset -eu\n"
            "for argument in \"$@\"; do case \"$argument\" in "
            "-o*) output=${argument#-o};; "
            f"*.iso) printf '%s' \"$argument\" > '{seven_zip_input}';; esac; done\n"
            "printf 'set timeout=30\\nmenuentry fixture {}\\n' >\"$output/grub.cfg\"\n")
        write_executable(self.tools / "xorriso", "#!/bin/sh\nset -eu\n"
            "while [ \"$#\" -gt 0 ]; do case \"$1\" in -indev) input=$2; shift 2;; "
            "-outdev) output=$2; shift 2;; *) shift;; esac; done\n"
            f"printf '%s' \"$input\" > '{xorriso_input}'\n"
            f"printf 'changed source' > '{self.source}'\n"
            "cp \"$input\" \"$output\"\n")
        output = self.root / "snapshot.iso"
        result = self.run_patch(self.source, output)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output.read_bytes(), b"source-iso\n")
        self.assertEqual(self.source.read_bytes(), b"changed source")
        self.assertEqual(seven_zip_input.read_text(), xorriso_input.read_text())
        self.assertNotEqual(seven_zip_input.read_text(), str(self.source))

    def test_rejects_source_symlink(self):
        source_link = self.root / "source-link.iso"
        source_link.symlink_to(self.source)
        result = self.run_patch(source_link, self.root / "installer.iso")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("source is not one regular file", result.stderr)

    def test_real_iso_grub_transform(self):
        require_real_tools(self, "7z", "xorriso")
        (self.tools / "7z").unlink()
        (self.tools / "xorriso").unlink()
        source_tree = self.root / "iso-tree"
        grub = source_tree / "boot" / "grub"
        grub.mkdir(parents=True)
        (grub / "grub.cfg").write_text("set timeout=30\nmenuentry fixture {}\n", encoding="utf-8")
        (grub / "bios.img").write_bytes(bytes(2048))
        (grub / "efi.img").write_bytes(bytes(2048))
        source = self.root / "real-source.iso"
        subprocess.run(
            ["xorriso", "-as", "mkisofs", "-o", str(source),
             "-b", "boot/grub/bios.img", "-no-emul-boot", "-boot-load-size", "4",
             "-eltorito-alt-boot", "-e", "boot/grub/efi.img", "-no-emul-boot", str(source_tree)],
            check=True, capture_output=True, timeout=30,
        )
        output = self.root / "real-patched.iso"
        result = self.run_patch(source, output)
        self.assertEqual(result.returncode, 0, result.stderr)
        extracted = subprocess.run(
            ["7z", "e", "-so", str(output), "boot/grub/grub.cfg"],
            check=True, capture_output=True, timeout=30,
        ).stdout
        self.assertEqual(extracted, b"set timeout=300\nmenuentry fixture {}\n")
        catalogs = []
        for image in (source, output):
            report = subprocess.run(
                ["xorriso", "-indev", str(image), "-report_el_torito", "plain"],
                check=True, capture_output=True, text=True, timeout=30,
            ).stdout
            catalogs.append([line.split()[:-1] for line in report.splitlines() if line.startswith("El Torito boot img")])
        self.assertEqual(catalogs[0], catalogs[1])
        self.assertEqual(len(catalogs[0]), 2)


if __name__ == "__main__":
    unittest.main()
