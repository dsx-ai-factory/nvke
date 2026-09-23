# Agent guide — NKE reference Grafana dashboards

Read this before adding, copying, or editing JSON under `controller/deploy/dashboards/`.

These files are **operator-importable reference dashboards**. They are **not** installed by the `nke-stack` Helm composition, and they are not Grafana Cloud or private-fleet exports.

Operators: import steps and a short description of each file are in [README.md](README.md). This file is the contributor contract.

## What lives here

| File | Purpose | Primary scrape |
|---|---|---|
| `nke-site-controller-health-reference.json` | Site-controller workload and node health | kube-state-metrics; optional apiserver/etcd, CNPG, Argo CD, Longhorn, Teleport |
| `nke-etcd-reference.json` | etcd member health | etcd mixin metrics, scoped by `$job` |
| `nke-cnpg-per-cluster-reference.json` | CloudNativePG (optional HA Postgres) | `cnpg_*` |
| `nke-ovn-kubernetes-controller-reference.json` | Tenant (or site) OVN-Kubernetes | kube-state-metrics; optional `ovnkube_controller_*` |
| `nke-provisioning-duration-reference.json` | Node provision/teardown duration and outcomes | controller `/metrics` (`cwe_*` or `nke_controller_*`) |
| `nke-cluster-upgrade-overview-reference.json` | GPU-cluster upgrade progress | kube-state-metrics; optional controller inventory metrics |

Each dashboard has a **Requirements and scope** markdown panel. Keep that panel truthful when you change queries.

## Format

- Grafana **import JSON** (schema with `__inputs`, `panels` / rows, `templating`), not `dashboard.grafana.app/v2` Kubernetes CRDs.
- Datasource: `__inputs` entry `DS_PROMETHEUS` (Prometheus plugin). Operators pick a datasource at import. Do not pin `mimir-*`, Promxy, or a Grafana Cloud UID.
- `id`: `null`. Stable `uid` matching the filename without `.json` (Grafana's 40-character limit; e.g. `nke-site-controller-health-reference`). `title` prefixed `Reference - …`.
- Filenames: `nke-<topic>-reference.json`.
- `refresh`: `1m` or slower, and keep `timepicker.refresh_intervals` starting at `1m`. Grafana refuses anything below its `min_refresh_interval` with "Dashboard refresh interval is too low", so do not copy an upstream mixin's `10s`.
- Query variables: write `query` as a **plain `label_values(...)` string**, matching `definition`. Grafana migrates a string into a Label-values query; an object without `qryType` falls back to **Label names**, so the dropdown lists label names (`namespace`, `pod`, `instance`) instead of values.

Do not bump `controller/deploy/charts/controller/Chart.yaml` for dashboard-only edits. These JSON files are not chart templates.

## Workflow for a dashboard change

1. Identify the **cluster that emits the metrics** (site controller vs tenant vs CWE process). Site kube-prometheus does not scrape tenant ovnkube or tenant etcd.
2. Confirm the series exist in **this repo’s OSS stack** (`generated/nke-stack/values.yaml`, controller metrics, ovn `values-nke.yaml` contract in `os-image/packages/cni-ovn/README.md`).
3. Prefer **required** panels that work with kube-state-metrics or documented CWE metrics. Put everything else in **optional** rows that stay empty without turning red (`or vector(0)` only when zero is a real reading, not a missing scrape).
4. Update the Requirements panel, `description`, and tags (`nke`, plus the topic).
5. Strip private-fleet chrome before commit: `$env`, hardcoded cluster-name regexes, datasource regexes for hosted Mimir, Slack, links to unpublished dashboards, `topk` over a fleet unless the dashboard is explicitly multi-cluster.

When porting a dashboard from elsewhere, treat it as a **query catalog**, not a file to check in.

## Ground truth: what OSS installs

`nke-stack` (`generated/nke-stack/values.yaml`):

- cert-manager (`cert-manager`)
- SPIRE server (`spire-server`) and agent (`spire-system`, DaemonSet/ServiceAccount `spire-agent`)
- kube-prometheus-stack (`monitoring`) — Prometheus, Grafana, node-exporter, kube-state-metrics. **Does not** configure kubeadm etcd TLS scrape.
- PostgreSQL (`postgres`, in-repo chart) — **not** CNPG
- Temporal (`temporal`)
- CWE (`cloud-workflow-engine`, `nameOverride: cloud-workflow-engine`, default **replicaCount 1**)

**Not** in nke-stack: SWE / `site-workflow-engine`, Longhorn, Teleport `tbot`, process-exporter, sdn-monitoring, Argo CD (deploy-repo only), CNPG (docs/overlays may use it; default stack does not).

Site bootstrap CNI may be `calico` or `ovn-kubernetes`. OVN chart path: `ovnkube-control-plane` + `ovnkube-single-node-zone`, host OVS, images `ovn-c` / `ovnkube-c`. Confirm DaemonSet **name** against the packaged `values-nke.yaml` before hardcoding `ovnkube-node`.

## Selectors and labels

**Cluster identity is `k8s_cluster_name`.** It is the Prometheus label produced
from the OpenTelemetry resource attribute `k8s.cluster.name`, and it is the same
convention the NKTP reference dashboards use, so a single Grafana can hold both
sets without two spellings of "which cluster". The variable is still *named*
`$cluster`; only the label is `k8s_cluster_name`:

```
label_values(kube_node_info, k8s_cluster_name)   # variable
kube_node_info{k8s_cluster_name=~"$cluster"}     # panel
```

Do not use a bare `cluster` label for Kubernetes cluster identity. Some
exporters already define `cluster` as something else — CNPG's `cluster` is the
**Postgres** Cluster resource name. Scope CNPG by `$namespace` (typically one
Cluster CR per namespace); leave CNPG's `cluster` in joins and legends only.

| Context | Do | Do not |
|---|---|---|
| Kubernetes cluster identity | `k8s_cluster_name=~"$cluster"` | A bare `cluster=` label, `$env`, or a hardcoded cluster-name regex |
| etcd mixin dashboard | `$cluster` = `k8s_cluster_name`; expose the mixin's `clusterLabel: job` as a separate `$job` | Name a variable `$cluster` and bind it to the Prometheus `job` |
| CNPG | `$cluster` = `k8s_cluster_name`; scope with `$namespace` | Treat CNPG's `cluster` label as a Kubernetes cluster, or add a Postgres-cluster variable |
| CWE / nke-controller lifecycle | Filter `tenant_cluster`, `node_type`; give-ups use `cluster_name`. Match both metric prefixes: `{__name__=~"(cwe\|nke_controller)_…"}` | Invent `env`; keep phase labels that are not in `lifecycle.go`; hard-code only `cwe_*` |
| Workloads | Match `cloud-workflow-engine\|nke-controller` for the controller Deployment/container; SPIRE as above | Query SWE, `tbot`, `job="process-exporter"` |

Controller metrics carry `tenant_cluster`, not `k8s_cluster_name`. A pure-controller board
(the provisioning dashboard) should filter `tenant_cluster` directly. Existing sites
emit `cwe_*`; greenfield compiled-in prefix is `nke_controller_*`. Match both in PromQL
rather than a dropdown the operator has to set. Only when joining those series against
kube-state-metrics on the same panel do you rewrite the label:

```
label_replace({__name__=~"(cwe|nke_controller)_node_info",tenant_cluster=~"$cluster"}, "k8s_cluster_name", "$1", "tenant_cluster", "(.*)")
```

HA thresholds of **3** (etcd members, CNPG collectors, ready nodes) are colo-sized. Parameterize or document “typical HA”; nke-stack CWE is 1 replica and Kind can be 1 node.

## Do not ship

- SWE (`site-workflow-engine`) — deprecated; CWE replaced it
- Teleport `tbot` as a required panel
- Longhorn as required for “renders with data”
- `namedprocess_namegroup_*` / `job="process-exporter"` (not packaged)
- Full SDN / ovn-kubernetes dashboard catalog (IC pack `dashboard-ic` is off for NKE). One controller reference is enough.
- Alert overlay lines (`vector(0.15)` “alert fires”) unless this repo ships those Prometheus rules
- Secrets, real Slack channels, Grafana Cloud URLs, kubeconfigs

## Per-dashboard contracts

**Site controller health.** Glance: CWE, SPIRE, Temporal, cert-manager, node Ready. Optional collapsed: apiserver/etcd (needs scrape), CNPG **or** postgres, Argo CD, Longhorn, Teleport. One etcd “members with leader” stat is enough; drill-down is the etcd reference dashboard. Empty optional ≠ red.

**etcd.** Panel set from [etcd mixin](https://github.com/etcd-io/etcd/tree/main/contrib/mixin) (`etcd.libsonnet` / `targets.libsonnet`): Up (`etcd_server_has_leader`, not scrape `up{}`), RPC rate, active streams, DB size, disk sync, memory, client/peer traffic, raft/leader elections, peer RTT. `$cluster` is `k8s_cluster_name`; the mixin's `clusterLabel: job` is a separate `$job` variable, which is single-select and also scopes the non-etcd-specific `grpc_server_*` and `process_resident_memory_bytes` series to the etcd process. Empty until kube-prometheus `kubeEtcd` (or equivalent) scrapes kubeadm etcd. Do not add apiserver watch-cache or process-exporter CPU here.

**CNPG.** Only for sites that run CloudNativePG. Default nke-stack Postgres will leave this empty — that is expected.

**OVN-Kubernetes controller.** Required: kube-state-metrics restarts (`container="ovnkube-controller"`) and DaemonSet ready if the name matches. Optional: `ovnkube_controller_build_info`, NB–SB timestamp diff (needs ovnkube scrape **on the cluster that runs the CNI**). Do not mix `ovnkube_node_workqueue_*` onto a controller board. CNI request metrics belong on a node row, if at all. Calico clusters: empty is OK.

**Provisioning duration.** Source of truth: `controller/internal/metrics/lifecycle.go` and `node_provisioning.go`.

- `{__name__=~"(cwe|nke_controller)_node_provision_total",result="failure"}` is **Temporal attempts**, not failed nodes. Terminal failure is the matching `*_node_provisioning_giveups_total`.
- Success rate = successes ÷ (successes + give-ups), not success/(success+failure).
- Provision phases: `capi_create`, `capi_ready`, `host_preparation`, `reboot` (optional), `join`, `post_join`.
- Teardown phases: `cluster_remove`, `capi_delete`.
- Drop any other `phase` label values copied from older dashboards (SWE and previous infrastructure-provider names).

## Duplication

Do not copy the same etcd WAL/leader or ovnkube workqueue graphs onto two reference dashboards. Glance board = one stat; detail board = mixin/IC subset.

## Verification

There is no `make` target for Grafana JSON. Before finishing:

- Every PromQL selector matches OSS names or is documented as optional
- Kubernetes cluster identity is `k8s_cluster_name`, and every query variable's
  `query` is a plain string (not an object)
- Requirements panel matches the queries
- No hosted-Mimir datasource pins, SWE selectors, or process-exporter leftovers
- Optional missing scrape does not false-green via `or vector(0)` unless zero is meaningful
