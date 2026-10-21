# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Manual command entry point for nvke-nais-dpu-sim."""
import argparse
from pathlib import Path

from . import charts, lifecycle, ovn, resume, scale, validation
from .runtime import Simulation, require


def main():
    parser = argparse.ArgumentParser(description="nvke-nais-dpu-sim: provision and test NVKE DPU and workload networking")
    parser.add_argument("action", choices=["stage", "chart-check", "create", "resume", "scale", "verify", "restart", "install", "cleanup"])
    parser.add_argument("--state", type=Path)
    parser.add_argument("--provider-chart", type=Path)
    parser.add_argument("--docker-context", default="colima")
    parser.add_argument("--workers", type=int, help="Desired workers per cluster for scale")
    args = parser.parse_args()
    if args.action == "stage":
        charts.stage_values()
    elif args.action == "chart-check":
        charts.chart_check()
    elif args.action == "create":
        require(args.provider_chart is not None, "--provider-chart is required")
        lifecycle.create(args.provider_chart.resolve(), args.docker_context)
    else:
        require(args.state is not None, "--state is required")
        sim = Simulation(args.state.resolve())
        if args.action == "resume":
            resume.resume(sim, args.provider_chart)
        elif args.action == "scale":
            require(args.workers is not None, "--workers is required for scale")
            scale.scale(sim, args.workers)
        elif args.action in ("verify", "restart"):
            validation.verify_management(sim)
            {"verify": validation.verify, "restart": ovn.restart}[args.action](sim)
            lifecycle.mark_validated(sim)
        else:
            {"install": ovn.install, "cleanup": lifecycle.cleanup}[args.action](sim)
