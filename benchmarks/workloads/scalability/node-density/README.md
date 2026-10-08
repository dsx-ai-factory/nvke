<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# node-density

Machine-readable definition: [scenario.yaml](scenario.yaml).

## What is tested

How long a pod takes to reach Ready when worker nodes are filled to a realistic
density.

Each iteration creates one `registry.k8s.io/pause:3.1` pod with no Service, probe or
application, so pod-ready latency covers only scheduling, kubelet admission, sandbox
creation from a cached image and the Ready report. It shows whether the scheduler and
kubelet keep up when several thousand pods arrive at once.

**This is not a maximum-density measurement, by design.** The scenario produces a
repeatable pod-ready latency at a known density, not a breaking point: pods-per-node
sits below the kubelet's `maxPods` with a buffer, and the run stops there.

## How it is tested

The iteration count is computed from live cluster state before the run and set
in the job configuration — see [How to replicate](#how-to-replicate). The engine then creates pods at a fixed request
rate and waits for every one to report Ready before computing latency quantiles.

Pod-ready latency is measured from the pod's creation timestamp to its Ready
condition, both read from the Kubernetes API, where condition timestamps have
one-second granularity. The Ready-family quantiles therefore land on whole seconds
and are meaningful to about +/- 1 s. At the sub-two-second latencies this workload
produces, that quantization is a large fraction of the value — container-start
latency, which comes from a different source and carries sub-second precision, is the
more precise signal and should be read alongside it.

Pod-scheduled latency is less precise than its values suggest. The engine times it
from the pod's `Scheduled` event, at microsecond precision, when it can see that event,
and otherwise from the `PodScheduled` condition, which is truncated to the second.
Either is subtracted from the creation timestamp, which is also truncated to the
second. A reading of 0 ms is therefore a resolution floor — the condition and creation
timestamps truncated to the same second, with the actual latency anywhere below one —
and event-timed readings run high by up to 1 s. Neither gives a precise scheduling
figure.

Peak Pending pods is the maximum Pending value of the `podStatusCount` series that
`engine/metrics.yml` collects (`sum(kube_pod_status_phase{}) by (phase)`, all
namespaces) over the run window. The series is coarsely sampled, so the figure is a
lower bound on the true peak.

Images are pre-loaded before the job starts, so image pull time is deliberately
outside the measured window. This is what makes the figure a floor rather than a
first-run number; a cluster pulling `pause` for the first time would see something
larger, and that difference is image distribution, not pod startup.

## Artifacts and scripts

| Path | What it is |
|------|------------|
| [scenario.yaml](scenario.yaml) | Machine-readable scenario definition |
| [engine/node-density.yml](engine/node-density.yml) | Fully-resolved job configuration, captured from the run |
| `engine/pod.yml` | The pause pod template — the only object this workload creates |
| `engine/metrics.yml` | Time-series metrics profile named by the job configuration |
| `engine/metrics-report.yml` | Run-window summary metrics profile — the `max-*` documents the result quotes |
| [../../../dashboards/1.34/worker-nodes.json](../../../dashboards/1.34/worker-nodes.json) | Dashboard for the pod and node metrics |
| [../../../dashboards/1.34/control-plane.json](../../../dashboards/1.34/control-plane.json) | Dashboard for the apiserver, etcd and APF metrics |
| [../../../alerts/1.34/control-plane.yml](../../../alerts/1.34/control-plane.yml) | Alerting rules evaluated over the run window |

The engine configuration is the configuration that ran, taken from the run rather
than reconstructed afterwards. Two fields are generalised: the metrics output
directory, which carried the run's UUID, and the alerts entry, which carried a local
path and now names the shared alerts file above.

Two values in it are **not** portable and must be recomputed rather than reused:
`jobIterations`, which is the derived count for the cluster this ran on, and the
`nodeSelector` inputVar, which is the node selector this run used already compiled to
node-affinity JSON. Note that the node affinity is not written into `pod.yml` — the
template receives it as an input variable, so it is the job configuration that must
be edited, not the pod template.

## Environment

The published result records the cluster this scenario was run on: 47 nodes — 3
control-plane, 42 worker, 2 monitoring — the workers each with 128 cores and roughly 1007 GiB of
memory, running Kubernetes 1.34.2 on containerd 1.7.28 and Ubuntu 22.04.4, with
OVN-Kubernetes as the CNI (a downstream build of upstream commit d5f19be, with OVN
25.09 and Open vSwitch 2.17) and no kube-proxy.

The run targeted 105 pods per node against a kubelet
[max-pods](https://kubernetes.io/docs/reference/config-api/kubelet-config.v1beta1/)
setting of 110, holding five back per node for ephemeral pods arriving and leaving
during the run. That buffer is the point of the number and should not be rounded
away.

The node selector targeted the worker role. Excluding only control-plane nodes would
match 44 nodes, adding the two monitoring nodes.

## How to replicate

1. Confirm every node you intend to target is Ready and schedulable.
2. Read the kubelet max-pods setting on those nodes. Do not start from 245, the
   pods-per-node default in the tooling this workload is commonly driven by — it is
   far above a stock kubelet's 110.
3. Choose a pods-per-node value below that setting, leaving a per-node buffer.
4. Set the node selector explicitly and confirm how many nodes it matches. Update the
   `nodeSelector` inputVar in the job configuration to match.
5. Count the pods already resident on those nodes, including DaemonSets and completed
   pods that have not been garbage collected.
6. Compute the iteration count: nodes multiplied by pods-per-node, minus that
   resident count. Set `jobIterations` to the result.
7. Confirm no pre-existing objects carry this workload's job label. Garbage
   collection selects on that label alone, with no run identifier, so it will delete
   a co-tenant's objects that happen to share it.
8. Run from `engine/` and keep the metrics output directory:

   ```bash
   cd engine
   PROMETHEUS_URL=<prometheus-url> kube-burner init --uuid=<run-uuid> --config=node-density.yml
   ```

   Add `token:` to the `metricsEndpoints` entry if your Prometheus needs one.
   The log and `collected-metrics/` are written to `engine/`.

### Writing the node selector

The `nodeSelector` inputVar is the pod's required node affinity, written as JSON. This
run targeted workers only:

```json
{"nodeSelectorTerms":[{"matchExpressions":[{"key":"node-role.kubernetes.io/worker","operator":"Exists"}]}]}
```

Expressions within a term are ANDed and separate terms are ORed. Count nodes with the
same selection when computing the iteration count, or the count and the placement will
disagree.

## How to judge the result

The verdict has two parts, and kube-burner's `passed` flag combines them: pod-ready
p99 against the scenario's `slo` entry, and no `severity: error` alerting rule firing
in the run window. The `slo` entry is the pod-ready p99 bar this scenario runs with,
carried as `threshold: 15s` in the engine configuration and evaluated once, after the
run, against the computed p99 — not a per-pod timeout.

Three checks decide whether the number means anything:

- **The iteration count matches the cluster.** The result records `nodes_targeted`,
  `pods_per_node` and `existing_pods_at_derivation` so the calculation can be repeated
  from the result alone. If it differs from `jobIterations`, the run measured a
  different density than it claims.
- **Coverage.** The pod latency measurement should produce exactly one document per
  iteration. Fewer means the quantiles describe a subset.
- **Peak Pending pods.** This should stay small relative to the pod count, and every
  pod should reach Ready. A sustained or growing Pending population means
  pods-per-node exceeded what the kubelets would accept, and the run is reporting
  scheduling backlog rather than density. Expect it to scale with creation rate, not
  just with cluster size — the same cluster driven at a higher QPS carries a larger
  in-flight front without anything being wrong.

Because the Ready quantiles are quantized to whole seconds and the values here are
small, do not read a one-second difference between two runs as a regression. Compare
container-start latency, which carries sub-second precision, before concluding
anything from a change of that size.

Treat a fired alert as a question, not a finding, until the sample count behind it is
checked. Rules built on a rolling window are evaluated from the start of the run, so
early evaluations describe a window that reaches back before the run began; a rule
covering a rarely-used verb can report a latency from before the workload started.

One instance of this is known and recurring across scenarios sharing this engine. The
engine pre-loads images with a DaemonSet and deletes that namespace as soon as the
pulls finish, shortly before the job starts, which deletes the namespace's events in
bulk. A mutating-API rule with a five-minute lookback can catch that burst and report
a `DELETE/events` latency that belongs to the engine's own setup rather than to the
workload or the cluster. If a `DELETE/events` alert appears, check whether its
timestamp falls inside the first minute of the job before drawing any conclusion.
Setting `preLoadImages: false` removes the burst, at the cost of moving image-pull
time into the measured window — which for this scenario would change what the number
means, not merely how large it is.
