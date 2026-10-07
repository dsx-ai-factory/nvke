<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Scheduler baseline

Definition: [scenario.yaml](scenario.yaml)
Result: [scheduler-baseline-2026-07-13.md](../../../results/1.34.2/scheduler-baseline-2026-07-13.md)

## What is tested

What determines kube-scheduler's throughput, and how far its latency moves as
a cluster grows. The scenario sweeps three axes as a full cross product:

| Axis | Values |
|---|---|
| Scheduler `kube-api-qps` (burst equal) | 50, 100, 200, 500 |
| Workload submission QPS (burst equal) | 50, 100, 200, 500 |
| Simulated node count | 100, 500, 1000, 2500, 5000 |

`kube-api-qps` here means the scheduler's client QPS, however it is set — see
"How it is tested".

80 cells. Each is one kube-burner invocation submitting `nodes × 10` pause
pods at that cell's rate, against that cell's node count, with the scheduler
configured at that cell's API rate.

**Every node in this scenario is a KWOK fake node.** They report Ready and
accept bindings with no kubelet and no container runtime behind them, and the
pods placed on them never start. What is measured is the scheduler's decision
and binding path — nothing else. A 5,000-node tier is 5,000 simulated nodes,
not 5,000 machines.

That is also what makes this the most portable scenario in the tree: because
the nodes are simulated, a reader can reproduce a 5,000-node result on
whatever hardware they have. The bound is their API server and KWOK
controller, not a node budget.

**This is not a maximum-scale claim.** The tiers are points chosen to expose a
trend, not a ceiling on NKE. `sizing.derivation` is the part to carry to your
own cluster.

## How it is tested

Each cell runs as five kube-burner jobs in order:

1. Create `NODE_COUNT` KWOK nodes, wait until every one reports Ready, then
   pause `NODE_READY_WAIT`.
2. Submit `NODE_COUNT × PODS_PER_NODE` pause pods at the cell's QPS.
3. Delete the pods, at the same rate.
4. Delete the namespace.
5. Delete the fake nodes.

The teardown jobs are part of the measurement, not housekeeping: letting
namespace garbage collection remove 50,000 pods issues an unbounded delete
burst that perturbs whatever runs next.

Two mechanisms are worth understanding before reading a number.

**Pod density is enforced by an extended resource, not by `maxPods`.** Each
fake node advertises `PODS_PER_NODE` units of an `example.com/scheduler-slot`
resource and each pod requests one. `maxPods` is set higher, to 110, so it
never binds first. This keeps the 1:10 ratio exact regardless of how many
DaemonSets exist on the target cluster — rename the resource's domain prefix
to one you control.

**The scheduler's own rate limit is set outside the engine config.** It is
kube-scheduler's client QPS and burst: `clientConnection.qps` and
`clientConnection.burst` in its `KubeSchedulerConfiguration` when it runs with
`--config`, otherwise the deprecated `--kube-api-qps` and `--kube-api-burst`
flags, which are ignored when `--config` is set. The defaults are 50 and 100.
No kube-burner config can set them, so sweeping that axis means reconfiguring
the scheduler between cells and letting it restart. See "How to replicate".

Measurements come from two independent places, and the result keeps them
apart: algorithm and attempt latency are Prometheus histograms; PodScheduled
P99 is kube-burner's own `podLatency` measurement, timed off the pod watch
stream. When the attempt histogram saturates its top bucket — which it does in
27 of 80 cells — PodScheduled P99 is the only end-to-end number that still
means anything.

## Artifacts and scripts

| Artifact | Role |
|---|---|
| [engine/scheduler-baseline.yml](engine/scheduler-baseline.yml) | The kube-burner config. Engine-native — hand to upstream kube-burner unchanged. |
| [engine/defaults.yaml](engine/defaults.yaml) | `--user-data` values shared by every cell. The per-cell values come from the environment. |
| [engine/kwok-nodes.yml](engine/kwok-nodes.yml) | The fake-node template |
| [engine/pod.yml](engine/pod.yml) | The pause pod submitted against it |
| [engine/scheduler-metrics.yml](engine/scheduler-metrics.yml) | kube-burner metrics profile: the scheduler queries collected per run |
| [scenario.yaml](scenario.yaml) | Topology, stack, sizing rule, metrics contract, SLOs |
| [scheduler.json](../../../dashboards/1.34/scheduler.json) | Grafana dashboard, shared. Scheduling attempts and latency, pending-pod queues, preemption, framework extension points, plugin execution, leader election. Lives in `dashboards/1.34/` because none of its queries are specific to this workload. |
| [scheduler.yml](../../../alerts/1.34/scheduler.yml) | Alerting rules, shared: scheduler correctness and convergence gates. |

Upstream: [kube-burner](https://github.com/kube-burner/kube-burner) v2.7.0,
and [KWOK](https://github.com/kubernetes-sigs/kwok) for the fake nodes. KWOK
is a separate project and its manifest is not vendored here.

```bash
cd engine
PROMETHEUS_URL=<prometheus-url> NODE_COUNT=<nodes> QPS=<qps> BURST=<qps> \
  kube-burner init \
  -c scheduler-baseline.yml \
  --user-data defaults.yaml \
  --uuid=<run-uuid>
```

Run it from `engine/`: kube-burner resolves the templates and profiles from
the working directory. `NODE_COUNT`, `QPS` and `BURST` must come from the
environment, not `defaults.yaml` — user-data takes precedence over the
environment.

### KWOK is not pinned by this scenario

`stack.engine` pins kube-burner to v2.7.0. **KWOK is deliberately not pinned**:
this scenario uses only the `kwok.x-k8s.io/node: fake` annotation, the
standard controller Deployment and the `stage-fast` stage policy, all stable
across releases, so any current release works. Install the stage policy with
the controller: without one, the controller does nothing and fake nodes never
report Ready. Record the release you install alongside your own results —
KWOK's node lifecycle behaviour is what the larger tiers are sensitive to.

### KWOK controller tuning at 2,500 nodes and above

At 2,500 simulated nodes and beyond, the KWOK controller's default lease and
status update rate is not sufficient: fake nodes flip Ready/NotReady mid-run
and inflate the unschedulable queue, which corrupts both latency and
throughput figures. Issuing node lease and status updates every 40 s was enough
to stabilise the published run.

**This tuning cannot live in `engine/kwok-nodes.yml`** — it is KWOK controller
configuration, not a node object, and this scenario does not vendor KWOK's
manifest. Tune it in your own KWOK installation, and treat the
`baseline_validation` step about nodes staying Ready as the check for whether
you need to.

The published result's anomalous block of 2,500-node cells at `kube-api-qps`
500 is consistent with exactly this disturbance — see that result's
Observations.

### Dashboard

`scheduler.json` needs a Prometheus scraping kube-scheduler. Its queries filter
on an optional `k8s_cluster_name` label and match everything when the label is absent,
so a single-cluster Prometheus needs no relabeling.

Every metric this scenario measures is on it: scheduling attempts and
throughput, attempt and algorithm latency, and the pending-pod queues by
queue name.

Three things to read carefully:

- **The dashboard cannot show you PodScheduled P99.** That number comes from
  kube-burner's watch-based measurement, not from a Prometheus metric. The
  latency panels plot the scheduler's own histograms, which is a different
  quantity — and in the cells where binding saturates, the histogram is
  capped while the kube-burner figure is not.
- **The scheduler panels return no data on an idle cluster.** They are
  `rate()` over histograms that only accumulate while pods are actually being
  scheduled. Check the workload is running before concluding a panel is
  broken. `scheduler_permit_wait_duration_seconds` is blank even under load,
  because no default plugin defers a pod at the Permit point.
- **Fake nodes do not limit what this dashboard can show.** Every panel reads
  either a `scheduler_*` metric or the scheduler process's own CPU, memory,
  goroutine and leader-election state. All of those are emitted by the
  scheduler regardless of what kind of node it binds to, so the dashboard is
  as complete under KWOK as it is on real hardware. A dashboard carrying
  `kubelet_*` panels would not be — nothing runs a kubelet here.

#### Validated against live data

Every panel was run against a live Kubernetes 1.34.2 cluster with
OVN-Kubernetes, during a KWOK-backed run of this scenario at the 500-node tier
and workload QPS 50. **13 of the 14 panels returned series**, including all
five latency histograms and the framework and plugin breakdowns.

Two of those 13 read zero throughout a healthy run — the queue panels. They
were confirmed separately by submitting 60 pods with a `nodeSelector` matching
no node: `Pending Pods by Queue` then reported 120 active and 60 unschedulable,
and `Unschedulable Pods` reported 60. A panel that only ever reads zero is
indistinguishable from a broken one until you make it move.

**Permit Wait Duration returned nothing, which is the result it exists to
show.** `scheduler_permit_wait_duration_seconds` is observed only when a plugin
defers a pod at the Permit extension point, and no in-tree default plugin uses
Permit — the metric is absent rather than zero, while `scheduler_preemption_*`
is present on the same scrape. Making it populate would mean running a
non-default scheduler with an out-of-tree Permit plugin, which is outside what
this scenario measures, so it was left unforced.

The latency panels are blank while the cluster is idle and while the scenario
is creating its fake nodes — they are `rate()` over histograms that only
accumulate once pods are actually being scheduled. During node creation the
legend lists the series while the plot stays empty, because
`histogram_quantile()` over an all-zero rate is NaN. Check that pods are being
submitted before concluding a panel is broken.

Figures observed during validation are **not** benchmark results: that cluster
is a simulation, and its numbers are not comparable to the published result.

Of the four rules in `alerts/1.34/scheduler.yml`, three were observed firing:
the stalled-throughput rule fired on its own with work queued and nothing
scheduling, the 5-minute convergence rule fired once the seeded unschedulable
queue had been non-empty for its full window, and the attempt-latency
diagnostic fired with its threshold seeded below the observed value. The rule
on attempts returning `error` stayed silent, correctly — there is no benign way
to make the scheduler fail a scheduling attempt, so it is the one rule
confirmed to evaluate but not to fire.

### Alerts

**These are part of the run's verdict.** `engine/defaults.yaml` passes
`scheduler.yml` to kube-burner as its alert profile, and a firing
`severity: error` rule fails the run, alongside the `podLatency` threshold.

**Kubernetes publishes no scheduler SLO.** SIG-Scalability defines bars for
API request latency and pod startup; it defines none for scheduler throughput,
attempt latency or algorithm latency. So there is no external contract to
encode here, and this scenario does not invent one. What ships instead is two
kinds of rule, and the difference matters:

- **Correctness and convergence gates** (`severity: error`) — a scheduling
  attempt returning `error`, a backoff or unschedulable queue that never
  drains, a queue with work but zero throughput. These have a right answer
  independent of hardware or configuration, so they travel anywhere.
- **A diagnostic** (`severity: warning`) — attempt latency P99 above 5 s. This
  is deliberately not a gate. The scheduler's API client is rate-limited by
  `kube-api-qps`, so a cell running the workload above the scheduler's ceiling
  will breach it *by design*. That is the finding, not a fault.

The scenario's `algorithm_p99_ms` ≤ 20 ms bar is `warning` for the same
reason, and its source is stated as this result's own ceiling rather than an
upstream standard: 19.1 ms was the worst cell across all 80. It is a
"you are outside what we characterized" signal, not a violated contract, and
it should be re-derived rather than carried to a materially larger cluster.

**There is no throughput floor**, and its absence is deliberate. Throughput
here is a configured rate limit, so a floor would only restate the client QPS
a reader already set. What this scenario establishes is a relationship, and a
relationship is not expressible as a scalar threshold.

## Environment

Stack the published result came from. A different stack is a different
scenario.

| | |
|---|---|
| Real nodes | 47 — 3 control-plane, 42 worker, 2 monitoring |
| Simulated nodes | 100 / 500 / 1,000 / 2,500 / 5,000 per tier (KWOK) |
| Worker node | 128 cores, ~1007 GiB memory |
| Control-plane / monitoring node | 64 cores, ~377 GiB memory |
| CPU | AMD EPYC 9004 series |
| Kubernetes | 1.34.2 |
| CNI | OVN-Kubernetes 1.2.0 (OVN core 25.09.1, OVS 3.6.1), vendor build |
| Container runtime | containerd 1.7.28 |
| OS / kernel | Ubuntu 22.04.4 LTS / 5.15.0-113-generic |
| etcd | 3.6.5 |
| Engine | kube-burner v2.7.0 |
| Node simulator | KWOK — not pinned by this scenario |

The real worker nodes carried the control plane and the load generator. The
scheduled pods went to fake nodes.

`build: vendor` on the CNI means a distribution-specific OVN-Kubernetes build
rather than an upstream release artefact. It matters less here than in other
scenarios — this workload does not exercise the data plane.

## How to replicate

1. Install KWOK and its `stage-fast` stage policy, and confirm the controller
   Deployment is Ready. This scenario does not vendor the manifests; take them
   from upstream.
2. Run the `prerequisites.baseline_validation` steps. The DaemonSet check is
   the one most likely to bite: a DaemonSet that tolerates the KWOK taint will
   eat the pod budget being measured.
3. Install upstream kube-burner v2.7.0.
4. For each cell you want:
   - Set the scheduler's client QPS and burst to the cell's value and let it
     restart. **Read the values back off the running scheduler**, not off the
     manifest you applied.
   - Run the invocation above with the cell's `NODE_COUNT`, `QPS` and
     `BURST`.
5. Judge against `slo`, then read the cell in context — see below.

Run time is dominated by the node tier, not the rate: the 100-node cells
complete in under 30 s, which is why they report no CPU figure. Reproducing
all 80 cells is a long exercise; reproducing one column at your own expected
pod creation rate is the useful subset.

Keep burst equal to QPS on both axes. The published matrix does throughout,
and a burst above QPS measures queue drain rather than steady-state
scheduling.

## How to judge the result

The `slo` block has three entries, and only two of them can fail a run. The
`severity: error` rules in `scheduler.yml` also fail it.

| Metric | Gate | What it means |
|---|---|---|
| `pod_scheduled_p99_s` | ≤ 600 s (`error`) | kube-burner's own `podLatency` threshold. Deliberately loose — cells that deliberately outrun the scheduler are *expected* to queue. It catches a run that never converged. |
| `scheduler_error_attempts_per_s` | ≤ 0 (`error`) | Correctness. A scheduling attempt returning `error` is never expected, at any rate or size. `unschedulable` attempts are excluded — those are a normal transient. |
| `algorithm_p99_ms` | ≤ 20 ms (`warning`) | Derived from this result's own ceiling, not from an upstream SLO. Above it at ≤ 5,000 nodes means something this characterization did not see. |

**A passing cell is not the interesting output.** What this scenario produces
is a capacity model, and it is read across cells rather than per cell:

- **Through 1,000 nodes, PodScheduled P99 near zero whenever scheduler QPS ≥
  workload QPS is the healthy shape.** At 2,500 and 5,000 nodes the published
  run queued in several such cells; see its PodScheduled table.
- **Compare the two latencies against each other.** Algorithm P99 rising while
  attempt P99 stays flat means the cycle is getting more expensive but binding
  is keeping up. Attempt P99 saturating while algorithm P99 stays low means
  the opposite: the cycle is fine and the binding queue is the problem. In the
  published run, the second pattern dominates.

Attempt P99 has no single range across the matrix — it depends on both the node
tier and the rate pair. See "Reading the latency ranges" in the result before
comparing your own figures against it.
