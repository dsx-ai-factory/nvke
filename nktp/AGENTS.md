# AGENTS.md — `nktp/`

The NVIDIA Kubernetes Telemetry Pipeline (NKTP) Helm chart. It deploys an
OpenTelemetry pipeline for cluster metrics, logs and traces. There is no Go code
here and no image; the chart is the whole component.

## What to read first

- [deploy/charts/nktp/README.md](deploy/charts/nktp/README.md) — prerequisites,
  installation, the components and the configuration model, in summary
- [docs/nktp/architecture/topology.md](../docs/nktp/architecture/topology.md) —
  how the Collectors fit together, with links to each component's page under
  [docs/nktp/components/](../docs/nktp/components/) and the guides under
  [docs/nktp/guides/](../docs/nktp/guides/)

## Layout

```text
deploy/charts/nktp/
  Chart.yaml, Chart.lock   # chart version and dependency pins
  values.yaml              # every Collector's configuration and workload
  templates/               # Collectors, kube-state-metrics, RBAC, development backend
  dashboards/              # Grafana dashboards: default/ the development backend loads, examples/ import-only
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
  equal it; `.github/workflows/nktp-chart-publish.yml` rejects one that does not.
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
- The README is packaged into the chart, where links to other files do not
  resolve. Keep it a self-contained summary that links only to the
  documentation under `docs/nktp/`, from its last section.
- When a change alters a component's behavior or values, update its page under
  `docs/nktp/components/`, and any guide under `docs/nktp/guides/` that shows
  those values, in the same pull request.
