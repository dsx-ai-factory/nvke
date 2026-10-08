<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# cluster-density

Machine-readable definition: [scenario.yaml](scenario.yaml).

## What is tested

How long a pod takes to become Ready while the control plane is absorbing a broad
mix of object creations — not pods alone.

Each iteration creates its own namespace containing five Services, ten Secrets, ten
ConfigMaps, three NetworkPolicies, three server Deployments of two replicas each and
two client Deployments of two replicas each: ten pods, and roughly thirty objects,
per iteration. Running many iterations concurrently puts the API server, etcd and
the Service and EndpointSlice path under load together, which is what separates this
from a pod-only density test.

The headline number is pod-ready p99. Service-ready latency is measured alongside it
because five Services per iteration is enough to make the Service path a plausible
contributor to pod readiness.

NetworkPolicy programming latency is **not** measured, despite three NetworkPolicies
per iteration. The engine's measurement for it requires a proxy pod and ping targets
that this workload does not create; enabled without them it writes empty documents
rather than reporting an error. A dedicated network-policy scenario would be the
right place for that measurement.

**This is not a maximum-density measurement, by design.** What the scenario exists to
produce is a repeatable latency under a known object load, not a breaking point. The
iteration count is therefore chosen to fit inside the cluster's pod budget with
headroom left over, and the run stops there rather than being driven until something
binds. Finding a ceiling would instead mean raising iterations until scheduling
backlogs, a latency gate trips, or an etcd or API server limit is reached. Read
`sizing.observed` as the load a run applied; a larger cluster is a different
`sizing.observed`, not a different capability.

## How it is tested

The engine renders [engine/cluster-density-v2.yml](engine/cluster-density-v2.yml)
and creates objects at a fixed request rate, then waits for every object to settle
before computing latency quantiles from the pod and Service watch streams.

Pod-ready latency is measured from the pod's creation timestamp to its Ready
condition. Both come from the Kubernetes API, where condition timestamps have
one-second granularity — so the quantiles this workload reports for the Ready family
land on whole seconds, and are meaningful to about +/- 1 s. Container-start latency
comes from a different source and does carry sub-second precision. This matters when
choosing a threshold: a bar of a few seconds is not measurable here, and a bar in the
tens of seconds is.

Sizing is the one thing a reader must decide. Iteration count is supplied directly
rather than derived from the cluster, so nothing stops a value whose pod total
exceeds what the cluster can schedule. Past that point the excess pods stay Pending
and the run measures scheduling backlog while still producing plausible-looking
latency quantiles. `sizing.derivation` in the scenario states the budget calculation;
peak Pending pod count is reported in the result as the check that it held.

## Artifacts and scripts

| Path | What it is |
|------|------------|
| [scenario.yaml](scenario.yaml) | Machine-readable scenario definition |
| [engine/cluster-density-v2.yml](engine/cluster-density-v2.yml) | Fully-resolved job configuration, captured from the run |
| `engine/service.yml`, `engine/secret.yml`, `engine/configmap.yml` | Per-iteration object templates |
| `engine/np-deny-all.yml`, `engine/np-allow-from-clients.yml`, `engine/np-allow-from-ingress.yml` | NetworkPolicy templates |
| `engine/deployment-server.yml`, `engine/deployment-client.yml` | Workload Deployment templates |
| `engine/metrics.yml` | Time-series metrics profile named by the job configuration |
| `engine/metrics-report.yml` | Run-window summary metrics profile |
| [../../../dashboards/1.34/control-plane.json](../../../dashboards/1.34/control-plane.json) | Dashboard for the metrics this scenario reports |
| [../../../alerts/1.34/control-plane.yml](../../../alerts/1.34/control-plane.yml) | Alerting rules this scenario is watched with — see the note below |

The engine configuration is the configuration that ran, taken from the run rather
than reconstructed afterwards. Two fields are generalised: the metrics output directory,
which carried the run's UUID, and the alerts entry, which carried an absolute local path
and now names the shared alerts file above.

Three things about it are worth knowing before replicating:

- **The server image is not upstream's.** Images that bind `[::]` fail on pods whose
  network namespace has IPv6 disabled, so it is replaced with upstream nginx (alpine)
  listening on `0.0.0.0:80`.
- **The client image is `curl:latest`**, unpinned, as it ran. Pin it to a digest for a
  reproducible run; an unpinned tag means a later replication is not necessarily
  running the same binary.
- **Node affinity is the `nodeSelector` inputVars** in the job configuration; the
  Deployment templates only receive it.

The object templates otherwise ship exactly as they ran and use only engine-native
template variables and the job configuration's inputVars.

## Environment

The published result records the cluster this scenario was run on: 47 nodes — 3
control-plane, 42 worker, 2 monitoring — the workers each with 128 cores and roughly 1007 GiB of
memory, running Kubernetes 1.34.2 on containerd 1.7.28 and Ubuntu 22.04.4, with
OVN-Kubernetes as the CNI (a downstream build of upstream commit d5f19be, with OVN
25.09 and Open vSwitch 2.17) and no kube-proxy.

Worker kubelets were configured with a
[max-pods](https://kubernetes.io/docs/reference/config-api/kubelet-config.v1beta1/)
setting of 110. That setting is what the pod budget on this cluster is computed from.

The node selector targeted the worker role. Excluding only control-plane nodes would
match 44 nodes, adding the two monitoring nodes.

## How to replicate

1. Confirm every worker is Ready and schedulable, and record the role split.
2. Record the kubelet max-pods setting on each worker. Do not assume it is uniform.
3. Compute the schedulable pod budget: workers multiplied by max-pods, minus the
   pods already resident on those workers.
4. Choose an iteration count so that ten pods per iteration fits inside that budget
   with headroom, and set `jobIterations` in the engine configuration to it. This is
   the only value the sizing depends on.
5. Set the node selector to the worker role and confirm the node count it matches,
   then update the `nodeSelector` inputVars in the job configuration to agree.
6. Confirm no pre-existing objects carry this workload's job label. Garbage
   collection selects on that label alone, with no run identifier, so it will delete
   a co-tenant's objects that happen to share it. `cleanup` is set to false in the
   published configuration for this reason; the global `gc` field shares the same
   unscoped selector and runs at the end.
7. Run from `engine/` and keep the metrics output directory:

   ```bash
   cd engine
   PROMETHEUS_URL=<prometheus-url> kube-burner init --uuid=<run-uuid> --config=cluster-density-v2.yml
   ```

   Add `token:` to the `metricsEndpoints` entry if your Prometheus needs one.
   The log and `collected-metrics/` are written to `engine/`.

## How to judge the result

Pod-ready p99 against the scenario's `slo` entry is the verdict; kube-burner's `passed`
flag also fails the run if a `severity: error` alerting rule fires. The 13 s `slo`
bar is this scenario's choice, not an upstream standard; the configuration's own
pod-ready gate is two minutes. Re-derive the bar for a different cluster.

Two checks decide whether the number means anything at all:

- **Coverage.** The pod latency measurement should produce one document per pod
  created. Fewer means the quantiles describe a subset of the run.
- **Peak Pending pods.** This should stay small and transient. A sustained Pending
  population means the run exceeded the schedulable budget and is reporting
  scheduling backlog rather than density.

**The bar sits close to a probe-interval step, so watch for a bimodal distribution.**
4 of every 10 pods per iteration are client pods whose readiness probe curls a Service,
and that probe runs every 10 s. If the Service is reachable when the first probe fires,
the pod is Ready in about a second; if it is not, the pod waits for the next probe and
lands around 11 s. In the published run this did not happen — the per-pod distribution
is unimodal, concentrated at 1 s with a maximum of 3 s, and client pods behave like the
rest. But a cluster whose Service programming is slower would produce a cluster of pods
at roughly 11 s, which is *under* the 13 s bar. The gate would still pass while the
distribution had changed shape entirely. Read the per-pod spread, not only the p99,
before concluding nothing moved.

The etcd entries are supporting signals rather than verdicts, and on virtualized or
shared storage they report the storage layer rather than the control plane.

Treat a fired alert as a question, not a finding, until its underlying request rate
is checked. A p99 computed by Prometheus over a handful of requests reports a
histogram bucket boundary, not a latency distribution, and rules covering rarely-used
verbs can fire this way on an otherwise idle cluster.

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
