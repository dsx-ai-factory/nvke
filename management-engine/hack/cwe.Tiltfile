# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Loaded by CWE's local tilt.d hook, after its own resources are registered.
load_dynamic("../Tiltfile")

local_resource(
    "cwe-credentials",
    cmd=["bash", "hack/dev.sh", "cwe-credentials"],
    dir="..",
    deps=["dev.sh", "cwe.sh"],
    resource_deps=["cwe", "management-state"],
    labels=["management-engine"],
)
k8s_resource("management-engine", resource_deps=["management-state", "cwe-credentials"], labels=["management-engine"])

local_resource(
    "cwe-integration-test",
    cmd=["make", "test-cwe", "DEV_NAME=" + os.getenv("DEV_NAME"), "TILT_PORT=" + os.getenv("TILT_PORT")],
    dir="..",
    resource_deps=["management-engine"],
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    labels=["tests"],
)
