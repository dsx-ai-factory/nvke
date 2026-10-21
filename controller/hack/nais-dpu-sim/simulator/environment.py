# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Prepare run state, Docker networks, NICo, and the device-wiring container."""
import json
import os
import uuid

from .config import (
    REPO, STATE, INPUTS, IMAGE, NODE_IMAGE, RUNTIME_IMAGE, DRIVER_IMAGE,
    PROVIDER_IMAGE, TEMPORAL_IMAGE, POSTGRES_IMAGE, PLUGIN_IMAGE,
)
from .charts import chart_check
from .runtime import Simulation, command, require, wait_until, free_port


def prepare(provider_chart, docker_context):
    """Prepare the local environment before provisioning Kubernetes clusters."""
    os.environ["DOCKER_CONTEXT"] = docker_context
    os.environ.pop("DOCKER_HOST", None)
    check_build_inputs(provider_chart)
    sim = initialize_run(docker_context)
    create_networks(sim)
    start_nico_simulator(sim)
    start_device_driver(sim)
    print(f"nvke-nais-dpu-sim state: {sim.state}", flush=True)
    return sim


def check_build_inputs(provider_chart):
    """Reject missing charts or images before allocating run resources."""
    chart_check()
    command("helm", "template", "capi-provider-nico", str(provider_chart), "--namespace", "capnico-system",
            "--set-string", "manager.image.repository=cluster-api-provider-nico", "--set-string", "manager.image.tag=local")
    for image in [NODE_IMAGE, RUNTIME_IMAGE, DRIVER_IMAGE, PROVIDER_IMAGE, TEMPORAL_IMAGE, POSTGRES_IMAGE, PLUGIN_IMAGE]:
        command("docker", "image", "inspect", image)
    require(command("docker", "image", "inspect", IMAGE, "--format", "{{.Id}}") == INPUTS["ovn"]["config_digest"],
            "The OVN tag does not identify the pinned upstream arm64 image")


def initialize_run(docker_context):
    """Give the run its own identity, private state directory, and NICo seed."""
    run_id = uuid.uuid4().hex[:8]
    name = "nvke-nais-dpu-sim-" + run_id
    state = STATE / ("managed-" + run_id)
    state.mkdir(mode=0o700)
    ids = {str(i): f"{run_id}-0000-0000-0000-{i:012d}" for i in range(1, 6)}
    seed = (REPO / "controller/nico-sim/internal/seed/default-seed.yaml").read_text()
    seed = seed.replace("00000000-0000-0000-0000-", f"{run_id}-0000-0000-0000-")
    (state / "seed.yaml").write_text(seed)
    port = free_port()
    config = {"name": name, "run_id": run_id, "ids": ids, "docker_context": docker_context, "hosts": [], "dpu_nodes": [],
              "cluster_ids": {}, "management_cluster": name + "-mgmt", "controller_port": port,
              "controller_api": f"http://127.0.0.1:{port}/api", "driver": name + "-driver", "inputs": INPUTS,
              "nvke_revision": command("git", "-C", str(REPO), "rev-parse", "HEAD"),
              "nvke_base_revision": command("git", "-C", str(REPO), "merge-base", "HEAD", "origin/main"),
              "built_images": {ref: command("docker", "image", "inspect", ref, "--format", "{{.Id}}")
                               for ref in [NODE_IMAGE, RUNTIME_IMAGE]}}
    (state / "run.json").write_text(json.dumps(config, indent=2))
    return Simulation(state)


def create_networks(sim):
    """Create the NICo host network and the workload gateway network."""
    label = "nvke-nais-dpu-sim.run=" + sim.config["run_id"]
    command("docker", "network", "create", "--label", label, sim.name)
    command("docker", "network", "create", "--label", label,
            "--subnet", "172.28.234.0/24", sim.name + "-gateway")


def start_nico_simulator(sim):
    """Start the NICo API with container-backed hosts and record its endpoint."""
    name = sim.name + "-nico-sim"
    settings = {
        "NICO_SIM_LISTEN_ADDR": "0.0.0.0:18080",
        "NICO_SIM_DB_PATH": "/state/nico.db",
        "NICO_SIM_SEED_PATH": "/state/seed.yaml",
        "NICO_VMS_VM_BACKEND": "mock",
        "NICO_VMS_BGP_ENABLED": "false",
        "NICO_VMS_CONSOLE_SSH_PORT": "-1",
        "NICO_CONTAINERS_NODE_NETWORK": sim.name,
        "NICO_CONTAINERS_NODE_IMAGE": NODE_IMAGE,
        "NICO_CONTAINERS_REQUIRE_HEALTHY": "true",
    }
    env_args = [arg for key, value in settings.items() for arg in ("-e", key + "=" + value)]
    command("docker", "create", "--name", name, "--network", sim.name,
            "--label", "nvke-nais-dpu-sim.run=" + sim.config["run_id"],
            "-v", "/var/run/docker.sock:/var/run/docker.sock", *env_args, "--entrypoint", "nico-sim", RUNTIME_IMAGE)
    command("docker", "cp", str(sim.state / "seed.yaml"), name + ":/state/seed.yaml")
    command("docker", "start", name)
    wait_until("NICo simulator running", lambda: json.loads(command("docker", "inspect", name))[0]["State"]["Running"], 30)
    address = json.loads(command("docker", "inspect", name))[0]["NetworkSettings"]["Networks"][sim.name]["IPAddress"]
    sim.config["nico_endpoint"] = f"http://{address}:18080"
    sim.save()


def start_device_driver(sim):
    """Start the privileged container used to wire simulated host and DPU devices."""
    command("docker", "run", "-d", "--name", sim.config["driver"],
            "--label", "nvke-nais-dpu-sim.run=" + sim.config["run_id"],
            "--privileged", "--pid=host", "--network=host", "--entrypoint", "/bin/sh", DRIVER_IMAGE, "-c", "sleep infinity")
