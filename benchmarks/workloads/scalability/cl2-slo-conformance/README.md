<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# ClusterLoader2 SLO conformance

Definition: [scenario.yaml](scenario.yaml)
Result: [cl2-slo-conformance-2026-05-25.md](../../../results/1.34.2/cl2-slo-conformance-2026-05-25.md)

## What is tested

Control-plane behavior under a mixed, production-representative object-churn
workload, judged against the three official SIG-Scalability Service Level
Objectives: mutating API latency, read-only API latency and pod startup
latency. Their bars are under "How to judge the result" below.

**This is not a maximum-scale claim.** The 47-node topology below is what
this run happened to test, not a ceiling on how large an NKE cluster can go.
`sizing.derivation` exists so a reader can size this workload to their own
node count — a bigger or smaller cluster is a different `sizing.observed`,
not a different scenario.

Alongside the SLOs, the run also characterizes scheduler throughput,
control-plane CPU/memory headroom, in-cluster network and DNS lookup latency,
and etcd DB growth — supporting signal for judging whether a cluster has
headroom for growth, not gated pass/fail criteria in their own right.

`engine/overrides.yaml` sets no request rate, so ClusterLoader2 runs at its
defaults.

## How it is tested

One ClusterLoader2 `load` run creates, churns and deletes a fixed object mix,
then tears it down. ClusterLoader2 sizes it from the schedulable, untainted
node count; on the published topology `engine/overrides.yaml` gives 14
namespaces and:

| Object | Count | Replicas / completions |
|---|---|---|
| Small deployments | 448 | 5 |
| Medium deployments | 14 | 30 |
| Small jobs | 14 | 15 completions |
| Medium jobs | 14 | 90 completions |

StatefulSets and DaemonSets are disabled.

ClusterLoader2 measures API response latency from Prometheus
(`APIResponsivenessPrometheus`, `useSimpleLatencyQuery` mode), pod startup
latency from its own `PodStartupLatency` measurement — timestamps taken
directly off the pod watch stream's create/schedule/run transitions, not a
kubelet-reported metric — and in-cluster ping and DNS lookup latency itself
during the run. Control-plane CPU/memory, etcd DB size and API Priority and
Fairness (APF) inflight counts are read from Prometheus before and after.

## Artifacts and scripts

| Artifact | Role |
|---|---|
| [engine/overrides.yaml](engine/overrides.yaml) | The `--testoverrides` file this scenario owns. Engine-native — hand to upstream ClusterLoader2 unchanged. |
| [scenario.yaml](scenario.yaml) | Topology, stack, sizing rule, metrics contract, SLOs |
| [control-plane.json](../../../dashboards/1.34/control-plane.json) | Grafana dashboard, shared. Inline PromQL, no recording rules. Threshold lines mark the SLOs, not observed values. |
| [control-plane.yml](../../../alerts/1.34/control-plane.yml) | Alerting rules, shared. `severity: error` tracks an SLO; `severity: warning` explains a violation rather than judging it. |

Upstream: [ClusterLoader2](https://github.com/kubernetes/perf-tests/tree/master/clusterloader2),
part of `kubernetes/perf-tests`, pinned at commit
`14525e2e2e08a236a2adb6f4f39740c586ed45fe` (2026-08-24). ClusterLoader2 has
no tagged releases, so this scenario pins a commit.

```bash
cd <perf-tests>/clusterloader2
go run cmd/clusterloader.go \
  --provider=<cloud-provider> \
  --kubeconfig=<path/to/kubeconfig> \
  --testconfig=testing/load/config.yaml \
  --testoverrides=<scenario-dir>/engine/overrides.yaml \
  --enable-prometheus-server=true \
  --prometheus-apiserver-scrape-port=<apiserver-port> \
  --report-dir=<report-dir>
```

`<perf-tests>` is a checkout of `kubernetes/perf-tests` at the pinned commit;
the `load` config is not vendored here. `engine/overrides.yaml` is the only
file this scenario owns.

`--enable-prometheus-server=true` makes ClusterLoader2 deploy its own
Prometheus. Its Prometheus-based measurements, API latency included, are
skipped without it. That Prometheus scrapes the API server on port 443 unless
`--prometheus-apiserver-scrape-port` says otherwise.

### Dashboard

`control-plane.json` needs a Prometheus scraping the control plane. Two things
to read carefully before trusting a panel:

- **The pod-startup panel is a proxy, not the gated number.** It plots
  `kubelet_pod_start_sli_duration_seconds_bucket`. The scenario's
  `pod_startup_p99_s` comes from ClusterLoader2's own `PodStartupLatency`
  measurement, which times create/schedule/run transitions off the pod watch
  stream rather than reading a Prometheus metric. The two will not match
  exactly.
- **The throughput threshold line is not a gate here.** ClusterLoader2 gates
  scheduler throughput only in a separate phase that runs on clusters of 100
  or more schedulable, untainted nodes. The panel plots the whole run.

The queries expect the `job` labels `apiserver`, `kube-etcd`,
`kube-controller-manager`, `kube-scheduler` and `kubelet`; the "Control-plane
component up" panel shows any that do not match.

### Alerts

**`control-plane.yml` does not judge the run.** ClusterLoader2 takes no alerts
input; it writes its own checks to `junit.xml` under `--report-dir`. The rules
are for watching a run in flight.

Below 100 schedulable, untainted nodes, `junit.xml` does not include the 5 s
pod-startup or scheduler-throughput checks. Judge `pod_startup_p99_s` against
`slo` from the `CreatePhasePodStartupLatency` summary in `--report-dir`.

`post_p99_ms` and `delete_p99_ms` share the mutating-API `severity: error`
rule, and `get_p99_ms` has a resource-scope rule at 1 s. A separate rule holds
namespace- and cluster-scoped LISTs to their 30 s bar; it tracks no `slo`
entry. `pod_startup_p99_s` has no rule.

### What the overrides file changes from upstream defaults

Every key in `engine/overrides.yaml` was confirmed directly against the
pinned commit's templates:

| Key | Value | Effect |
|---|---|---|
| `NODES_PER_NAMESPACE` | 3 | Drives namespace count: `floor(schedulable untainted nodes / 3)`, 14 on the published topology |
| `PODS_PER_NODE` | 110 | Pod density budget the workload mix is sized against |
| `SMALL_STATEFUL_SETS_PER_NAMESPACE` / `MEDIUM_STATEFUL_SETS_PER_NAMESPACE` | 0 / 0 | Removes StatefulSets — and the PersistentVolumes they would claim — from the mix |
| `CL2_SCHEDULER_THROUGHPUT_THRESHOLD` | 15 | Pass/fail gate of the scheduler-throughput phase, pods/s. That phase runs only on clusters of 100 or more schedulable, untainted nodes. |
| `CL2_SCHEDULER_THROUGHPUT_PODS_PER_DEPLOYMENT` | 80 | Pod count of that phase's deployment |
| `CL2_DISABLE_DAEMONSETS` | true | Removes DaemonSets from the mix |
| `CL2_PROMETHEUS_PVC_ENABLED` | false | ClusterLoader2's Prometheus uses no PersistentVolumeClaim; its default claims a GCE persistent-disk StorageClass |

## Environment

Stack the published result came from. A different stack is a different
scenario.

| | |
|---|---|
| Nodes | 47 — 3 control-plane, 42 worker, 2 monitoring |
| Control-plane / monitoring node | 64 cores, ~377 GiB memory, ~877 GiB disk |
| Worker node | 128 cores, ~1007 GiB memory, ~877 GiB disk |
| CPU | AMD EPYC 9004 series |
| Kubernetes | 1.34.2 |
| CNI | OVN-Kubernetes, ovn-k operator 1.2.0 / OVN core 25.09.1 (OVS 3.6.1), vendor build |
| Container runtime | containerd 1.7.28 |
| OS / kernel | Ubuntu 22.04.4 LTS / 5.15.0-113-generic |
| etcd | 3.6.5 |

Prerequisites: worker nodes labelled `node-role.kubernetes.io/worker`;
Prometheus scraping the API servers from the monitoring nodes, on whichever
port the API server is exposed on for your cluster;
kubelet max-pods on worker nodes at least `PODS_PER_NODE`.

`build: vendor` means a distribution-specific OVN-Kubernetes build, not an
upstream release artefact. Comparable to another vendor build of the same
version; read against an upstream build with care.

## How to replicate

1. Provision a cluster matching `topology`, or apply `sizing.derivation` to
   your own hardware: recompute `NODES_PER_NAMESPACE` from your schedulable,
   untainted node count and target namespace count, and confirm
   `PODS_PER_NODE` stays under your worker kubelet's max-pods setting so the
   peak pod count is schedulable.
2. Run the `prerequisites.baseline_validation` steps.
3. Check out `kubernetes/perf-tests` at
   `14525e2e2e08a236a2adb6f4f39740c586ed45fe`.
4. Run the invocation above.
5. Judge against the SLOs below.

Run on an otherwise idle cluster.

## How to judge the result

The scenario's `slo` block is the pass/fail contract. All four entries are
`severity: error` latency bars, where lower is better.

| Metric | Gate | Source |
|---|---|---|
| `post_p99_ms`, `delete_p99_ms` | ≤ 1000 ms | [SIG-Scalability mutating-API SLO](https://github.com/kubernetes/community/blob/main/sig-scalability/slos/slos.md) |
| `get_p99_ms` | ≤ 1000 ms | [SIG-Scalability read-only-API SLO](https://github.com/kubernetes/community/blob/main/sig-scalability/slos/slos.md), resource scope (the tighter of the two official bars — this scenario does not split GET latency by scope) |
| `pod_startup_p99_s` | ≤ 5 s | [SIG-Scalability pod-startup SLO](https://github.com/kubernetes/community/blob/main/sig-scalability/slos/slos.md) |

**Scheduler throughput is a supporting metric, not an `slo` entry.**
ClusterLoader2 gates it against `CL2_SCHEDULER_THROUGHPUT_THRESHOLD` only in
its scheduler-throughput phase, which runs on clusters of 100 or more
schedulable, untainted nodes. `scheduler_throughput_pods_per_s` is from the
load phase, which ClusterLoader2 does not gate.

**Zero APF 429 rejections is a supporting observation, not an `slo` entry.**
No per-size metric key carries it in this scenario's metrics contract, so it
is reported in the result's prose rather than judged mechanically.
