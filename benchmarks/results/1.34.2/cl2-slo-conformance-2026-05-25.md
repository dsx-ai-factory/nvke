---
type: result
scenario: ../../workloads/scalability/cl2-slo-conformance/scenario.yaml
workload: scalability
k8s_version: 1.34.2
date: 2026-05-25
authors: [NVIDIA NVKE Scale Team]
---

# ClusterLoader2 SLO conformance — k8s 1.34.2

Scenario: [cl2-slo-conformance](../../workloads/scalability/cl2-slo-conformance/README.md)

## TL;DR

A ClusterLoader2 run with the upstream `load` config (640 small + 14 medium
deployments plus jobs, peaking at 3,920 pods across 42 workers)
at 40 QPS met every
[SIG-Scalability SLO](https://github.com/kubernetes/community/blob/main/sig-scalability/slos/slos.md)
with room to spare: pod startup p99 2.54 s (SLO ≤ 5 s), all API verb p99
≈ 49–50 ms (SLO ≤ 1 s), and zero APF 429 rejections.

**Not a maximum-scale claim.** 42 workers is what this run tested, not how
far an NVKE cluster can go — every SLO passed with room to spare, which is
itself an argument for testing larger, not evidence that this is the top.
Apply the scenario's `sizing.derivation` to size the workload to a different
node count.

## Scope of this publication

- The run predates the ClusterLoader2 commit the scenario pins (2026-08-24),
  which is the commit `engine/overrides.yaml` was checked against.
- The object counts above are as recorded. On this topology the scenario's
  `engine/overrides.yaml` produces 448 small deployments.
- The 20 and 40 QPS levels were set outside `engine/overrides.yaml`, and the
  parameter used was not recorded. The overrides run at ClusterLoader2's
  default rates.
- Below 100 schedulable, untainted nodes ClusterLoader2 skips its
  scheduler-throughput phase and its 5 s pod-startup check. The 15 pods/s
  threshold was not evaluated; pod startup p99 is judged against the
  scenario's 5 s `slo`.

## Environment

| Field | Value |
|-------|-------|
| Nodes | 47 — 3 control-plane, 2 monitoring, 42 worker |
| Control-plane / monitoring node | 64 cores, ~377 GiB memory, ~877 GiB disk |
| Worker node | 128 cores, ~1007 GiB memory, ~877 GiB disk |

## Software configuration

| Component | Version |
|-----------|---------|
| Kubernetes | 1.34.2 |
| Containerd | 1.7.28 |
| OS image | Ubuntu 22.04.4 LTS |
| Kernel | 5.15.0-113-generic |
| CNI | OVN-Kubernetes, downstream build of upstream commit d5f19be |
| OVN | 25.09 |
| Open vSwitch | 2.17 |
| CoreDNS | 1.12.1 |
| etcd | 3.6.5 |
| kube-proxy mode | n/a — not installed |

## Results summary

| Size | Scenario | Duration (s) | Post p99 (ms) | Get p99 (ms) | Delete p99 (ms) | Pod startup p99 (s) | Incluster latency p99 (ms) | Dns lookup p99 (ms) | Peak apiserver reqs per sec | Scheduler throughput pods per s |
|------|----------|--------------|---------------|--------------|-----------------|---------------------|----------------------------|---------------------|-----------------------------|---------------------------------|
| mixed | S+M workload resources @ 40 QPS | 886 | 49.64 | 49.56 | 49.5 | 2.54 | 24.75 | 4.04 | 183 | 48 |

## Per-size detail

### mixed
```yaml
size: mixed
scenario: S+M workload resources @ 40 QPS
duration_s: 886
post_p99_ms: 49.64
get_p99_ms: 49.56
delete_p99_ms: 49.5
pod_startup_p99_s: 2.54
incluster_latency_p99_ms: 24.75
dns_lookup_p99_ms: 4.04
peak_apiserver_reqs_per_sec: 183
scheduler_throughput_pods_per_s: 48
qps: 40
max_apiserver_cpu_cores: 1.66
max_apiserver_memory_gib: 1.6
etcd_size_increase_mb: 161.8
```
**Observations:** The three official SIG-Scalability SLOs — mutating API,
read-only API and pod startup — were met with wide margins. Peak API
request rate 183/s across the 3 replicas; etcd grew 118.2 → 280 MB. No APF
429s; peak inflight (mutating + read-only) reached 34, well under one API
server's APF limit of 600.

## Observations & anomalies

- **All SIG-Scalability SLOs met:** mutating/read-only API p99 ≈ 49–50 ms
  (≤ 1 s SLO); pod startup p99 2.54 s (≤ 5 s SLO).
- **Scheduler throughput:** the 48 pods/s above is from the load phase,
  which ClusterLoader2 does not gate.
- **Comparison run at 20 QPS** (same workload, ~14:14 duration): peak
  184 API reqs/s, pod startup p99 2.490 s, scheduler throughput 47 pods/s,
  etcd 117 → 252 MB, peak inflight 35, in-cluster latency p99 24.80 ms, DNS
  p99 4.78 ms, max API-server CPU 64%. The two QPS levels produced
  near-identical SLI results, indicating ample headroom. This run is
  comparison context only — it does not carry a full per-size metrics block,
  because its POST/GET/DELETE API-latency figures were not captured to the
  same detail as the 40 QPS run above.
- **Single constraint found:** kube-controller-manager workqueue processing
  p99 reported as 10 s for several controllers (deployment, statefulset, GC,
  job, replicaset). This is an artifact of the histogram's top [10s, +inf]
  bucket, not a real stall — queues drained quickly. Tuning concurrent
  worker counts is therefore optional; if pursued it would require raising
  kube-controller-manager qps/burst and API-server inflight limits in
  concert.
- **Pod saturation:** peak 3,920 pods, about 85% of the 4,620-pod budget
  (42 workers × `PODS_PER_NODE` 110). Full saturation was not reached due to
  load-generation logic, not a cluster limit.
- **Conclusion:** this cluster shape had sufficient capacity for the
  workload tested.
