<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# CRD / CR API-server characterization

How the API server behaves when a cluster registers many Custom Resource
Definitions and writes many Custom Resources under them.

Result: [crd-cr-characterization-2026-04-17](../../../results/1.34.2/crd-cr-characterization-2026-04-17.md)

## What is tested

Burst create and delete of CRDs and their custom resources, across five sizes
and two schema complexity classes. The questions are whether API request
latency holds, whether API Priority and Fairness rejects anything at the
default inflight configuration, and whether etcd absorbs the write volume.

The workload models a controller-heavy cluster. A platform that installs a few
dozen operators accumulates hundreds of CRDs and thousands of custom
resources, and each CRD registration costs the API server discovery and
OpenAPI work that has nothing to do with how many workers exist.

| Size | CRDs | CRs each | Schema |
|---|---|---|---|
| XS | 100 | 100 | simple |
| S | 500 | 100 | simple |
| M | 50 | 100 | complex |
| L | 100 | 1000 | simple |
| XL | 100 | 100 | complex |

The CRD pass runs at 20 qps and burst (10 for XS), the CR pass at 50 per CRD
invocation — registering a CRD costs the API server far more than writing a
resource under one, so the two are not driven at the same rate.

The two schema classes are defined by the size of the stored resource, not by
field names: **simple** targets roughly 2-3 KB, **complex** roughly 6-10 KB.
Those ranges come from measuring the custom resources a real controller-heavy
cluster holds — secret-sync and DNS resources at the low end, monitoring and
collector resources at the high end.

## How it is tested

Two passes per size.

**CRDs.** Every CRD is the same GVR, so one template registers all of them —
`jobIterations` is the CRD count, and the template varies its group and names
per iteration.

**Custom resources.** One kube-burner invocation per CRD, generated from
templates rather than checked in. Substitute into `cr-simple.yaml.tmpl` (or `cr-complex.yaml.tmpl`) and
`cr-pass.yaml.tmpl`:

| Placeholder | Value |
|---|---|
| `${CRD_KIND}` | the kind the CRD registered, e.g. `TestResource00042` |
| `${CRD_INDEX}` | its zero-padded index, e.g. `00042` |
| `${SCENARIO}` | the size group, e.g. `xs` |
| `${CR_COUNT}` | custom resources per CRD for this size |
| `${NAMESPACE}` | namespace to create them in |
| `${CR_TEMPLATE}` | path to the substituted object template |

kube-burner then templates `{{.Iteration}}` per resource. `{{.JobNamespace}}` in
the object template is **not** a kube-burner builtin — the builtins are
`Iteration`, `Replica`, `JobName`, `UUID` and `RunID` — so `cr-pass.yaml.tmpl`
supplies it through `inputVars`. Without that block the template fails to render.

**Wait for every CRD to reach `Established` before starting this pass**, for
example with `kubectl wait --for condition=Established crd/<name>`. A custom resource of a kind the API server has not finished
registering is rejected.

**Teardown.** Not driven by kube-burner — the configs set `cleanup: false`, and
deletion is `kubectl delete` of the namespace and then the CRDs:

```
kubectl delete namespace <namespace>
kubectl get crd -o name | grep <group> | xargs kubectl delete
```

`delete_p99_ms` is those namespace and CRD DELETEs, so a run that skips teardown
produces no DELETE figure. The namespace removes its custom resources by
DELETECOLLECTION, which `delete_p99_ms` does not include.

**Metrics.** `engine/metrics-profile.yaml` reads the API server's and etcd's
metrics from Prometheus, one figure per window. Query two windows with
`kube-burner index`, from `engine/`: create (CRD pass start to CR pass end) and
teardown. Extend `--end` by two scrape intervals so a short window returns data.

```
kube-burner index -u <prometheus-url> -m metrics-profile.yaml --start <epoch> --end <epoch>
```

| Metric | Window | From |
|---|---|---|
| `duration_s` | create | window length |
| `post_p99_ms` | create | `post_p99_ms` |
| `peak_reqs_per_sec` | create | `peak_reqs_per_sec` |
| `etcd_size_increase_mb` | create | (`etcd_db_in_use_bytes` − `etcd_db_in_use_bytes-start`) ÷ 10⁶ |
| `delete_p99_ms` | teardown | `delete_p99_ms` |
| `429s_count` | both | `apf_rejected`, summed; absent means 0 |

Check `post_count` and `delete_count` are non-zero: kube-burner reports a p99 of 0
for a window with no matching calls.

## Artifacts and scripts

| Artifact | Role |
|---|---|
| [engine/xs.yaml](engine/xs.yaml) and `s`, `m`, `l`, `xl` | One per size. CRD pass only. |
| [engine/crd-simple.yaml](engine/crd-simple.yaml), [engine/crd-complex.yaml](engine/crd-complex.yaml) | CRD templates, one per schema class |
| [engine/cr-simple.yaml.tmpl](engine/cr-simple.yaml.tmpl), [engine/cr-complex.yaml.tmpl](engine/cr-complex.yaml.tmpl) | CR object templates for the second pass, before substitution |
| [engine/cr-pass.yaml.tmpl](engine/cr-pass.yaml.tmpl) | The second pass's kube-burner config, before substitution |
| [engine/metrics-profile.yaml](engine/metrics-profile.yaml) | Prometheus queries for the published metrics |
| [scenario.yaml](scenario.yaml) | Topology, stack, sizing rule, metrics contract, SLOs |
| [control-plane.json](../../../dashboards/1.34/control-plane.json) | Grafana dashboard, shared |

No driver script or alerting rules ship with this scenario. The published run ran
the CR passes in parallel across CRDs. The 50 qps limit applies to each
invocation, so the aggregate CR rate grows with the number running at once.

## Environment

The published run used 7 nodes — 3 control plane, 2 workers, 2 monitoring — at
128 cores and roughly 1 TB of memory each, on Kubernetes 1.34.2 with
OVN-Kubernetes as the CNI (a downstream build of upstream commit d5f19be, with
OVN 25.09 and Open vSwitch 2.17). Full version block in
[scenario.yaml](scenario.yaml).

Worker count barely matters here. The load is control-plane bound: custom
resources are API and etcd objects, and almost nothing is scheduled. What
matters is the API server and etcd, and the APF configuration in front of them.

## How to replicate

1. Read `scenario.yaml`. `sizing.derivation` is the rule for choosing sizes on
   your own cluster.
2. Run the `baseline_validation` steps under `prerequisites`.
3. Install kube-burner. The published run used v2.6.1.
4. Record the start time. From `engine/`, run the size's config to register the
   CRDs, then wait for all of them to reach `Established`:

   ```bash
   cd engine
   kube-burner init -c xs.yaml
   ```
5. Generate the object template and config from the `.tmpl` files for each CRD
   and run them, one invocation per CRD. Record the end time, and query the
   create window as above.
6. Tear down with `kubectl delete` as above, recording its start and end, and
   query that window.
7. Judge the run against `slo`.

Sizes are separate runs against a clean baseline. Running them back to back
without deleting between them measures something else.

## How to judge the result

Three gates, all in `slo`:

| Metric | Bar |
|---|---|
| `post_p99_ms` | ≤ 1000 |
| `delete_p99_ms` | ≤ 1000 |
| `429s_count` | 0 |

The first two use the 1 s p99 of the SIG-Scalability mutating-API SLO, which
itself excludes CRDs. The third: a rejection at the default 200 mutating / 400
read-only inflight configuration means the workload exceeded what the API
server will admit, which is the condition this scenario exists to detect.

`etcd_size_increase_mb` has no bar. It is reported because it should scale
linearly with the bytes written, and a departure from that is worth
investigating even when latency is fine.

Read the complex-schema sizes separately from the simple ones: at the same
per-invocation rate, M reached 788 ms POST p99 on 5,000 resources and L 19.9 ms
on 100,000, with twice M's CRDs in parallel.
