---
type: result
scenario: ../../workloads/apiserver/crd-cr-characterization/scenario.yaml
workload: apiserver
k8s_version: 1.34.2
date: 2026-04-17
authors: [NVIDIA NKE Scale Team]
---

# CRD / CR API-server characterization

Scenario: [crd-cr-characterization](../../workloads/apiserver/crd-cr-characterization/README.md)

## TL;DR

Burst create and delete of CRDs and custom resources across five sizes produced
**zero API-server rejections** at the default 200 mutating / 400 read-only
inflight configuration, up to 500 CRDs and 100,000 custom resources.

Schema complexity, not object count, drives POST latency. Simple schemas stayed
between 20 and 42 ms at p99 even at the largest sizes; nested schemas reached
788-813 ms at a tenth to a twentieth of the object count. etcd growth tracked total bytes
written, peaking at +411 MB for the 100,000-resource size.

Every gate passed, but one barely: DELETE p99 reached 990 ms at size S against a
1000 ms bar.

## Scope of this publication

All five sizes ran and all five are reported. Limits on reading them:

- **This is a single cluster.** Absolute latencies are a property of the
  control-plane hardware. What transfers is the relationship the sizes expose —
  simple against complex schemas, and how growth scales with object count.
- **The kube-burner version is determined, not pinned.** The run installed the
  then-current release; v2.6.1 held that position on the run date.
- **POST and DELETE p99, peak request rate and etcd growth were read from the
  cluster's Prometheus over each phase.** The queries used were not kept.
  `engine/metrics-profile.yaml` defines them for a replication, so its values may
  not match the published method exactly.

Custom resource update churn and watch/list scalability were not tested.

## Environment

| Field | Value |
|---|---|
| Nodes | 7 — 3 control plane, 2 workers, 2 monitoring |
| Per-node CPU | 128 cores |
| Per-node memory | ~1007 GiB |
| Per-node disk | ~877 GiB |
| APF inflight | 200 mutating / 400 read-only (default) |
| Client rate, CRD pass | 20 qps / 20 burst (XS: 10 / 10) |
| Client rate, CR pass | 50 qps / 50 burst per CRD, CRDs in parallel |

| Component | Version |
|---|---|
| Kubernetes | 1.34.2 |
| etcd | 3.6.5 |
| Containerd | 1.7.28 |
| CNI | OVN-Kubernetes, downstream build of upstream commit d5f19be |
| OVN | 25.09 |
| Open vSwitch | 2.17 |
| CoreDNS | 1.12.1 |
| OS image | Ubuntu 22.04.4 LTS |
| Kernel | 5.15.0-113-generic |
| kube-proxy | not installed |

## Results summary

| Size | Shape | Schema | Duration (s) | POST p99 (ms) | DELETE p99 (ms) | Peak reqs/s | etcd growth (MB) | 429s |
|---|---|---|---|---|---|---|---|---|
| XS | 100 CRDs × 100 CRs | simple | 17.44 | 42.1 | 21.6 | 9.72 | 49.4 | 0 |
| S | 500 CRDs × 100 CRs | simple | 79.47 | 24.4 | 990 | 20.7 | 218 | 0 |
| M | 50 CRDs × 100 CRs | complex | 11.77 | 788 | 22.9 | 5.8 | 41 | 0 |
| L | 100 CRDs × 1000 CRs | simple | 93.46 | 19.9 | 24 | 125 | 411 | 0 |
| XL | 100 CRDs × 100 CRs | complex | 21.46 | 813 | 32 | 6.95 | 82 | 0 |

## Acceptance gate

| Metric | Bar | XS | S | M | L | XL |
|---|---|---|---|---|---|---|
| `post_p99_ms` | ≤ 1000 | 42.1 ✅ | 24.4 ✅ | 788 ✅ | 19.9 ✅ | 813 ✅ |
| `delete_p99_ms` | ≤ 1000 | 21.6 ✅ | **990 ✅** | 22.9 ✅ | 24 ✅ | 32 ✅ |
| `429s_count` | 0 | 0 ✅ | 0 ✅ | 0 ✅ | 0 ✅ | 0 ✅ |

**All gates pass.** DELETE at size S is marked: it passes with 1% margin.

## Observations

**XS ran its CRD pass at half the client rate of the other sizes** — 10 qps
against 20 — so an XS-to-S comparison holds one more variable than any other
pair. It does not affect the simple-against-complex comparison, which is drawn
between sizes that shared a CRD-pass rate.

**Schema complexity dominates POST latency.** M created 5,000 resources and
reached 788 ms at p99; L created 100,000 and reached 19.9 ms. Twenty times the
objects, one fortieth the latency — the difference is the schema. Validating
and serializing a nested resource costs the API server far more per object than
storing many flat ones, so sizing a cluster on object count alone will
mispredict.

**DELETE at size S is the outlier and is not explained by volume.** 990 ms
against 21-32 ms everywhere else, at 50,000 resources — half what L wrote,
where DELETE stayed at 24 ms. The distinguishing feature of S is CRD count: 500
against 50-100 elsewhere.

**No APF rejections anywhere**, including 500 CRDs and 100,000 resources. The
default 200/400 inflight configuration was not the limit at any size tested.
This says nothing about where the limit is — no size reached it.

`peak_reqs_per_sec` is bounded by the client rate limits — 10-20 qps for the
CRD pass and 50 per CR invocation — not by what the server would accept.

**etcd growth tracked bytes written, not object count**: +411 MB for 100,000
simple resources, +82 MB for 10,000 complex ones — about 4.1 KB and 8.2 KB per
stored object.

The complex figure sits inside its 6-10 KB class. The simple one does not: 4.1
KB against a 2-3 KB target. etcd stores more than the resource body — the key,
revision history and index overhead all count — so per-object database cost
runs above the serialized object size, and the gap is proportionally larger for
smaller objects. Size a cluster from measured database growth rather than from
the resource sizes alone.

## Reproducing this

See the scenario's [README](../../workloads/apiserver/crd-cr-characterization/README.md).
Each size needs a clean baseline; the growth figures are deltas across the create
window.
