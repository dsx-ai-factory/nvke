# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Add workers to existing clusters while preserving node pairings and saved setup progress."""
import json
import shutil
import subprocess

from . import lifecycle
from .config import WORKLOAD_ROLE
from .runtime import Simulation, require


def scale(sim, workers):
    require(1 <= workers <= 3, "nvke-nais-dpu-sim supports one to three workers per cluster")
    require(workers >= sim.config.get("workers", 1), "scale cannot decrease worker capacity")
    if workers > sim.config.get("workers", 1):
        baseline = sim.state / ("before-scale-" + str(workers))
        baseline.mkdir(mode=0o700, exist_ok=True)
        for name in ["management-validation.json", "network-matrix.json", "ovn-runtime-validation.json",
                     "dpu-primary-validation.json", "host-provisioning-validation.json"]:
            shutil.copy2(sim.state / name, baseline / name)
    sim.config["workers"] = workers
    sim.config["validated"] = False
    sim.save()
    for role in ["dpu", WORKLOAD_ROLE]:
        pools = sim.nvke("GET", "/nodePools?clusterID=" + sim.config["cluster_ids"][role])["nodePools"]
        pool = next(p for p in pools if not p["controlPlane"])
        if role == "dpu":
            scale_dpu_cluster(sim, pool["id"], workers)
        else:
            baselines = sim.config.setdefault("scale_baselines", {})
            existing = set(baselines.setdefault(str(workers), [host["name"] for host in sim.hosts]))
            versions = sim.config.setdefault("scale_watch_versions", {})
            version = versions.setdefault(str(workers), json.loads(sim.kubectl("get", "--raw", "/api/v1/nodes"))["metadata"]["resourceVersion"])
            require(version.isdigit() and int(version) > 0, "Node creation watch requires an API list revision")
            sim.save()
            events = sim.state / f"worker-registration-events-{workers}.json"
            with events.open("a") as output, (sim.state / f"worker-registration-watch-{workers}.log").open("a") as errors:
                watch = subprocess.Popen(["kubectl", "--kubeconfig", str(sim.state / "workload.kubeconfig"),
                    "get", "--raw", "/api/v1/nodes?watch=true&resourceVersion=" + version,
                    "--request-timeout=900s"], stdout=output, stderr=errors)
                try:
                    sim.nvke("PATCH", "/nodePools/" + pool["id"], {"desiredNodeCount": workers})
                    print(f"Requested {workers} workload workers through the NVKE node-pool API", flush=True)
                    lifecycle.configure_workload_cluster(sim, record_success=False)
                finally:
                    watch.terminate()
                    watch.wait(timeout=10)
            decoder = json.JSONDecoder()
            pending = events.read_text().strip()
            registered = {}
            while pending:
                event, end = decoder.raw_decode(pending)
                pending = pending[end:].lstrip()
                node = event["object"]
                if event["type"] == "ADDED":
                    require(node["metadata"]["name"] not in existing, "Watch returned a pre-existing Node; cannot prove first registration")
                    registered.setdefault(node["metadata"]["name"], node["metadata"]["labels"])
            added = {host["name"] for host in sim.hosts} - existing
            require(added and added <= registered.keys(), "Node watch did not capture all new workers' creation events")
            for name in added:
                require(registered[name].get("k8s.ovn.org/dpu-host") == "" and registered[name].get("nodeType") == "gpu",
                        "New worker was missing required labels at first registration: " + name)
            (sim.state / "worker-registration-validation.json").write_text(json.dumps(registered, indent=2))
            lifecycle.mark_validated(sim)
            print("PASS new workers carried DPU-host and GPU labels on their initial Node creation events", flush=True)


def scale_dpu_cluster(sim: Simulation, worker_pool_id, workers):
    """Request more DPU workers and prepare them without changing existing host-to-DPU pairs."""
    sim.nvke("PATCH", "/nodePools/" + worker_pool_id, {"desiredNodeCount": workers})
    print(f"Requested {workers} DPU workers through the NVKE node-pool API", flush=True)
    refresh_saved_dpu_nodes(sim)
    lifecycle.configure_dpu_cluster(sim)


def refresh_saved_dpu_nodes(sim: Simulation):
    """Refresh DPU records during resume or scale, preserving their order and saved setup progress."""
    discovered_dpu_nodes = lifecycle.wait_for_dpu_nodes(sim)
    dpu_nodes_to_save = []

    # Refresh saved DPUs in their existing order. Keep their setup fields, such as offload_ready.
    for saved_node in sim.dpus:
        for discovered_node in discovered_dpu_nodes:
            if saved_node["node_id"] == discovered_node["node_id"]:
                refreshed_node = saved_node.copy()
                refreshed_node.update(discovered_node)
                dpu_nodes_to_save.append(refreshed_node)
                break
        # If discovery no longer finds this saved DPU, leave it out of the saved list.

    # Append new DPUs after the existing ones so existing fixture addresses stay unchanged.
    for discovered_node in discovered_dpu_nodes:
        already_recorded = False
        for recorded_node in dpu_nodes_to_save:
            if recorded_node["node_id"] == discovered_node["node_id"]:
                already_recorded = True
                break

        if not already_recorded:
            dpu_nodes_to_save.append(discovered_node)

    # Update the shared list so config["dpu_nodes"] still points to these records.
    sim.dpus[:] = dpu_nodes_to_save
    sim.save()
