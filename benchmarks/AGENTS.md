<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Agent guide — benchmarks

For AI agents reading this tree to answer questions about NVKE scale and
performance, or adding to it. Humans should start at [README.md](README.md).

This file explains how the tree works. It deliberately does not list what is in
it — the layout is regular and the schemas are authoritative, so enumerate the
directories rather than trusting an index that can go stale.

## Structure

```
benchmarks/
├── schemas/       JSON Schemas — the authoritative contract
├── workloads/<workload>/<scenario-slug>/
│                  scenario.yaml, README.md, engine/, optional alerts.yml
├── dashboards/<kubernetes-minor>/
├── alerts/<kubernetes-minor>/
└── results/<kubernetes-patch>/
                   <scenario-slug>-<date>.md, charts/
```

A **scenario** is the unit to reproduce: what was run, on what stack, against
what thresholds. A **result** is one run of one scenario. Results name their
scenario in frontmatter; scenarios never name results.

Everything is text and machine-readable already — scenarios and engine configs
are YAML, results are Markdown with YAML frontmatter, schemas are JSON Schema.
To answer a question, parse the scenario for the recipe and the result for the
numbers. `benchmarks/schemas/scenario.schema.json` documents every field.

## Answering questions about results

These are the mistakes most likely to produce a confidently wrong answer.

**No result is a maximum-scale claim.** `topology` and `sizing.observed`
describe what one run happened to measure, not a ceiling on what NVKE or the
component under test can do. A bigger cluster is a different `sizing.observed`,
not a different capability. Never report a published number as a limit.

**Compare ratios, not absolutes.** Absolute throughput and latency are
properties of the hardware they were measured on. What transfers between
clusters is the relationship a scenario exposes — an overlay path against a
host-network reference on the same cluster, or one workload across a version
bump.

**An absent metric is usually the healthy state, not a gap.** Several metrics
are label-partitioned and simply do not exist until the event they count
happens. `apiserver_flowcontrol_rejected_requests_total` is absent when nothing
has been rejected. Absence means "it did not happen", not "not measured", and
never means the panel is broken.

**Check what a result says about its own scope before quoting a number.** Most
carry a "Scope of this publication" section stating which cells ran, which were
dropped and what is not covered. Result sections are not uniform across the
tree and are not enforced, so read what is there rather than assuming a
section exists.

**Failures are published.** A gate that was not met is reported, not omitted.
Do not assume every published result passed.

**T-shirt sizes are local to a scenario.** `L` in one scenario implies nothing
about `L` in another. They are points on that scenario's own scale.

## Versioning

Directories differ on purpose:

- `results/` is **patch**-versioned (`1.34.2`) — a result records one run on one
  release, and a patch can move the numbers.
- `dashboards/` and `alerts/` are **minor**-versioned (`1.34`) — they change only
  when metric names do.

A dashboard or alert directory is valid from its release until the next one
exists. They are added only when metrics change meaningfully, so most releases
have none of their own: resolve to the newest directory **at or below** the
scenario's Kubernetes minor. Validation rejects a reference ahead of the
scenario, or a stale one when a newer applicable directory exists.

Dashboards are never edited in place for a later release. That is what makes an
old reference safe.

## Adding to the tree

Contributions are validated before publication against the schemas in
`schemas/`, the reference paths every document declares, and a sanitization
gate. The checks run where this tree is maintained, not from here — this
directory is published without the tooling that produces it.

What the gate rejects, and therefore what not to write:

- Cluster and host names from the environments a run used, private registry
  paths, hardware SKU identifiers, programme names, non-public URLs, issue keys
  and individual usernames.
- **Charts as PNG.** SVG is text and can be read and checked; a PNG cannot, and
  its axis labels carry whatever the plot was built from.

Other rules the contract enforces:

- `authors` attributes to an org or team, never an individual.
- Every scenario directory carries a `README.md` with all six required H2
  sections. The skeleton is enforced; the content is a human judgement.
- Every reference — `engine_config`, `dashboard`, `alerts`, a result's
  `scenario` — must resolve inside this tree.

Do not state a fact the source does not support. Where a value was derived
rather than observed, say so beside it — several fields here are best-effort
reconstructions from run dates and archived repositories, and each says which.
