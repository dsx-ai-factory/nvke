<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# OVN-Kubernetes data-plane throughput

Definition: [scenario.yaml](scenario.yaml)
Result: [k8s-netperf-throughput-2026-05-26.md](../../../results/1.34.2/k8s-netperf-throughput-2026-05-26.md)

## What is tested

Throughput and latency overhead of running pod traffic over an OVN-Kubernetes
overlay, measured against a host-network reference on the same hardware.

**This is not a scale-limit test.** The topology and concurrency levels below
are what this run happened to measure on this cluster, not a ceiling on what
OVN-Kubernetes or any given hardware can do. `sizing.derivation` exists so a
reader can size this workload to their own cluster; a different node count or
NIC speed changes the numbers, not what the scenario is capable of measuring.

Four paths carry the same traffic, so differences between them are attributable
to the network layer rather than the load generator:

| Path | Exercises |
|---|---|
| **host-to-host** | Node host network. CNI bypassed — the NIC and kernel ceiling, and the reference every other row is read against. |
| **pod-to-pod same-node** | veth into the OVS integration bridge and back out on one node. No inter-node hop, no Geneve tunnel. |
| **pod-to-pod cross-node** | The representative pod data path: Geneve overlay between two nodes. |
| **pod-to-service** | Cross-node plus a ClusterIP hop, isolating the Service and conntrack layer. |

Four concurrency levels per path: S/M/L/XL = 4/8/16/32 concurrent flows.
`UDP_STREAM` runs at 1/4/16/32, `TCP_RR` runs with `burst: 2` (netperf `-b 2`),
and pod-to-service stops at L.

Measures TCP stream throughput as a fraction of line rate, TCP efficiency under
sustained load, UDP loss on an overdriven path, and TCP request-response rate
and tail latency.

## How it is tested

Three profiles per size, per path:

| Profile | Reports |
|---|---|
| `TCP_STREAM` | Throughput (Gb/s, % of line rate) and TCP efficiency per RFC 6349 |
| `UDP_STREAM` | Throughput and packet loss. No flow control, so loss shows what an overdriven path discards. |
| `TCP_RR` | Request/response rate (op/s) and p99 round-trip latency |

60 s per test, 3 samples, means reported.

Message sizes:

| Profile | Size | Why |
|---|---|---|
| `TCP_STREAM` | 8192 B at every size and path | Largest power of two fitting a single pod jumbo frame at MTU 8940. Fixed so no cell pays a fragmentation penalty another avoids — this is what makes the matrix comparable. |
| `UDP_STREAM` | 8192 B at S/M, 8800 B at L/XL | Just under MTU, so loss reflects congestion rather than fragmentation |
| `TCP_RR` | 256 B at S/M, 1024 B at L/XL | Small enough to stay latency-bound rather than bandwidth-bound |

`pN` is concurrent netperf flows inside one client pod, not pod count. Each run
uses single-replica Deployments and tears down the namespace afterwards.

## Artifacts and scripts

| Artifact | Role |
|---|---|
| [engine/netperf.yml](engine/netperf.yml) | Test matrix for host-to-host, same-node and cross-node. Engine-native — hand to upstream unchanged. |
| [engine/netperf-service.yml](engine/netperf-service.yml) | The same S–L tests with `service: true`, for pod-to-service. |
| [scenario.yaml](scenario.yaml) | Topology, stack, sizing rule, metrics contract, SLOs |
| [networking-ovn.json](../../../dashboards/1.34/networking-ovn.json) | Grafana dashboard, shared: OVN-controller flow-installation and CNI-request metrics. 12 panels, inline PromQL — no recording rules required. Lives in `benchmarks/dashboards/1.34/` because it is a subsystem view, not specific to this workload. |

No `alerts.yml` — see
[No alerting rules ship with this scenario](#no-alerting-rules-ship-with-this-scenario)
for why.

Upstream: [k8s-netperf](https://github.com/cloud-bulldozer/k8s-netperf) `v0.1.41`

One run per path:

```bash
k8s-netperf --config engine/netperf.yml --hostNet        # host-to-host
k8s-netperf --config engine/netperf.yml --local          # pod-to-pod same-node
k8s-netperf --config engine/netperf.yml                  # pod-to-pod cross-node
k8s-netperf --config engine/netperf-service.yml          # pod-to-service
```

**Pod-to-service is not a flag.** It is selected per test by `service: true`, so
it has its own config: the S, M and L tests of `netperf.yml`, each with
`service: true`, run with no placement flag. Mixing them into `netperf.yml`
would also run them under `--local`, producing same-node Service cells this
scenario does not measure.

Flag names have changed between k8s-netperf releases — verify against upstream
docs for the version installed. No wrapper script is shipped: the commands above
are the whole procedure.

### Load drivers

`--netperf` is the default. Each driver supports a different subset of profiles,
enforced upstream by `IsTestSupported()`:

| Driver | Flag | Supports | Excludes |
|---|---|---|---|
| netperf | `--netperf` (default) | All profiles | — |
| iperf3 | `--iperf` | `TCP_STREAM`, `UDP_STREAM` | Everything else — the guard matches on `STREAM` |
| uperf | `--uperf` | Everything except `TCP_CRR` | `TCP_CRR` only |

This scenario uses the default netperf driver. `TCP_CRR` is netperf-only, so a
scenario needing connect-rate figures cannot use iperf3 or uperf.

### Engine image

k8s-netperf hardcodes the workload container as
`quay.io/cloud-bulldozer/k8s-netperf:latest` with `imagePullPolicy: Always`.

Two consequences worth knowing before treating a run as reproducible:

- **The workload image is not pinned.** `engine_version` pins the orchestrator
  binary, not the netperf build inside the pods. Two runs of this scenario weeks
  apart can execute different netperf code.
- **Every pod start pulls.** An air-gapped or registry-restricted cluster needs
  the image mirrored, and the constant is compiled in rather than configurable —
  changing it means patching upstream.

`networking-ovn.json` needs a Prometheus scraping OVN-Kubernetes. Its queries filter
on an optional `k8s_cluster_name` label and match everything when that label is absent,
so a single-cluster Prometheus needs no relabeling.

Validated against a live OVN-Kubernetes cluster (Kubernetes 1.34.2) on a
Prometheus that carries no `k8s_cluster_name` label: all 19 metrics present, and the
latency panels return values under pod churn — p99 CNI request 0.99 s, p99 pod
creation 1.59 s, p99 LSP-to-port-binding 0.0099 s.

### No alerting rules ship with this scenario

**The pass/fail metrics cannot be alerted on.** All three entries in this
scenario's `slo` block — `tcp_efficiency_pct`, `udp_loss_pct` and
`execution_completeness_pct` — are computed by the netperf client from its
own run output. They never reach Prometheus, so no Prometheus rule can
evaluate them. Judging a run against them means reading the result, not
watching a dashboard.

**The metrics that are in Prometheus have no agreed thresholds.** The
OVN-Kubernetes signals `networking-ovn.json` plots — CNI request duration,
pod-creation latency, LSP-to-port-binding, flow-installation — are the right
things to watch while a run is in flight, and the validation figures above
show what they look like on one healthy cluster. But one cluster's numbers
are not a threshold, and picking a bar from a single observation would
manufacture a false one. Those values are hardware- and CNI-version-specific
in a way the SLOs above are not.

### Verified against upstream

The config vocabulary and the host-to-host, same-node and cross-node invocations
above were run against a live OVN-Kubernetes cluster using only the upstream
`k8s-netperf` v0.1.41 release binary — no wrapper, no orchestrator:

- The config parses, and `service: true` selects the ClusterIP path. The results
  table reports it in the `SERVICE` column.
- `--local` places both ends on one node — the results table reports
  `SAME NODE true`.
- `--clean=true` removes the `netperf` namespace afterwards.

The cluster used was a 10-worker simulation at pod MTU 1440, so its throughput
figures are not comparable to the published result and are not reproduced here.
The message size was recomputed to 1024 for that MTU, per `sizing.derivation` —
which is the rule doing its job rather than an exception to it.

`burst: 2` and `netperf-service.yml` were not part of that check. Both configs
load in the pinned release's own config parser, but neither has been run.

## Environment

Stack the published result came from. A different stack is a different scenario.

| | |
|---|---|
| Nodes | 11 — 3 control-plane, 7 worker, 1 monitoring |
| Worker CPU | AMD EPYC 9334, 64 logical cores (x86_64) |
| Worker memory | ~377 GiB |
| Worker NIC | 2 × 200 Gb/s = 400 Gb/s aggregate |
| Host / pod MTU | 9000 / 8940 |
| Kubernetes | 1.34.2 |
| CNI | OVN-Kubernetes 25.09.1 (OVS 3.6.1), vendor build |
| Container runtime | containerd 1.7.28 |
| OS / kernel | Ubuntu 22.04.4 LTS / 5.15.0-113-generic |

Prerequisites: worker nodes labelled `node-role.kubernetes.io/worker`;
`netperf` namespace creatable and deletable.

`build: vendor` means a distribution-specific OVN-Kubernetes build, not an
upstream release artefact. Comparable to another vendor build of the same
version; read against an upstream build with care.

## How to replicate

1. Provision a cluster matching `topology`, or apply `sizing.derivation` to your
   own hardware. Concurrency is the parameter to adjust, not node count: pick
   the largest power of two at which added concurrency stops raising throughput.
   Past that you are measuring the load generator.
2. If your pod MTU is not 8940, recompute the `TCP_STREAM` anchor as the largest
   power of two below it, and the UDP size as just under it, in both
   `engine/netperf.yml` and `engine/netperf-service.yml`. Skipping this makes
   your numbers incomparable — some cells fragment, others do not.
3. Run the `prerequisites.baseline_validation` steps.
4. Install upstream k8s-netperf at the pinned version.
5. Run all four invocations. Keep `netperf-service.yml` in step with
   `netperf.yml`: changing the matrix between paths destroys the comparison.
6. Judge against the SLOs below.

Roughly 30–60 minutes per path on comparable hardware. Paths are independent and
may run in any order, but must run on the same cluster in the same state — the
delta between them is the measurement.

## How to judge the result

The scenario's `slo` block is the pass/fail contract. All three are
`severity: error`:

| Metric | Gate | Source |
|---|---|---|
| `tcp_efficiency_pct` | ≥ 99% | Scenario gate; efficiency as defined in RFC 6349 §4.2 |
| `udp_loss_pct` | ≤ 5% | Scenario gate |
| `execution_completeness_pct` | = 100% | Every size × path cell the configs run must produce samples |

**Read throughput as a fraction of line rate.** An absolute number means nothing
against different hardware. What carries across clusters is the host-to-host
reference against the cross-node overlay path on the *same* cluster — that delta
is the encapsulation overhead, and it is what this scenario measures.

**Expect the UDP gate to fail on overlay paths at high concurrency.** It did
here. UDP has no flow control, so at saturation the overlay drops traffic the
host-network path does not. Reported as a FAIL rather than tuned away.

**A same-node result lower than cross-node is not a bug.** Traffic that never
leaves the node still hairpins veth → OVS → OVN pipeline → OVS → veth, paying
switching overhead twice on one node's CPUs while both endpoints compete for the
same cores. See the result's observations for per-core evidence.
