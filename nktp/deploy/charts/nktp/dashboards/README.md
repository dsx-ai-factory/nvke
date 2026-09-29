# NKTP dashboards

Grafana dashboards for NKTP's own pipeline. With `otel-lgtm.enabled: true`, the
chart default, every dashboard in `default/` is provisioned into the bundled
Grafana. Grafana reads them at start, so changing one rolls the `lgtm` Pod. To
use them with another Grafana, import the JSON and select a Prometheus data
source.

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
