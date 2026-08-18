#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import os
import pathlib
import subprocess
import tempfile
import time


root = pathlib.Path(__file__).resolve().parent
manifests = root / "manifests"


def run(*args: str) -> None:
    subprocess.run(args, check=True)


run(
    "kubectl",
    "apply",
    "--server-side",
    "-f",
    str(manifests / "operator-crds.yaml"),
)
run(
    "kubectl",
    "wait",
    "--for=condition=Established",
    "crd/installations.operator.tigera.io",
    "--timeout=120s",
)
time.sleep(2)
run("kubectl", "apply", "-f", str(manifests / "tigera-operator.yaml"))
run(
    "kubectl",
    "rollout",
    "status",
    "deployment/tigera-operator",
    "--namespace=tigera-operator",
    "--timeout=180s",
)
custom_resources = manifests / "custom-resources.yaml"
rendered_custom_resources = None
pod_network_cidr = os.environ.get("CALICO_POD_CIDR", "").strip()
if pod_network_cidr:
    default_cidr = "cidr: 192.168.0.0/16"
    manifest = custom_resources.read_text(encoding="utf-8")
    if default_cidr not in manifest:
        raise RuntimeError(
            f"cannot apply pod network CIDR {pod_network_cidr!r}: "
            f"default pool line {default_cidr!r} is missing from {custom_resources}"
        )
    with tempfile.NamedTemporaryFile(
        mode="w",
        encoding="utf-8",
        prefix="nke-calico-custom-resources-",
        suffix=".yaml",
        delete=False,
    ) as rendered:
        rendered.write(manifest.replace(default_cidr, f"cidr: {pod_network_cidr}"))
        rendered_custom_resources = pathlib.Path(rendered.name)
    custom_resources = rendered_custom_resources

try:
    run("kubectl", "apply", "-f", str(custom_resources))
finally:
    if rendered_custom_resources is not None:
        rendered_custom_resources.unlink(missing_ok=True)
