#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Prepare pinned simulation inputs and build NKE-managed simulation images."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import urllib.request

import yaml

from lab import ROOT, STATE, IMAGE, chart_check, command, require
from networking import PLUGIN_IMAGE, SIM_IMAGE, MULTUS_IMAGE, FRR_CONTROLLER, FRR_IMAGE
from managed import NODE_IMAGE, RUNTIME_IMAGE, PROVIDER_IMAGE, POSTGRES_IMAGE, TEMPORAL_IMAGE

OVN_COMMIT = "a8b978986e8903f883a006808074bde83596139c"
PLUGIN_COMMIT = "a99c4111eb1b3db7241f666662906f2cc8b0e42a"
FRR_COMMIT = "b43efcb206be36a361401a233908b74b8e8b077e"
PACKAGE_SHA256 = "d660cec5f0817d56bb9bb51aeac87612b0457410fbd46fed76bc51f18be6ba3f"
MULTUS_SHA256 = "5ae5d2b0d63e0f058ab42dcd0a611479a271d850899c1e8369999901943f56cc"


def download(url, path):
    if not path.exists():
        with urllib.request.urlopen(url, timeout=120) as response, path.open("wb") as output:
            shutil.copyfileobj(response, output)


def extract(archive, target):
    if target.exists():
        return
    target.mkdir(parents=True)
    with tarfile.open(archive) as bundle:
        members = bundle.getmembers()
        prefix = members[0].name.split("/")[0] + "/"
        for member in members:
            if member.name.startswith(prefix):
                member.name = member.name[len(prefix):]
                if member.name:
                    bundle.extract(member, target, filter="data")


def prepare_frr():
    archive = STATE / "frr-k8s.tar.gz"
    download(f"https://codeload.github.com/metallb/frr-k8s/tar.gz/{FRR_COMMIT}", archive)
    source = STATE / "frr-k8s"
    extract(archive, source)
    stage = STATE / "frr-build"
    stage.mkdir(exist_ok=True)
    env = {**os.environ, "GOWORK": "off", "GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0"}
    binaries = {"frr-k8s": "cmd/frr-k8s-controller/main.go", "frr-metrics": "cmd/metrics/exporter.go",
                "frr-status": "cmd/status/exporter.go", "statuscleaner": "cmd/statuscleaner/main.go"}
    for name, entry in binaries.items():
        command("go", "build", "-o", str(stage / name), "-ldflags",
                "-X github.com/metallb/frr-k8s/internal/version.gitCommit=" + FRR_COMMIT, entry, cwd=source, env=env)
    shutil.copy2(source / "cmd/reloader/frr-reloader.sh", stage / "frr-reloader.sh")
    shutil.copy2(source / "LICENSE", stage / "LICENSE")
    (stage / "Dockerfile").write_text('FROM docker:29.1.3-cli\nRUN apk add --no-cache iproute2\nCOPY frr-k8s frr-metrics frr-status statuscleaner frr-reloader.sh LICENSE /\n')
    command("docker", "build", "-t", FRR_CONTROLLER, str(stage))


def load_package(package):
    require(hashlib.sha256(package.read_bytes()).hexdigest() == PACKAGE_SHA256,
            "Unexpected arm64 NVIDIA OVN image package checksum")
    # Parse ar directly: macOS ar does not handle this package's member names.
    with package.open("rb") as source:
        require(source.read(8) == b"!<arch>\n", "Not a Debian ar archive")
        while header := source.read(60):
            size = int(header[48:58])
            name = header[:16].decode().strip().rstrip("/")
            data = source.read(size)
            if size % 2:
                source.read(1)
            if name.startswith("data.tar"):
                payload = STATE / name
                payload.write_bytes(data)
                with tarfile.open(payload) as bundle:
                    bundle.extractall(STATE / "image-package", filter="data")
                break
    images = list((STATE / "image-package").rglob("*docker.tar"))
    require(len(images) == 1, "Expected one NVIDIA OVN image archive")
    command("docker", "load", "-i", str(images[0]))
    command("docker", "tag", "nvcr.io/nv-ngn/sdn/ovnkube:a8b97898-69369865-2b600cc", IMAGE)


def prepare_managed(repo, chart, node_only=False):
    chart_check(chart)
    stage = STATE / "managed-build"
    stage.mkdir(mode=0o700, exist_ok=True)
    node = stage / "node"
    runtime = stage / "runtime"
    node.mkdir(exist_ok=True)
    runtime.mkdir(exist_ok=True)
    for image in [IMAGE, SIM_IMAGE, PLUGIN_IMAGE, FRR_IMAGE, FRR_CONTROLLER, "cwe-capd-kindest-node:local", "docker:29.1.3-cli"]:
        command("docker", "image", "inspect", image)
    if not node_only:
        # CAPNICo is an input from its owning repository, not another lab build.
        command("docker", "image", "inspect", PROVIDER_IMAGE)
        for image in [POSTGRES_IMAGE, TEMPORAL_IMAGE]:
            command("docker", "pull", image)
    # Docker save needs named tags so containerd can import the helper images.
    command("docker", "tag", FRR_IMAGE, "quay.io/frrouting/frr:10.6.0")
    print("Building the NKE agent through make build-arm64", flush=True)
    command("make", "-C", str(repo / "agent"), "build-arm64", "GOPROXY=off")
    shutil.copy2(repo / "agent/bin/linux_arm64/nke-agent", node / "nke-agent")
    package = node / "package"
    charts = package / "etc/dgx/ovn-kubernetes"
    if charts.exists():
        shutil.rmtree(charts)
    shutil.copytree(STATE / "chart/ovn-kubernetes", charts)
    values = yaml.safe_load((ROOT / "values-host-sim.yaml").read_text())
    values.pop("k8sAPIServer", None)
    values["global"].update(imagePullSecretName="", skipCallToK8s=True, simulateDpu=True)
    values["global"]["image"] = {"repository": SIM_IMAGE.split(":")[0]}
    (charts / "values-nke.yaml").write_text(yaml.safe_dump(values))
    shutil.copy2(ROOT / "values-dpu-primary.yaml", charts / "values-dpu-primary.yaml")
    images = yaml.safe_load((charts / "values-images.yaml").read_text())
    def replace_images(value):
        if isinstance(value, dict):
            if value.get("repository", "").endswith("/ovnkube"):
                value.update(repository=SIM_IMAGE.split(":")[0], tag=SIM_IMAGE.split(":")[1])
            for child in value.values():
                replace_images(child)
        elif isinstance(value, list):
            for child in value:
                replace_images(child)
    replace_images(images)
    (charts / "values-images.yaml").write_text(yaml.safe_dump(images))
    # The released chart does not expose the gateway VF option in the host
    # daemon environment. This fixture-only adaptation matches networking.py.
    patched = False
    for path in charts.rglob("*.yaml"):
        text = path.read_text()
        if 'name: OVNKUBE_LOGLEVEL' not in text or 'ovnkube-node-dpu-host' not in text:
            continue
        lines = text.splitlines()
        for index, line in enumerate(lines):
            if "- name: OVNKUBE_LOGLEVEL" in line:
                prefix = line[:len(line)-len(line.lstrip())]
                lines[index:index] = [prefix+'- name: OVN_GATEWAY_OPTS', prefix+'  value: "--gateway-interface=eth0-0"']
                patched = True
                break
        path.write_text("\n".join(lines)+"\n")
    require(patched, "Cannot locate the DPU-host chart environment for the simulation adaptation")
    control = package / "DEBIAN"
    control.mkdir(exist_ok=True)
    (control / "control").write_text("Package: nke-cni-ovn-charts-1.5.18\nVersion: 1.5.18\nArchitecture: all\nMaintainer: NKE lab\nDescription: Qualified VMaaS OVN chart and simulation overrides for local NKE provisioning\n")
    command("docker", "save", "-o", str(node / "images.tar"), IMAGE, SIM_IMAGE, PLUGIN_IMAGE,
            FRR_CONTROLLER, "quay.io/frrouting/frr:10.6.0")
    preload = """#!/bin/sh
set -eu
ctr -n k8s.io images import --no-unpack /var/lib/nke-dpu-lab/images.tar
ctr -n k8s.io images tag --force quay.io/frrouting/frr:10.6.0 '"""+FRR_IMAGE+"""'
"""
    (node / "preload.sh").write_text(preload)
    (node / "preload.service").write_text("[Unit]\nDescription=Preload local NKE DPU simulation images\nRequires=containerd.service\nAfter=containerd.service\nBefore=nke-agent.service\n[Service]\nType=oneshot\nExecStart=/usr/local/bin/nke-dpu-preload\nRemainAfterExit=yes\nTimeoutStartSec=600\n")
    print("Building the managed node image with pinned simulation fixtures", flush=True)
    command("docker", "build", "-f", str(ROOT / "Dockerfile.managed-node"), "-t", NODE_IMAGE, str(node))
    if node_only:
        print("Managed node image rebuilt with primary and DPU-host OVN profiles", flush=True)
        return
    print("Building the NKE controller through make build", flush=True)
    command("make", "-C", str(repo / "controller"), "build", "GOOS=linux", "GOARCH=arm64", "GOPROXY=off")
    shutil.copy2(repo / "controller/bin/controller", runtime / "controller")
    # Match the NICo E2E lane's static CGO build for SQLite. The Linux
    # toolchain container uses the committed workspace vendor tree.
    print("Building nico-sim through its E2E make build-static target", flush=True)
    command("docker", "run", "--rm", "-v", str(repo) + ":/src", "-w", "/src/controller/nico-sim",
            "golang:1.26.8-alpine", "sh", "-ec", "apk add --no-cache gcc musl-dev make; "
            "GOPROXY=off make build-static "
            "BINARY=/src/controller/hack/nvke-accelerated-nais-sim/.state/managed-build/runtime/nico-sim")
    for name, source in [("LICENSE", repo / "LICENSE"), ("NOTICE", repo / "NOTICE"),
                         ("THIRD_PARTY_NOTICES.md", repo / "controller/THIRD_PARTY_NOTICES.md")]:
        shutil.copy2(source, runtime / name)
    command("docker", "build", "-f", str(ROOT / "Dockerfile.managed-runtime"), "-t", RUNTIME_IMAGE, str(runtime))
    report = {image: json.loads(command("docker", "image", "inspect", image))[0]["Id"]
              for image in [NODE_IMAGE, RUNTIME_IMAGE, PROVIDER_IMAGE, IMAGE, SIM_IMAGE, PLUGIN_IMAGE, FRR_IMAGE, FRR_CONTROLLER, POSTGRES_IMAGE, TEMPORAL_IMAGE]}
    (stage / "images.json").write_text(json.dumps(report, indent=2))
    print("Managed node/runtime images built; image identities recorded in " + str(stage / "images.json"), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chart", required=True, type=Path)
    parser.add_argument("--image-package", type=Path)
    parser.add_argument("--managed", action="store_true", help="Build the NKE-managed node and runtime images from local source")
    parser.add_argument("--node-only", action="store_true", help="Rebuild only the managed node image from prepared simulation inputs")
    parser.add_argument("--docker-context", default="colima")
    args = parser.parse_args()
    os.environ["DOCKER_CONTEXT"] = args.docker_context
    os.environ.pop("DOCKER_HOST", None)
    require(not args.node_only or args.managed, "--node-only requires --managed")
    STATE.mkdir(mode=0o700, exist_ok=True)
    STATE.chmod(0o700)
    arch = command("docker", "info", "--format", "{{.Architecture}}")
    require(arch in ("aarch64", "arm64"), "This package lock currently qualifies Linux arm64 only")
    if args.managed:
        prepare_managed(ROOT.parents[2], args.chart.resolve(), args.node_only)
        return
    chart_check(args.chart.resolve())
    if subprocess.run(["docker", "image", "inspect", IMAGE], capture_output=True).returncode:
        package = args.image_package or STATE / "ovn-images-1.5.18-arm64.deb"
        if not package.exists():
            with package.open("wb") as output:
                subprocess.run(["glab", "api", "--hostname", "gitlab-master.nvidia.com",
                                "projects/321312/jobs/459788815/artifacts/release-assets/nke-cni-ovn-images-1.5.18_1.5.18_arm64.deb"],
                               stdout=output, check=True)
        load_package(package)
    require(command("docker", "image", "inspect", IMAGE, "--format", "{{.Id}}") ==
            "sha256:48ea10ad60ebc3930e0f11355ee5cc64c2d93f4307879436947763948b4e21f3",
            "The local VMaaS base tag does not identify the pinned arm64 image")
    archive = STATE / "nvidia-ovn-source.tar.gz"
    if not archive.exists():
        with archive.open("wb") as output:
            subprocess.run(["glab", "api", "--hostname", "gitlab-master.nvidia.com",
                            f"projects/2649/repository/archive.tar.gz?sha={OVN_COMMIT}"], stdout=output, check=True)
    source = STATE / "nvidia-ovn"
    extract(archive, source)
    patch = (ROOT / "patches/nvidia-ovn-simulation.patch").read_text()
    check = subprocess.run(["patch", "--batch", "--forward", "--dry-run", "-p1"], cwd=source, input=patch, text=True, capture_output=True)
    if check.returncode == 0:
        command("patch", "--batch", "--forward", "-p1", cwd=source, input=patch)
    command("patch", "--batch", "--forward", "--dry-run", "-R", "-p1", cwd=source, input=patch)
    env = {**os.environ, "GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0", "GOWORK": "off",
           "GIT_COMMIT": OVN_COMMIT, "GIT_BRANCH": "nke-dpu-simulation"}
    subprocess.run(["make", "build"], cwd=source / "go-controller", env=env, check=True)
    context = STATE / "simulation-image"
    context.mkdir(exist_ok=True)
    for binary in ("ovnkube", "ovn-k8s-cni-overlay"):
        shutil.copy2(source / "go-controller/_output/go/bin" / binary, context / binary)
    subprocess.run(["docker", "build", "-f", str(ROOT / "Dockerfile.ovn-sim"), "-t", SIM_IMAGE, str(context)], check=True)
    plugin_archive = STATE / "device-plugin.tar.gz"
    download(f"https://codeload.github.com/ovn-kubernetes/dpu-simulator/tar.gz/{PLUGIN_COMMIT}", plugin_archive)
    plugin = STATE / "device-plugin/source"
    extract(plugin_archive, plugin)
    plugin_context = plugin.parent
    subprocess.run(["go", "build", "-o", str(plugin_context / "dpu-sim-dp"), "./cmd/dpu-sim-dp"],
                   cwd=plugin, env=env, check=True)
    (plugin_context / "Dockerfile").write_text('FROM scratch\nCOPY dpu-sim-dp /dpu-sim-dp\nENTRYPOINT ["/dpu-sim-dp"]\n')
    subprocess.run(["docker", "build", "-t", PLUGIN_IMAGE, str(plugin_context)], check=True)
    prepare_frr()
    raw_multus = STATE / "multus-original.yaml"
    download("https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/v4.2.2/deployments/multus-daemonset-thick.yml", raw_multus)
    require(hashlib.sha256(raw_multus.read_bytes()).hexdigest() == MULTUS_SHA256, "Unexpected Multus manifest checksum")
    docs = list(yaml.safe_load_all(raw_multus.read_text()))
    for doc in docs:
        if doc and doc["kind"] == "DaemonSet":
            spec = doc["spec"]["template"]["spec"]
            for container in spec.get("initContainers", []) + spec["containers"]:
                container["image"] = MULTUS_IMAGE
                container["imagePullPolicy"] = "IfNotPresent"
    (STATE / "multus.yaml").write_text(yaml.safe_dump_all(docs))
    multus_archive = STATE / "multus-oci.tar"
    if not multus_archive.exists():
        subprocess.run(["skopeo", "copy", "--preserve-digests", "--override-os", "linux", "--override-arch", "arm64",
                        "docker://" + MULTUS_IMAGE, "oci-archive:" + str(multus_archive) + ":multus"], check=True)
    print("Pinned VMaaS inputs and simulation images prepared", flush=True)


if __name__ == "__main__":
    main()
