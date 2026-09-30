<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Dashboards

Grafana dashboards scenarios are read through, partitioned by Kubernetes minor
version: `dashboards/<kubernetes-minor>/<subsystem>.json`.

Dashboards describe subsystems, not workloads, so scenarios reference them
rather than bundling copies. The version directory pins them: queries are
coupled to the metric names a release emits, so a scenario measured on 1.34
keeps reading the panels it was measured with.

**A directory is valid from its own release until the next directory exists.**
One is added only when metrics change meaningfully, so most releases will not
have their own — use the newest directory at or below your Kubernetes version.
A 1.36 cluster with no `1.36/` or `1.35/` reads `1.34/`. Validation enforces
this: a scenario may not reference a directory ahead of its own version, nor a
stale one when a newer applicable directory exists.

Dashboards are not edited in place for a later release. That is what makes an
old reference safe.

Minor version, not patch — a patch release does not rename metrics. Results are
partitioned by *patch* instead, because a result records one specific run.

All use inline PromQL and need no recording rules. Queries filter on an
optional `k8s_cluster_name` label by regex, which matches everything when the
label is absent, so a single-cluster Prometheus needs no relabeling. A Prometheus
holding several clusters must name them in `k8s_cluster_name` and select one:
queries do not group by cluster, so under "All", or any multi-cluster selection,
aggregated panels add or blend the clusters together.

## 1.34

| Dashboard | Covers |
|---|---|
| [control-plane.json](1.34/control-plane.json) | API request latency by verb and scope, kubelet pod startup SLI, scheduling throughput and latency, API Priority and Fairness, etcd size / fsync / commit / leader changes, control-plane resource use and liveness |
| [networking-ovn.json](1.34/networking-ovn.json) | OVN pod-creation latency, LSP pipeline, CNI request duration, service sync, controller workqueues, northd and controller flow processing, CoreDNS |
| [scheduler.json](1.34/scheduler.json) | Scheduling attempts and latency, pending-pod queues, preemption, framework extension points, plugin execution, leader election |
| [worker-nodes.json](1.34/worker-nodes.json) | Kubelet PLEG, pod worker, cgroup manager, runtime operations; node CPU, memory, network, PSI, conntrack, filesystem and disk headroom |

Validated on Kubernetes 1.34.2 with OVN-Kubernetes, against a Prometheus
carrying no `k8s_cluster_name` label.

These return no data on a healthy cluster, which is the result they exist to
show — the series is absent because the event did not occur:

- **Permit Wait Duration** (`scheduler.json`) —
  `scheduler_permit_wait_duration_seconds` is only observed when a Permit
  plugin defers a pod, and no default plugin uses it.
- **APF rejected requests** (`control-plane.json`) —
  `apiserver_flowcontrol_rejected_requests_total` is partitioned by
  `flow_schema` and `priority_level`, so zero rejections means no series.
- **CoreDNS Tripwire** fault rcodes (`networking-ovn.json`) — only SERVFAIL,
  REFUSED and other failures are shown; with none, the right half is empty.
