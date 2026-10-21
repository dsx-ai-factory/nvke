# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Continue a retained simulation run after a failure."""
from . import lifecycle, management
from .runtime import Simulation, require
from .config import WORKLOAD_ROLE
from .scale import refresh_saved_dpu_nodes


def resume(sim: Simulation, provider_chart=None):
    """Retry a retained run without recreating clusters whose IDs are already saved."""
    if not sim.config.get("management_ready"):
        require(provider_chart is not None, "--provider-chart is required to resume management bootstrap")
        management.management_up(sim, provider_chart.resolve())
    resume_dpu_cluster(sim)
    resume_workload_cluster(sim)


def resume_dpu_cluster(sim: Simulation):
    """Continue DPU preparation using the cluster ID and setup progress saved before a failure."""
    if "dpu" not in sim.config["cluster_ids"]:
        lifecycle.prepare_dpu_cluster(sim)
        return

    refresh_saved_dpu_nodes(sim)
    lifecycle.configure_dpu_cluster(sim)


def resume_workload_cluster(sim: Simulation):
    """Continue workload preparation using the saved cluster ID, host pairings, and setup progress."""
    if WORKLOAD_ROLE not in sim.config["cluster_ids"]:
        lifecycle.prepare_workload_cluster(sim)
        return

    lifecycle.configure_workload_cluster(sim)
