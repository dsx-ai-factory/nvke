# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Create clusters, configure their networking, and clean up a simulation run."""
import json
import os
import time


from .config import STATE, WORKLOAD_ROLE
from .runtime import Simulation, command, wait_until
from .dpu_workload_network import setup as dpu_workload_network_setup
from . import clusters, devices, environment, fabric, management, ovn, validation


def create(provider_chart, docker_context):
    sim = environment.prepare(provider_chart, docker_context)
    management.management_up(sim, provider_chart)
    prepare_dpu_cluster(sim)
    prepare_workload_cluster(sim)
    print(f"PASS nvke-nais-dpu-sim with three clusters. Kubeconfigs and evidence: {sim.state}", flush=True)


def prepare_dpu_cluster(sim: Simulation):
    """Create a new DPU cluster, save its nodes, and set up workload networking."""
    clusters.provision(sim, "dpu")
    sim.dpus.extend(wait_for_dpu_nodes(sim))
    sim.save()
    configure_dpu_cluster(sim)


def wait_for_dpu_nodes(sim: Simulation):
    """Wait for the expected NVKE DPU nodes to have running containers connected to the Docker network created for this run."""
    expected_node_count = 1 + sim.config.get("workers", 1)

    def discover_expected_dpu_nodes():
        nodes = clusters.discover(sim, "dpu")
        if len(nodes) == expected_node_count:
            return nodes
        return None

    return wait_until("NVKE-provisioned DPU nodes", discover_expected_dpu_nodes)


def configure_dpu_cluster(sim: Simulation):
    """Wait for ready DPU nodes and primary OVN, set up workload networking, and export access."""
    wait_until("NVKE DPU cluster provisioned", lambda: clusters.ready(sim, "dpu"))
    wait_until("DPU Kubernetes nodes Ready", lambda: clusters.nodes_ready(sim, "dpu"), 300)
    # waits until the DPU cluster’s primary OVN networking pods are ready.
    sim.dpu_kubectl("rollout", "status", "daemonset/ovnkube-node", "-n", "ovn-kubernetes", "--timeout=300s")
    dpu_workload_network_setup(sim)
    clusters.export_kubeconfig(sim, "dpu", sim.dpus)
    print(f"PASS DPU cluster provisioned by NVKE and all {len(sim.dpus)} nodes Ready", flush=True)


def prepare_workload_cluster(sim: Simulation):
    """Create a new workload cluster and configure its hosts and networking."""
    clusters.provision(sim, WORKLOAD_ROLE)
    configure_workload_cluster(sim)


def configure_workload_cluster(sim: Simulation, record_success=True):
    """Pair new hosts with DPUs, configure networking, wait for readiness, and test both clusters."""
    deadline = time.monotonic() + 1200
    while time.monotonic() < deadline:
        nodes = clusters.discover(sim, WORKLOAD_ROLE)
        # The first host supplies admin.conf and API access for the fixtures.
        if not sim.hosts and not any("-cp-" in node["name"] for node in nodes):
            time.sleep(3)
            continue
        added = False
        for node in nodes:
            if any(h["node_id"] == node["node_id"] for h in sim.hosts):
                continue
            used = {host["dpu"] for host in sim.hosts}
            dpu = next(d for d in sim.dpus if d["container"] not in used)
            node["dpu"] = dpu["container"]
            node["dpu_node_name"] = dpu["name"]
            node["management"] = sim.management(node["container"])
            sim.hosts.append(node)
            sim.save()
            added = True
        if sim.hosts and (added or any(not h.get("fixtures_ready") for h in sim.hosts)):
            host_ids = sorted(h["node_id"] for h in sim.hosts)
            if sim.config.get("installed_host_ids") != host_ids:
                wait_until("workload API initialized by node agent", lambda: sim.exec(sim.hosts[0]["container"], "test", "-s", "/etc/kubernetes/admin.conf") is not None)
                devices.wire(sim)
                ovn.load(sim)
                devices.simulation_links(sim)
                devices.load_helpers(sim)
                node = json.loads(sim.kubectl("get", "node", sim.hosts[0]["name"], "-o", "json"))
                if any(t["key"] == "node-role.kubernetes.io/control-plane" for t in node["spec"].get("taints", [])):
                    sim.kubectl("taint", "node", sim.hosts[0]["name"], "node-role.kubernetes.io/control-plane-")
                ovn.install(sim)
                sim.config["installed_host_ids"] = host_ids
                sim.save()
            fabric.routing(sim)
            for host in sim.hosts:
                host["fixtures_ready"] = True
            sim.save()
        if sim.hosts:
            ovn.configure_dns_devices(sim)
            ovn.activate_offload(sim)
        if len(sim.hosts) == 1 + sim.config.get("workers", 1) and clusters.ready(sim, WORKLOAD_ROLE):
            wait_until("workload Kubernetes nodes Ready", lambda: clusters.nodes_ready(sim, WORKLOAD_ROLE), 300)
            ovn.activate_offload(sim)
            clusters.export_kubeconfig(sim, WORKLOAD_ROLE, sim.hosts)
            validation.verify(sim)
            validation.verify_management(sim)
            if record_success:
                mark_validated(sim)
            return
        time.sleep(5)
    raise RuntimeError("NVKE workload cluster did not complete provisioning; inspect retained nvke-nais-dpu-sim state")


def cleanup(sim):
    for role, cid in sim.config["cluster_ids"].items():
        sim.nvke("DELETE", "/clusters/" + cid)
        print(f"Requested NVKE teardown of {role}", flush=True)
    owned_ids = set(sim.config["cluster_ids"].values())
    if owned_ids:
        wait_until("NVKE teardown of managed nodes and cluster records", lambda: not sim.nvke("GET", "/nodes")["nodes"] and not any(
            c["id"] in owned_ids for c in sim.nvke("GET", "/clusters")["clusters"]), 900)
    command("kind", "delete", "cluster", "--name", sim.config["management_cluster"])
    ids = command("docker", "ps", "-aq", "--filter", "label=nvke-nais-dpu-sim.run=" + sim.config["run_id"]).split()
    if ids:
        command("docker", "rm", "-f", *ids)
    for net in command("docker", "network", "ls", "-q", "--filter", "label=nvke-nais-dpu-sim.run=" + sim.config["run_id"]).split():
        command("docker", "network", "rm", net)
    print("Removed nvke-nais-dpu-sim resources through NVKE teardown and run-owned management cleanup", flush=True)


def mark_validated(sim):
    sim.config["validated"] = True
    sim.save()
    latest = STATE / "managed-latest"
    latest.unlink(missing_ok=True)
    latest.symlink_to(sim.state.name, target_is_directory=True)
