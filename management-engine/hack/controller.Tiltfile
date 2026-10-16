# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Loaded by the controller's local tilt.d hook, after its own resources are registered.
load_dynamic("../Tiltfile")

local_resource(
    "management-engine-credentials",
    cmd=["bash", "hack/dev.sh", "controller-credentials"],
    dir="..",
    deps=["dev.sh", "controller.sh"],
    resource_deps=["cwe", "management-state"],
    labels=["management-engine"],
)
k8s_resource("management-engine", resource_deps=["management-state", "management-engine-credentials"], labels=["management-engine"])

local_resource(
    "management-engine-integration-test",
    cmd=["make", "test-nvke-controller-integration", "DEV_NAME=" + os.getenv("DEV_NAME"), "TILT_PORT=" + os.getenv("TILT_PORT")],
    dir="..",
    resource_deps=["management-engine"],
    trigger_mode=TRIGGER_MODE_MANUAL,
    auto_init=False,
    labels=["tests"],
)
