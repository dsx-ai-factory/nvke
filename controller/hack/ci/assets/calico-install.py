#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

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
