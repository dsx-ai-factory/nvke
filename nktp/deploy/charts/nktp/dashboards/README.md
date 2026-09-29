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
4. Confirm the labels required by the dashboard you import. Most example queries use `k8s_cluster_name` (from the OpenTelemetry resource attribute `k8s.cluster.name`); NKE controller inventory queries use `name` or `tenant_cluster`.

## Default (autoloaded into LGTM)

| File | Title | Description |
| --- | --- | --- |
| `default/nktp-health.json` | NKTP Health | Workload readiness, pipeline faults, queue pressure, diagnostic logs, and internal traces. |
| `default/nktp-daemonset.json` | NKTP DaemonSet | Node collector throughput, failures, queues, CPU and memory, plus Target Allocator scrape health. |
| `default/nktp-watchdog.json` | NKTP Watchdog | Watchdog receive/export rates, failures, queues, and allocator collectors. |
| `default/nktp-gateway-logs.json` | NKTP Logs Gateway | Logs gateway receive/export rates. |
| `default/nktp-gateway-metrics.json` | NKTP Metrics Gateway | Metrics gateway receive/export rates. |
| `default/nktp-gateway-traces.json` | NKTP Traces Gateway | Traces gateway receive/export rates. |
| `default/nktp-cluster-events.json` | NKTP Cluster Events | Kubernetes Event reader throughput, export failures, and leadership changes. |
| `default/nktp-large-target-metrics.json` | NKTP Large-target Metrics | Large-target metrics collector receive/export rates. |
| `default/nktp-operator.json` | NKTP Operator | OpenTelemetry Operator reconciliation, webhook HTTP traffic and process health. Requires scraped Operator metrics in the selected Prometheus datasource. |
| `default/otel-lgtm-health.json` | OTel LGTM Health | Bundled LGTM collector up, receive/export rates, memory, and exporter queue. |

## Examples (import only)

| File | Title | Description |
| --- | --- | --- |
| `examples/availability-per-cluster-reference.json` | Reference - Cluster Availability | Per-cluster capacity availability (GPUs) and service availability (Kubernetes API). Requires `k8s_cluster_name`. Extra installs: NVIDIA GPU Operator with DCGM Exporter; NVSentinel Janitor. |
| `examples/kubernetes-health-reference.json` | Reference - Kubernetes Health | Cluster, control-plane, node, pod, and workload health. Requires `k8s_cluster_name`. |
| `examples/nvsentinel-latency-errors-reference.json` | Reference - NVSentinel Latency and Errors | NVSentinel latency, errors, deployment health, GPU health, node draining, MongoDB, and fault quarantine. Requires `k8s_cluster_name`. See https://github.com/NVIDIA/NVSentinel. |
| `examples/nke/nke-cluster-upgrade-overview-reference.json` | Reference - NKE Cluster Upgrade Overview | Kubernetes kubelet, Calico, and OS-profile upgrade progress. Control-plane upgrade status and OS profile require NKE controller inventory metrics (`cwe_*` or `nke_controller_*`) (https://github.com/dsx-ai-factory/nke). |
