# NVKE reference Grafana dashboards

These JSON files are **importable reference dashboards**. They are not installed
by the `nke-stack` Helm chart. Grafana in the site `monitoring` namespace is a
reasonable target; any Prometheus datasource that scrapes the relevant cluster
works.

Each dashboard has a **Requirements and scope** panel that lists required vs
optional metrics. Empty optional rows mean that scrape is not present; they are
not a failure.

## Import

1. Open Grafana → **Dashboards** → **New** → **Import**.
2. Upload a file from this directory, or paste its JSON.
3. When prompted, select a **Prometheus** datasource (`DS_PROMETHEUS`). Do not
   expect a pre-pinned Mimir or Grafana Cloud UID.
4. Import. `uid` is stable and matches the filename without `.json`, so a
   second import **updates** the same dashboard instead of creating a duplicate.

Refresh is `1m` or `5m`. The time-picker interval list starts at `1m`. Grafana
rejects anything below the server `min_refresh_interval` (often `10s` or `1m`)
with "Dashboard refresh interval is too low".

### After import

- **`$cluster`** is a Kubernetes cluster name from the Prometheus label
  `k8s_cluster_name` (OpenTelemetry resource `k8s.cluster.name`). If the
  dropdown is empty, the scrape is not stamping that label.
- **Provisioning** uses `$tenant_cluster` and `$node_type` from controller
  `/metrics`, not `k8s_cluster_name`.

## Dashboards

| File | Title | What it shows |
|---|---|---|
| [`nvke-site-controller-health-reference.json`](nvke-site-controller-health-reference.json) | Reference - NVKE Management Cluster Health | Glance board for the management cluster: NVKE, SPIRE, Temporal, cert-manager, and node Ready. Optional collapsed rows for apiserver/etcd and PostgreSQL/CNPG. |
| [`nvke-provisioning-duration-reference.json`](nvke-provisioning-duration-reference.json) | Reference - NVKE Provisioning Duration | Node provision/teardown duration, success vs give-up rate, and in-progress nodes from controller `/metrics`. |
| [`nvke-cluster-upgrade-overview-reference.json`](nvke-cluster-upgrade-overview-reference.json) | Reference - NVKE Cluster Upgrade Overview | Kubernetes cluster upgrade progress: kubelet versions from kube-state-metrics, plus control-plane and OS-profile panels when NVKE controller inventory metrics are available. |

For sites running CloudNativePG, use its
[official Grafana dashboard](https://github.com/cloudnative-pg/grafana-dashboards/blob/main/charts/cluster/grafana-dashboard.json).

Controller series use the `nvke_controller_*` prefix. Management cluster Ready/Version/Restarts match the
`cloud-workflow-engine`, `nke-controller` and `nvke-controller` workload names.

## Editing

Contributor constraints (selectors, what OSS installs, what not to ship) are in
[AGENTS.md](AGENTS.md).
