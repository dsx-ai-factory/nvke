<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# node-density-cni

Machine-readable definition: [scenario.yaml](scenario.yaml).

## What is tested

How long a pod takes to become *usable* — not merely scheduled — when worker nodes
are filled to a realistic density.

Each iteration creates a webserver Deployment, a Service in front of it, and a curl
Deployment whose startup probe succeeds only once it can reach that webserver
*through* the Service. Readiness is therefore gated on the network path being
programmed end to end: pod to Service to pod. The pod-ready latency this produces is
a time-to-connectivity figure, and it will be larger than the same cluster's
pod-only density number by however long CNI and Service programming takes under load.

That readiness gate waits on a chain of network work, not on scheduling alone. Before
the curl pod's first probe can succeed, the CNI has to allocate an IP and wire the pod's
network namespace, the Service's endpoints have to be updated and programmed into each
node's dataplane, and cluster DNS has to resolve the Service name — the probe targets
the Service by name, not by address. Pod-ready latency here moves with any link in that
chain, which makes this scenario an indicator of network-plumbing latency under creation
load rather than a property of the kubelet or the scheduler.

That puts the CNI in the result rather than in the background. The same workload, on the
same hardware, against a different CNI — or a different version of the same one — is
exercising a different dataplane, so the numbers are not comparable across them. That is
why `stack.cni` records the upstream commit the build is based on and the OVN and OVS
versions rather than just a name.

**This is not a maximum-density measurement, by design.** What the scenario exists to
produce is a repeatable time-to-connectivity at a known density, not a breaking point.
Pods-per-node is therefore chosen to sit below the kubelet's configured `maxPods` with
a buffer for ephemeral pods, and the run stops there rather than being driven until
something binds. Finding a ceiling would instead mean raising pods-per-node until the
kubelet refuses pods, a latency gate trips, or CNI and Service programming falls
behind.

## How it is tested

The iteration count is computed from live cluster state before the run and set in the
job configuration. The engine then creates objects at a fixed request rate and waits for every pod to pass its probe before
computing latency quantiles.

Pod-ready latency is measured from the pod's creation timestamp to its Ready
condition, both read from the Kubernetes API, where condition timestamps have
one-second granularity. The quantiles for the Ready family therefore land on whole
seconds and are meaningful to about +/- 1 s. Container-start latency comes from a
different source and does carry sub-second precision, as does Service-ready latency.

Two measurements are reported side by side. Pod-ready latency covers the whole path
including connectivity; Service-ready latency isolates how long the Service itself
took to become reachable. When pod-ready latency moves, the Service figure is the
first place to look before attributing the change to the scheduler or kubelet.

## Artifacts and scripts

| Path | What it is |
|------|------------|
| [scenario.yaml](scenario.yaml) | Machine-readable scenario definition |
| [engine/node-density-cni.yml](engine/node-density-cni.yml) | Fully-resolved job configuration, captured from the run |
| `engine/webserver-deployment.yml` | Webserver Deployment template |
| `engine/webserver-service.yml` | Service template |
| `engine/curl-deployment.yml` | Curl client template; its startup probe is the connectivity test |
| `engine/metrics.yml` | Time-series metrics profile named by the job configuration |
| `engine/metrics-report.yml` | Run-window summary metrics profile |
| [../../../dashboards/1.34/worker-nodes.json](../../../dashboards/1.34/worker-nodes.json) | Dashboard for the pod and node metrics |
| [../../../dashboards/1.34/control-plane.json](../../../dashboards/1.34/control-plane.json) | Dashboard for the apiserver, etcd and APF metrics |
| [../../../alerts/1.34/control-plane.yml](../../../alerts/1.34/control-plane.yml) | Alerting rules evaluated over the run window |

The engine configuration is the configuration that ran, taken from the run rather
than reconstructed afterwards. Two fields are generalised: the metrics output
directory, which carried the run's UUID, and the alerts entry, which carried a local
path and now names the shared alerts file above.

Two values in it are **not** portable and must be recomputed rather than reused:
`jobIterations`, which is the derived count for the cluster this ran on, and the
`nodeSelector` inputVars in the job configuration, which carry the node selector this
run used as node-affinity JSON. The templates only receive them.

The webserver image is also not upstream's: the cloud-bulldozer image binds `[::]`,
which fails on pods whose network namespace has IPv6 disabled, so it is replaced with
upstream nginx (alpine).

The curl image is `curl:latest`, unpinned, as it ran. Pod readiness depends on its
startup probe, so pin it to a digest for a like-for-like replication.

## Environment

The published result records the cluster this scenario was run on: 47 nodes — 3
control-plane, 42 worker, 2 monitoring — the workers each with 128 cores and roughly 1007 GiB of
memory, running Kubernetes 1.34.2 on containerd 1.7.28 and Ubuntu 22.04.4, with
OVN-Kubernetes as the CNI (a downstream build of upstream commit d5f19be, with OVN
25.09 and Open vSwitch 2.17) and no kube-proxy.

Worker kubelets were configured with a
[max-pods](https://kubernetes.io/docs/reference/config-api/kubelet-config.v1beta1/)
setting of 110, and the run targeted 105 pods per node — five held back per node for
ephemeral pods arriving and leaving during the run. That buffer is the point of the
number and should not be rounded away.

The node selector targeted the worker role. Excluding only control-plane nodes would
match 44 nodes, adding the two monitoring nodes.

## How to replicate

1. Confirm every node you intend to target is Ready and schedulable.
2. Record the kubelet max-pods setting on those nodes. Do not assume it is uniform
   across the cluster.
3. Choose a pods-per-node value below that setting, leaving a per-node buffer.
4. Set the node selector explicitly and confirm how many nodes it matches. Update the
   `nodeSelector` inputVars in the job configuration to match.
5. Count the pods already resident on those nodes, including DaemonSets and completed
   pods that have not been garbage collected.
6. Compute the iteration count: nodes multiplied by pods-per-node, minus that
   resident count, halved with integer division — the halving truncates, so an odd
   remainder loses one pod's worth of budget. Set `jobIterations` to it.
7. Confirm no pre-existing objects carry this workload's job label. Garbage
   collection selects on that label alone, with no run identifier, so it will delete
   a co-tenant's objects that happen to share it.
8. Run from `engine/` and keep the metrics output directory:

   ```bash
   cd engine
   PROMETHEUS_URL=<prometheus-url> kube-burner init --uuid=<run-uuid> --config=node-density-cni.yml
   ```

   Add `token:` to the `metricsEndpoints` entry if your Prometheus needs one.
   The log and `collected-metrics/` are written to `engine/`.

## How to judge the result

The verdict has two parts, and kube-burner's `passed` flag combines them: pod-ready
p99 against the scenario's `slo` entry, and no `severity: error` alerting rule firing
in the run window. The `slo` threshold is the configured one minute, longer than the
pod-only variant's fifteen seconds because readiness waits on connectivity here.

Three checks decide whether the number means anything:

- **The iteration count matches the cluster.** The result records
  `nodes_targeted`, `pods_per_node` and `existing_pods_at_derivation` so the count can
  be re-derived from the result alone. If it differs from `jobIterations`, the run
  measured a different density than it claims.
- **Coverage.** The pod latency measurement should produce two documents per
  iteration. Fewer means the quantiles describe a subset.
- **Peak Pending pods.** This should stay small and transient. A sustained Pending
  population means pods-per-node exceeded what the kubelets would accept, and the run
  is reporting scheduling backlog rather than density.

Read Service-ready latency alongside pod-ready latency. Because readiness depends on
the network here, a CNI or Service-programming regression presents as a pod latency
regression, and the Service figure is what separates the two.

Treat a fired alert as a question, not a finding, until the sample count behind it is
checked. Rules built on a rolling window are evaluated from the start of the run, so
early evaluations describe a window that reaches back before the run began; a rule
covering a rarely-used verb can report a latency from before the workload started.

One instance of this is known and recurring. The engine pre-loads this workload's
images with a DaemonSet and deletes that namespace as soon as the pulls finish, about
ten seconds before the job starts, which deletes the namespace's events in bulk. A
mutating-API rule with a five-minute lookback can catch that burst and report a
`DELETE/events` latency that belongs to the engine's own setup rather than to the
workload or the cluster. It is intermittent — it has fired on some runs of this
scenario and not others, and on other scenarios sharing the same engine. If a
`DELETE/events` alert appears, check whether its timestamp falls inside the first
minute of the job before drawing any conclusion. Setting `preLoadImages: false`
removes the burst, at the cost of moving image-pull time into the measured window.
Conversely, an alert backed by a large sample is worth taking seriously even when the
engine's own figure is lower — the two measure different populations over different
windows, and the engine's figure is the one the scenario is judged on.
