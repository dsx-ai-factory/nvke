# AGENTS.md — `nktp/`

The NVIDIA Kubernetes Telemetry Pipeline (NKTP) Helm chart. It deploys an
OpenTelemetry pipeline for cluster metrics, logs and traces. There is no Go code
here and no image; the chart is the whole component.

## What to read first

- [deploy/charts/nktp/README.md](deploy/charts/nktp/README.md) — what the chart
  deploys, its values, the development backend and the bundled dependencies

## Layout

```text
deploy/charts/nktp/
  Chart.yaml, Chart.lock   # chart version and dependency pins
  values.yaml              # every Collector's configuration and workload
  templates/               # Collectors, kube-state-metrics, RBAC, development backend
  dashboards/              # Grafana dashboards the development backend loads
hack/chart-validate.sh     # renders the chart, validates it; make nktp-chart-lint runs it
```

## Commands

From the repository root:

```bash
make nktp-chart-deps   # download the dependencies into deploy/charts/nktp/charts/
make nktp-chart-check  # lint, render and validate the chart; the merge gate runs this
```

Both need network access. The dependencies come from the chart repositories
named in `Chart.yaml`, and kubeconform downloads its schemas.

## Constraints

- A released chart version is immutable. Once the `version:` in `Chart.yaml`
  has been released, any change to the chart bumps it, including the README and
  dashboards, which ship in the package. A release tag `nktp/vX.Y.Z` has to
  equal it.
- Dependency pins in `Chart.yaml` and `Chart.lock` move together, in their own
  pull request, with `helm dependency update nktp/deploy/charts/nktp`.
  `make nktp-chart-deps` fails when the two disagree.
- Never commit `deploy/charts/nktp/charts/`. The chart's `.gitignore` keeps the
  downloaded archives out.
- Every YAML file carries the Apache-2.0 SPDX header. In `templates/` it is a
  Go-template comment, so the rendered manifests do not change:

  ```yaml
  {{- /*
  SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
  */ -}}
  ```

  Keep both trim markers. Do not add headers to `templates/` with addlicense: it
  writes `#` lines, and Helm copies those into every rendered manifest.
- When syncing the chart from its upstream project, take only `Chart.yaml`,
  `Chart.lock`, `values.yaml`, `templates/` and `dashboards/`, then re-apply the
  header rule to any new YAML file. The README, checks and agent guidance here
  are written for this repository; do not replace them with upstream copies.
