<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# NKE benchmarks

Scale and performance benchmarks for NKE clusters: the recipes that were run,
and the results obtained.

Every scenario is defined as data and runs on publicly available tools.

## Layout

```
benchmarks/
├── schemas/       JSON Schemas for scenarios and results
├── workloads/     scenario definitions, grouped by workload
├── AGENTS.md      guide for AI agents reading or extending this tree
├── dashboards/    Grafana dashboards, by Kubernetes minor
├── alerts/        alerting rules, by Kubernetes minor
└── results/       published results, by Kubernetes patch, charts in charts/
```

Version directories differ on purpose. `results/` is partitioned by **patch**
(`1.34.2`) because a result records one run on one release. `dashboards/` and
`alerts/` are partitioned by **minor** (`1.34`) because they change only when
metric names do, not on every patch.

## Definitions

| Term | Meaning |
|---|---|
| **workload** | What is measured — `apiserver`, `cni`, `scalability`, `scheduler`. Groups scenarios. |
| **scenario** | One recipe for a workload, pinned to a software stack. Validates against [`schemas/scenario.schema.json`](schemas/scenario.schema.json). The unit to reproduce. |

Scenario directory:

```
workloads/<workload>/<scenario-slug>/
├── scenario.yaml     definition
├── README.md         how to replicate — start here
├── engine/           engine-native config
└── alerts.yml        rules scoped to this workload (optional)
```

Dashboards and shared alerting rules are referenced, not bundled:

```
dashboards/<kubernetes-minor>/     control-plane.json, networking-ovn.json, …
alerts/<kubernetes-minor>/         control-plane.yml, …
```

Panels and rules describe *subsystems* — control plane, networking, worker
nodes, scheduler — not the workload that exercised them, so several scenarios
share one copy. `dashboard` and `alerts` each accept a list, since a run is
usually read through more than one.

The version directory pins them: queries and thresholds are coupled to the
metric names a release emits. A directory is valid from its release until the
next one exists — they are added only when metrics change — so a scenario
references the newest directory at or below its own version. Validation rejects
a reference ahead of the scenario, or a stale one when a newer applicable
directory exists.

Rules specific to one workload — those selecting on its own namespace or pods —
stay beside the scenario. `alerts` is optional, but a scenario shipping none
should say so in its README. Thresholds calibrated for one environment can be
wrong in another: a rule that fires on simulated storage rather than a real
problem is noise, not coverage.

Revalidating on a newer stack adds a sibling `scenario-<version>.yaml` in the
same directory, sharing the engine config.

## Not included

| | |
|---|---|
| Raw measurement data | Results are markdown and charts. Regenerate from the recipe. |
| A runner or CLI | A scenario is a config file plus a documented command. |
| Verbatim-executable scripts | A scenario names engine, version, config and argv. Engine flags change between releases and cluster shapes differ — read the scenario, then run the tool it names. |

## Reading a result

Results live in `results/<k8s-version>/`. Frontmatter is validated against
`schemas/result.schema.json` and names the scenario that produced it:

```yaml
---
type: result
scenario: ../../workloads/cni/k8s-netperf-throughput/scenario.yaml
workload: cni
k8s_version: 1.34.2
date: 2026-05-26
authors: [NVIDIA NKE Scale Team]
---
```

Sections, in order:

| Section | Contains |
|---|---|
| TL;DR | Headline number and finding |
| Scope of this publication | Which cells are present, which were dropped, and why. Read before quoting a number. |
| Environment | The stack the numbers came from |
| Results summary | Measurements |
| Acceptance gate | Pass/fail against the scenario's `slo` block |
| Observations | Why the numbers look as they do |

**Compare ratios, not absolutes.** Absolute throughput is a property of the
hardware. What transfers between clusters is the relationship a scenario
exposes — an overlay path against a host-network reference on the same cluster,
or one workload across a version bump.

**Failures are published.** Where a gate is not met, the result says so.

## Replicating a scenario

Per-scenario instructions live in each scenario's `README.md`, covering: what is
tested, how it is tested, artifacts and scripts, environment, how to replicate,
how to judge the result.

Worked example: [`workloads/cni/k8s-netperf-throughput/`](workloads/cni/k8s-netperf-throughput/README.md).

1. Read `scenario.yaml`. `topology` is the cluster the published run used;
   `sizing.derivation` is the rule for scaling to a different one.
2. Check `prerequisites`, then run the `baseline_validation` steps.
3. Install the engine named in `upstream` at `upstream.engine_version`.
4. Run `upstream.argv` against `engine_config`.
5. Judge the run against `slo`.

`engine_version` is a release tag where the engine publishes one, and otherwise
a commit SHA on a best-effort basis. Not every tool is versioned, and
for runs already completed the exact commit is not always recoverable — record
what is known rather than leaving it blank.

Engine documentation is linked from the scenario, not restated here.

## Sizing

| Field | Meaning |
|---|---|
| `topology` | Cluster the published run used: node count, node type, role split |
| `sizing.derivation` | Rule for computing run parameters from cluster shape, and the properties it reads. This is the part to apply elsewhere. |
| `sizing.observed` | What that rule resolved to for the published run |

A fixed iteration count cannot transfer to different hardware; a derivation rule
can. Both are published.

T-shirt sizes (S/M/L/XL) are points on a scenario's own scale. `L` in one
scenario implies nothing about `L` in another.

**No result here is a maximum-scale claim.** `topology` and `sizing.observed`
are what a given run happened to measure, not a ceiling on what NKE or the
component under test can do — a bigger topology is a different
`sizing.observed`, not a different capability. Read `sizing.derivation` as an
instruction to authors and readers alike: size the workload to your own
cluster rather than treating the published node count, concurrency level or
`XL` as the largest this goes.

## Licence

Apache 2.0 — see [LICENSE](../LICENSE).

Each scenario's `upstream.project` names the project its engine configuration
comes from.
