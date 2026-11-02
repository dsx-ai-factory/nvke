# Agent guide — NVKE reference Grafana dashboards

Read this before adding, copying, or editing JSON under `controller/deploy/dashboards/`.

These files are **operator-importable reference dashboards**. They are **not** installed by the `nke-stack` Helm composition, and they are not Grafana Cloud or private-fleet exports.

Operators: import steps and a short description of each file are in [README.md](README.md). This file is the contributor contract.

## What lives here

| File | Purpose | Primary scrape |
|---|---|---|
| `nvke-site-controller-health-reference.json` | Management cluster workload and node health | kube-state-metrics; optional apiserver/etcd, CNPG |
| `nvke-provisioning-duration-reference.json` | Node provision/teardown duration and outcomes | controller `/metrics` (`nvke_controller_*`) |
| `nvke-cluster-upgrade-overview-reference.json` | Kubernetes cluster upgrade progress | kube-state-metrics; optional NVKE controller inventory metrics |

Each dashboard has a **Requirements and scope** markdown panel. Keep that panel truthful when you change queries.

## Format

- Grafana **import JSON** (schema with `__inputs`, `panels` / rows, `templating`), not `dashboard.grafana.app/v2` Kubernetes CRDs.
- Datasource: `__inputs` entry `DS_PROMETHEUS` (Prometheus plugin). Operators pick a datasource at import. Do not pin `mimir-*`, Promxy, or a Grafana Cloud UID.
- `id`: `null`. Stable `uid` matching the filename without `.json` (Grafana's 40-character limit; e.g. `nvke-site-controller-health-reference`). `title` prefixed `Reference - …`.
- Filenames: `nvke-<topic>-reference.json`.
- `refresh`: `1m` or slower, and keep `timepicker.refresh_intervals` starting at `1m`. Grafana refuses anything below its `min_refresh_interval` with "Dashboard refresh interval is too low", so do not copy an upstream mixin's `10s`.
- Query variables: write `query` as a **plain `label_values(...)` string**, matching `definition`. Grafana migrates a string into a Label-values query; an object without `qryType` falls back to **Label names**, so the dropdown lists label names (`namespace`, `pod`, `instance`) instead of values.

Do not bump `controller/deploy/charts/controller/Chart.yaml` for dashboard-only edits. These JSON files are not chart templates.

## Workflow for a dashboard change

1. Identify the **cluster that emits the metrics** (management cluster vs tenant vs controller process). Site kube-prometheus does not scrape tenant etcd.
2. Confirm the series exist in **this repo’s OSS stack** (`generated/nke-stack/values.yaml`, controller metrics).
3. Prefer **required** panels that work with kube-state-metrics or documented controller metrics. Put everything else in **optional** rows that stay empty without turning red (`or vector(0)` only when zero is a real reading, not a missing scrape).
4. Update the Requirements panel, `description`, and tags (`nvke`, plus the topic).
5. Strip private-fleet chrome before commit: `$env`, hardcoded cluster-name regexes, datasource regexes for hosted Mimir, Slack, links to unpublished dashboards, `topk` over a fleet unless the dashboard is explicitly multi-cluster.

When porting a dashboard from elsewhere, treat it as a **query catalog**, not a file to check in.

## Ground truth: what OSS installs

`nke-stack` (`generated/nke-stack/values.yaml`):

- cert-manager (`cert-manager`)
- SPIRE server (`spire-server`) and agent (`spire-system`, DaemonSet/ServiceAccount `spire-agent`)
- kube-prometheus-stack (`monitoring`) — Prometheus, Grafana, node-exporter, kube-state-metrics. **Does not** configure kubeadm etcd TLS scrape.
- PostgreSQL (`postgres`, in-repo chart) — **not** CNPG
- Temporal (`temporal`)
- NVKE controller (`cloud-workflow-engine`, `nameOverride: cloud-workflow-engine`, default **replicaCount 1**)

**Not** in nke-stack: SWE / `site-workflow-engine`, process-exporter, sdn-monitoring, CNPG (docs/overlays may use it; default stack does not).

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
**Postgres** Cluster resource name, not the Kubernetes cluster identity.

| Context | Do | Do not |
|---|---|---|
| Kubernetes cluster identity | `k8s_cluster_name=~"$cluster"` | A bare `cluster=` label, `$env`, or a hardcoded cluster-name regex |
| NVKE controller lifecycle | Filter `tenant_cluster`, `node_type`; give-ups use `cluster_name`. Query `nvke_controller_*` metrics directly. | Invent `env`; keep phase labels that are not in `lifecycle.go` |
| Workloads | Match `cloud-workflow-engine\|nke-controller\|nvke-controller` for the controller Deployment/container; SPIRE as above | Query SWE or `job="process-exporter"` |

Controller metrics carry `tenant_cluster`, not `k8s_cluster_name`. A pure-controller board
(the provisioning dashboard) should filter `tenant_cluster` directly. Only when joining
those series against kube-state-metrics on the same panel do you rewrite the label:

```
label_replace(nvke_controller_node_info{tenant_cluster=~"$cluster"}, "k8s_cluster_name", "$1", "tenant_cluster", "(.*)")
```

HA thresholds of **3** (etcd members, ready nodes) are colo-sized. Parameterize or document “typical HA”; nke-stack runs one controller replica and Kind can be 1 node.

## Do not ship

- SWE (`site-workflow-engine`) — deprecated; the NVKE controller replaced it
- `namedprocess_namegroup_*` / `job="process-exporter"` (not packaged)
- Full SDN / ovn-kubernetes dashboard catalog (IC pack `dashboard-ic` is off for NVKE).
- Alert overlay lines (`vector(0.15)` “alert fires”) unless this repo ships those Prometheus rules
- Secrets, real Slack channels, Grafana Cloud URLs, kubeconfigs

## Per-dashboard contracts

**Management cluster health.** Glance: NVKE controller, SPIRE, Temporal, cert-manager, node Ready. Optional collapsed: apiserver/etcd (needs scrape), CNPG **or** postgres. Keep the etcd Has Leader panel as the control-plane health signal. Empty optional ≠ red.

**Provisioning duration.** Source of truth: `controller/internal/metrics/lifecycle.go` and `node_provisioning.go`.

- `nvke_controller_node_provision_total{result="failure"}` is **Temporal attempts**, not failed nodes. Terminal failure is the matching `nvke_controller_node_provisioning_giveups_total`.
- Success rate = successes ÷ (successes + give-ups), not success/(success+failure).
- Provision phases: `capi_create`, `capi_ready`, `host_preparation`, `reboot` (optional), `join`, `post_join`.
- Teardown phases: `cluster_remove`, `capi_delete`.
- Drop any other `phase` label values copied from older dashboards (SWE and previous infrastructure-provider names).

## Verification

There is no `make` target for Grafana JSON. Before finishing:

- Every PromQL selector matches OSS names or is documented as optional
- Kubernetes cluster identity is `k8s_cluster_name`, and every query variable's
  `query` is a plain string (not an object)
- Requirements panel matches the queries
- No hosted-Mimir datasource pins, SWE selectors, or process-exporter leftovers
- Optional missing scrape does not false-green via `or vector(0)` unless zero is meaningful
