#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Helm post-renderer: enable the existing simulated-device backend on the DPU."""
import sys

import yaml

documents = [doc for doc in yaml.safe_load_all(sys.stdin) if doc]
matched = 0
for doc in documents:
    if not doc or doc.get("kind") != "DaemonSet":
        continue
    if doc["metadata"]["name"] != "ovnkube-node-dpu":
        continue
    spec = doc["spec"]["template"]["spec"]
    controller = next(c for c in spec["containers"] if c["name"] == "ovnkube-controller")
    env = controller.setdefault("env", [])
    env[:] = [entry for entry in env if entry["name"] != "OVN_SIMULATE_DPU"]
    env.append({"name": "OVN_SIMULATE_DPU", "value": "true"})
    matched += 1
if matched != 1:
    raise SystemExit(f"Expected one VMaaS DPU DaemonSet, found {matched}")
yaml.safe_dump_all(documents, sys.stdout, sort_keys=False)
