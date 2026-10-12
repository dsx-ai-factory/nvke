<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Results

Published benchmark results, partitioned by the Kubernetes version they were
measured on:

```
results/<kubernetes-version>/
├── <scenario-slug>-<date>.md    the result
└── charts/                      SVGs referenced by it
```

Charts sit in `charts/` rather than beside the results — two per result adds up
quickly once a version directory holds several.

**These are patch-level directories** — `1.34.2`, not `1.34`. A result is a
record of one run on one release, and a patch release can move the numbers.
Dashboards and alerts are partitioned by *minor* version instead, since they
change only when metric names do; that difference is deliberate.

Every result names the scenario that produced it in its frontmatter, and
validation checks that the reference resolves. A result is never standalone —
the scenario is what makes it reproducible.

```yaml
---
type: result
scenario: ../../workloads/cni/k8s-netperf-throughput/scenario.yaml
workload: cni
k8s_version: 1.34.2
date: 2026-05-26
authors: [NVIDIA NVKE Scale Team]
---
```

## Reading one

Most results follow the same shape — TL;DR, Scope of this publication,
Environment, Results summary, Acceptance gate, Observations — but sections are
not uniform and are not enforced by validation. Older results carry the
sections they were originally written with.

**Where a result states its own scope, read that before quoting a number.** It
says which cells ran, which were dropped, and what the result does not cover.

**Compare ratios, not absolutes.** Absolute figures are a property of the
hardware they were measured on. What transfers between clusters is the
relationship a scenario exposes — an overlay path against a host-network
reference on the same cluster, or one workload across a version bump.

**Failures are published.** Where a gate is not met, the result says so rather
than omitting the cell.

No result here is a maximum-scale claim. `topology` and `sizing.observed` in
the scenario describe what a given run measured, not a ceiling.
