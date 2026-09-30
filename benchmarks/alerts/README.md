<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Alerts

Alert definitions a run is watched with, partitioned by Kubernetes minor
version: `alerts/<kubernetes-minor>/<subsystem>.yml`.

Each file is a flat list of `expr` / `description` / `severity` entries — the
alert-profile format benchmark engines such as kube-burner consume directly.
**This is not Prometheus `rule_files` format**, which expects `groups` of named
`rules`; loading these into Prometheus unchanged will not work. The PromQL in
`expr` is portable, so converting is mechanical if that is what you want.

An engine may load these files; where it does, a firing `severity: error` rule
fails the run. Otherwise they are a second signal to watch while a run is in
flight.

Rules here describe *subsystems*, so several scenarios share one file. Rules
scoped to a single workload — those selecting on its own namespace or pod names
— live beside that scenario instead, since a directory describing one run
already pins them.

## Versioning

A directory is valid from its release until the next one exists. One is added
only when metric names or thresholds change meaningfully, so most releases will
not have their own — use the newest directory at or below your Kubernetes
version. Validation rejects a scenario referencing a directory ahead of its
own version, or a stale one when a newer applicable directory exists.

Minor version, not patch — a patch release does not rename metrics. Results are
partitioned by *patch* instead, because a result records one specific run.

## 1.34

| File | Covers |
|---|---|
| [control-plane.yml](1.34/control-plane.yml) | API request latency by verb and scope, API Priority and Fairness, etcd disk and leader changes, control-plane liveness |
| [scheduler.yml](1.34/scheduler.yml) | Scheduling attempts returning error, backoff and unschedulable queue convergence, stalled-throughput detection, attempt-latency diagnostic |

`scheduler.yml` tracks no *performance* SLO, and that is not an omission.
Kubernetes publishes no upstream bar for scheduler throughput or scheduling
latency the way SIG-Scalability does for API request latency and pod startup,
so there is none to encode. Its `error` rules track correctness and convergence
SLOs instead — a scheduling attempt returning `error`, a queue that never
drains — which have a right answer independent of hardware and therefore
travel anywhere. Its one `warning` rule is a diagnostic that a rate-limited
scheduler will breach by design. A scheduler capacity bar, where one is
warranted, belongs in the scenario that measured it.

## Reading a rule

`severity: error` tracks an SLO a scenario is judged against. `severity:
warning` explains a violation rather than constituting one.

Thresholds are calibrated for the hardware the published results came from. The
API-latency bars are the SIG-Scalability SLOs and travel anywhere. The etcd disk rules assume real disks — on virtualized or shared
storage they report the storage layer rather than a control-plane problem.
