#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Shared command helpers and offline VMaaS chart checks for the managed lab."""
import argparse
import hashlib
from pathlib import Path
import subprocess
import sys
import tarfile

import yaml

ROOT = Path(__file__).resolve().parent
STATE = ROOT / ".state"
CHART_SHA256 = "470e8255ce3f4a7362b84471d4d183757f6b679a94ea882500feac5a6abd75f2"
IMAGE = "nvcr.io/fcypcg1knhby/vmaas-dev/ovnkube:a8b97898-69369865-2b600cc"


def command(*args, **kwargs):
    result = subprocess.run(args, text=True, capture_output=True, **kwargs)
    if result.returncode:
        raise RuntimeError(f"{args[0]} failed ({result.returncode}): {result.stderr.strip()} {result.stdout.strip()}")
    return result.stdout.strip()


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def chart_check(archive):
    require(hashlib.sha256(archive.read_bytes()).hexdigest() == CHART_SHA256,
            "Chart checksum differs from the qualified VMaaS 1.5.18 archive")
    cache = STATE / "chart"
    cache.mkdir(parents=True, exist_ok=True)
    with tarfile.open(archive) as chart:
        chart.extractall(cache, filter="data")
    base = cache / "ovn-kubernetes"
    for side in ("host", "dpu", "primary"):
        profile = "values-vmaas-nooverlay-dpu-host.yaml" if side == "host" else "values-vmaas-nooverlay-dpu.yaml"
        values = [str(base / profile), str(ROOT / f"values-{side}-sim.yaml")]
        if side == "primary":
            profile = "values-dpu-primary.yaml"
            values = [str(ROOT / profile)]
        args = ["helm", "template", "ovn-kubernetes", str(base), "--namespace", "ovn-kubernetes",
                "-f", str(base / "values-images.yaml")]
        for path in values:
            args.extend(["-f", path])
        if side == "primary":
            args.extend(["--set", "k8sAPIServer=https://192.0.2.11:6443", "--set", "podNetwork=10.236.64.0/18/24",
                         "--set", "serviceNetwork=10.236.0.0/18"])
        output = command(*args)
        if side == "dpu":
            # Helm 4 resolves --post-renderer as a plugin, unlike Helm 3's executable.
            # Run the same renderer explicitly for these offline checks.
            output = command(sys.executable, str(ROOT / "simulate-dpu.py"), input=output)
        (STATE / f"rendered-{side}.yaml").write_text(output + "\n")
        docs = [d for d in yaml.safe_load_all(output) if d]
        daemonsets = {d["metadata"]["name"]: d for d in docs if d["kind"] == "DaemonSet"}
        expected = {"host": "ovnkube-node-dpu-host", "dpu": "ovnkube-node-dpu", "primary": "ovnkube-node"}[side]
        require(set(daemonsets) == {expected}, f"Unexpected {side} DaemonSets: {list(daemonsets)}")
        spec = daemonsets[expected]["spec"]["template"]["spec"]
        containers = spec.get("initContainers", []) + spec["containers"]
        for container in containers:
            require(container["image"] == IMAGE, f"Unexpected image in {side}/{container['name']}")
        envs = [{e["name"]: e.get("value") for e in c.get("env", [])} for c in spec["containers"]]
        if side != "primary":
            require(any(e.get("OVN_SIMULATE_DPU") == "true" for e in envs), f"{side} missing simulation flag")
        require(spec.get("hostNetwork"), f"{side} OVN control components require host networking")
        print(f"PASS {profile}: component placement and pinned NVIDIA image references", flush=True)
    print("Chart checks passed. This is not an OVN traffic test.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    chart = commands.add_parser("chart-check")
    chart.add_argument("--chart", required=True, type=Path)
    args = parser.parse_args()
    STATE.mkdir(exist_ok=True)
    chart_check(args.chart)


if __name__ == "__main__":
    main()
