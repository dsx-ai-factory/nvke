# NKE reference Grafana dashboards

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
- **etcd** also has **`$job`** (the Prometheus scrape job for etcd). Leave
  `$cluster` on All if etcd series have no `k8s_cluster_name`.
- **Provisioning** uses `$tenant_cluster` and `$node_type` from controller
  `/metrics`, not `k8s_cluster_name`.

Site Prometheus does not scrape tenant ovnkube or tenant etcd. Import those
boards against the Prometheus that actually scrapes that cluster.

## Dashboards

| File | Title | What it shows |
|---|---|---|
| [`nke-site-controller-health-reference.json`](nke-site-controller-health-reference.json) | Reference - NKE Site Controller Health | Glance board for the site controller: NKE, SPIRE, Temporal, cert-manager, and node Ready. Optional collapsed rows for apiserver/etcd, PostgreSQL/CNPG, Argo CD, Longhorn, and Teleport. |
| [`nke-etcd-reference.json`](nke-etcd-reference.json) | Reference - etcd | Panels adapted from the upstream etcd mixin (reusable monitoring dashboard definitions). You do not need to install the mixin to use this dashboard; you do need a Prometheus scrape of etcd. |
| [`nke-cnpg-per-cluster-reference.json`](nke-cnpg-per-cluster-reference.json) | Reference - CNPG Per Cluster | CloudNativePG health by Kubernetes cluster and namespace. Default nke-stack Postgres is not CNPG; empty panels are expected there. |
| [`nke-ovn-kubernetes-controller-reference.json`](nke-ovn-kubernetes-controller-reference.json) | Reference - OVN-Kubernetes Controller | ovnkube-controller restarts and node-agent DaemonSet readiness (kube-state-metrics). NB–SB timestamps need an ovnkube scrape on the cluster that runs the CNI. Empty on Calico. |
| [`nke-provisioning-duration-reference.json`](nke-provisioning-duration-reference.json) | Reference - NKE Provisioning Duration | Node provision/teardown duration, success vs give-up rate, and in-progress nodes from controller `/metrics`. |
| [`nke-cluster-upgrade-overview-reference.json`](nke-cluster-upgrade-overview-reference.json) | Reference - NKE Cluster Upgrade Overview | Upgrade progress for GPU clusters: kubelet, Calico, control plane, and OS profile. Applies only to GPU clusters. |

Controller series match both `cwe_*` (existing sites) and `nke_controller_*`
(greenfield prefix). Site-controller Ready/Version/Restarts match both
`cloud-workflow-engine` and `nke-controller` workload names.

## Editing

Contributor constraints (selectors, what OSS installs, what not to ship) are in
[AGENTS.md](AGENTS.md).
