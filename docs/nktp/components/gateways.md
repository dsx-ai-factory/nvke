# Gateways

NKTP runs one gateway per signal. Each gateway accepts OTLP over gRPC and HTTP
at its Service in the Helm release namespace, forwards what it receives to LGTM
by default, and sends its own metrics, logs and traces through its
[sidecar](../architecture/self-telemetry.md) to watchdog.

| Gateway | Values key | gRPC | HTTP |
| --- | --- | --- | --- |
| Logs | `gateway_logs` | `nktp-gateway-logs-collector:4317` | `http://nktp-gateway-logs-collector:4318` |
| Metrics | `gateway_metrics` | `nktp-gateway-metrics-collector:4317` | `http://nktp-gateway-metrics-collector:4318` |
| Traces | `gateway_traces` | `nktp-gateway-traces-collector:4317` | `http://nktp-gateway-traces-collector:4318` |

Use `<gateway>.otelcol.config` for Collector configuration and
`<gateway>.kubernetes` for Kubernetes workload configuration. Autoscaling
requires the Kubernetes resource metrics API. Retain `GOMEMLIMIT` and `GOGC` when
replacing `<gateway>.kubernetes.env`.

To export to a production backend, see
[Set a backend](../getting-started.md#set-a-backend) and
[Secure gateway export](../guides/secure-gateway-export.md).
