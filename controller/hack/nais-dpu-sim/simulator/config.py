# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Repository paths and pinned nvke-nais-dpu-sim inputs."""
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
REPO = ROOT.parents[2]
STATE = ROOT / ".state"
VALUES = ROOT / "charts"
INPUTS = json.loads((ROOT / "inputs.json").read_text())
IMAGE = INPUTS["ovn"]["image"]
CHART = STATE / "build/bundle/charts/ovn-kubernetes"
NODE_IMAGE = "nvke-nais-dpu-sim-node:local"
RUNTIME_IMAGE = "nvke-nais-dpu-sim-runtime:local"
# The privileged driver supplies host namespace tools, not a Kubernetes node.
DRIVER_IMAGE = "kindest/node:v1.34.8"
PROVIDER_IMAGE = "cluster-api-provider-nico:local"
TEMPORAL_IMAGE = "temporalio/temporal:1.9.1"
POSTGRES_IMAGE = "postgres:17.4"
PLUGIN_IMAGE = INPUTS["device_plugin"]["image"]
FRR_CONTROLLER = INPUTS["frr_k8s"]["image"]
FRR_IMAGE = INPUTS["frr"]["image"]


WORKLOAD_ROLE = "workload"
