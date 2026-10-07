# NKTP Grafana dashboards

JSON Grafana dashboards shipped with the chart. The provisioned dashboards in
`default/` use the Classic JSON model. The import-only dashboards in `examples/`
use the V1 Resource model, with the dashboard under `spec`. Use Grafana 12.2 or
later when importing the examples.

**`default/`** is provisioned into the bundled LGTM Grafana when `otel-lgtm.enabled` is true (the chart default). Grafana reads them at pod start. Changing a default dashboard rolls the LGTM pod via a ConfigMap checksum. Disabling LGTM removes that provisioning.

**`examples/`** is not provisioned. Use these with your own Grafana.

## Import (your own Grafana)

1. Open Grafana → **Dashboards** → **Import**.
2. Upload the JSON file (or paste its contents).
3. Select a Prometheus datasource. Example dashboards declare an input named `DS_PROMETHEUS`.
4. Confirm the labels required by the dashboard you import. Most example queries use `k8s_cluster_name` (from the OpenTelemetry resource attribute `k8s.cluster.name`).

## Default (autoloaded into LGTM)

Each Collector has its own dashboard, tagged `nktp-collector`. They share one
layout: a **Pipeline** row with received, exported, failed and refused items
as totals for each signal the Collector handles, plus the 20 busiest replicas,
rows specific to that Collector, a **Queues and resources** row with the 20
largest replicas, and a **Self-telemetry sidecar** row that shows all three
signals. The **Collectors** dropdown in the top bar switches between
them. Queries select either the main Collector (`job="nktp-<name>"`) or its
sidecar (`job="nktp-<name>-sidecar"`), never both.

| File | Title | Description |
| --- | --- | --- |
| `default/nktp-health.json` | NKTP Health | Workload readiness, pipeline faults, queue pressure, diagnostic logs, and internal traces across all Collectors, with a separate row for the sidecars. |
| `default/nktp-daemonset.json` | NKTP DaemonSet | Node collector pipeline for all three signals per node, its Target Allocator, resources, and sidecar. |
| `default/nktp-watchdog.json` | NKTP Watchdog | Watchdog pipeline, its Target Allocator and the Operator scrape, resources, and sidecar. |
| `default/nktp-gateway-logs.json` | NKTP Logs Gateway | Logs gateway pipeline, resources, and sidecar. |
| `default/nktp-gateway-metrics.json` | NKTP Metrics Gateway | Metrics gateway pipeline, resources, and sidecar. |
| `default/nktp-gateway-traces.json` | NKTP Traces Gateway | Traces gateway pipeline, resources, and sidecar. |
| `default/nktp-cluster-events.json` | NKTP Cluster Events | Kubernetes Event reader pipeline, leader election, resources, and sidecar. |
| `default/nktp-large-target-metrics.json` | NKTP Large-target Metrics | Large-target pool pipeline, its Target Allocator, the largest targets, kube-state-metrics scrape health, resources, and sidecar. |
| `default/nktp-operator.json` | NKTP Operator | OpenTelemetry Operator reconciliation, webhook HTTP traffic and process health. Requires scraped Operator metrics in the selected Prometheus datasource. |
| `default/otel-lgtm-health.json` | OTel LGTM Health | Bundled LGTM collector up, receive/export rates, memory, and exporter queue. |

## Examples (import only)

| File | Title | Description |
| --- | --- | --- |
| `examples/availability-per-cluster-reference.json` | Reference - Cluster Availability | Per-cluster capacity availability (GPUs) and service availability (Kubernetes API). Requires `k8s_cluster_name`. Extra installs: NVIDIA GPU Operator with DCGM Exporter; NVSentinel Janitor. |
| `examples/etcd-reference.json` | Reference - etcd | etcd health, RPCs, storage, memory, and network traffic. Requires a Prometheus datasource that scrapes etcd; the cluster filter uses `k8s_cluster_name` when available. |
| `examples/kubernetes-health-reference.json` | Reference - Kubernetes Health | Cluster, control-plane, node, pod, and workload health. Requires `k8s_cluster_name`. |
| `examples/nvsentinel-latency-errors-reference.json` | Reference - NVSentinel Latency and Errors | NVSentinel latency, errors, deployment health, GPU health, node draining, MongoDB, and fault quarantine. Requires `k8s_cluster_name`. See https://github.com/NVIDIA/NVSentinel. |
