---
type: result
scenario: ../../workloads/scalability/node-density-cni/scenario.yaml
workload: scalability
k8s_version: 1.34.2
date: 2026-09-18
authors: [NVIDIA NKE Scale Team]
---

# node-density-cni — k8s 1.34.2

Scenario: [node-density-cni](../../workloads/scalability/node-density-cni/README.md)

## TL;DR

2,060 iterations filled 42 workers to an average of 105 pods per node, creating 4,120 pods in
996 s. Every curl pod became Ready only after reaching a webserver through its Service, so
the headline figure is time-to-connectivity: **pod-ready p99 of 3 s** against a
1 minute p99 bar. Peak Pending was 8 pods and latency coverage was complete. The
calculated iteration count equals the observed iteration count.

The engine reported **`passed: true`** with no execution errors. Measured directly, the
slowest pod this workload created reached Ready in 4 s.

| | Outcome |
|---|---|
| Pod-ready p99 vs the 1 m bar | **Met** — 3 s |
| Latency measurement coverage | **Complete** — 4,120 of 4,120 pods |
| Scheduling headroom | **Held** — peak Pending 8 |
| Slowest pod this workload created | **4 s** |
| Prometheus alerting | **`passed: true`** — no `error` rule fired |

**Not a maximum-density claim.** See
[What is tested](../../workloads/scalability/node-density-cni/README.md#what-is-tested)
in the scenario README.

## Environment

| Field | Value |
|-------|-------|
| Nodes | 47 — 3 control-plane, 2 monitoring, 42 worker |
| Worker node | 128 cores, ~1007 GiB memory |
| Worker kubelet max-pods | 110 |
| Nodes targeted | 42, selected explicitly by worker role |

## Software configuration

| Component | Version |
|-----------|---------|
| Kubernetes | 1.34.2 |
| Containerd | 1.7.28 |
| OS image | Ubuntu 22.04.4 LTS |
| Kernel | 5.15.0-113-generic |
| CNI | OVN-Kubernetes, Helm chart 1.4.0 (app 2026-02.0) |
| OVN | 25.09.1 |
| Open vSwitch | 3.6.1 |
| CoreDNS | 1.12.1 |
| etcd | 3.6.5 |
| kube-proxy mode | n/a — not installed |
| Engine | kube-burner 2.5.0 |

## Results summary

| Size | Scenario | Duration (s) | Pod ready p50 (ms) | Pod ready p95 (ms) | Pod ready p99 (ms) | Pod scheduled p99 (ms) | Containers ready p99 (ms) | Svc ready p99 (ms) |
|------|----------|--------------|--------------------|--------------------|--------------------|------------------------|---------------------------|--------------------|
| L | 105 pods/node across 42 workers, 2060 iterations | 996 | 2000 | 3000 | 3000 | 945 | 3000 | 1310.4 |

## Per-size detail

### L
```yaml
size: L
scenario: 105 pods/node across 42 workers, 2060 iterations
duration_s: 996
pod_ready_p50_ms: 2000
pod_ready_p95_ms: 3000
pod_ready_p99_ms: 3000
pod_scheduled_p99_ms: 945
containers_ready_p99_ms: 3000
svc_ready_p99_ms: 1310.4
pods_per_node: 105
nodes_targeted: 42
existing_pods_at_derivation: 290
iterations: 2060
pods_created: 4120
qps: 20
burst: 20
pod_ready_threshold_s: 60
max_apiserver_cpu_cores: 1.64
max_apiserver_memory_gib: 11.75
max_etcd_cpu_cores: 0.4
max_etcd_memory_mib: 558
max_readonly_inflight: 6
max_mutating_inflight: 8
etcd_db_in_use_mib: 276
etcd_wal_fsync_p99_ms: 0.99
etcd_backend_commit_p99_ms: 1.98
peak_pending_pods: 8
```

**Observations:** The calculated iteration count equals the observed one — 42 workers
x 105 pods = 4,410, minus the 290 pods already resident, is 4,120; halved with integer
division that is the 2,060 iterations the run used. The halving truncates rather than
rounds, so an odd remainder would leave one pod's worth of budget unused. The pod
latency measurement produced 4,120 documents, two per iteration, so the quantiles
cover the whole run. Control-plane headroom was ample throughout: read-only inflight
peaked at 6 and mutating at 8 across all three API servers, well under one server's
APF limit of 600, and etcd disk latency stayed
an order of magnitude inside conventional bars. Because the derivation subtracts pods
already resident, the iteration count is a property of the cluster at the moment the
run started, not a fixed number for this scenario.

## Scope of this publication

This result covers one density point on one cluster: 105 pods per node, below that
cluster's max-pods setting of 110.

## Gate outcome

The engine reported `passed: true` and `executionErrors` was empty. The flag combines
two things, and both were satisfied: the configured pod-ready p99 bar of 1 minute
against a measured 3 s, and no `severity: error` alerting rule firing in the run
window.

That bar is not a timeout. The engine computes the pod-ready quantiles when the run
finishes and compares the p99 against it — nothing is cancelled, and no pod is affected
by the comparison. The only per-pod deadline in this workload is the curl container's
startup probe, which allows 600 consecutive one-second failures before the kubelet
restarts it.

## Reading the numbers

The Ready-family quantiles are quantized to whole seconds — p50, p95 and p99 are exact
multiples of 1000 ms, while container-start latency reports 1,625 ms. The Ready values
derive from pod condition timestamps, which are second-granularity in the Kubernetes
API, so the true p99 is 3 s +/- 1 s. Service-ready latency comes from a different
measurement and does carry sub-second precision.
