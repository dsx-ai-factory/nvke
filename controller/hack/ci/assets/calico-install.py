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

import pathlib
import subprocess
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
run("kubectl", "apply", "-f", str(manifests / "custom-resources.yaml"))
