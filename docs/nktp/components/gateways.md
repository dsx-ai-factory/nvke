# Gateways

NVKE telemetry pipeline (NKTP) runs one gateway per signal. Each gateway accepts OTLP over gRPC with mTLS
at its Service in the Helm release namespace, forwards what it receives to LGTM
by default, and sends its own metrics, logs and traces through its
[sidecar](../architecture/self-telemetry.md) to watchdog.

| Gateway | Values key | gRPC |
| --- | --- | --- |
| Logs | `gateway_logs` | `nktp-gateway-logs-collector:4317` |
| Metrics | `gateway_metrics` | `nktp-gateway-metrics-collector:4317` |
| Traces | `gateway_traces` | `nktp-gateway-traces-collector:4317` |

**Breaking change for HTTP clients:** the default gateway HTTP receivers on port
4318 have been removed. Existing clients must switch to OTLP/gRPC on port 4317,
or explicitly configure an HTTP receiver with TLS under
`<gateway>.otelcol.config.receivers.otlp.protocols.http`. Changing a client's
port alone does not change its protocol. DaemonSet HTTP ingress and sidecar
loopback HTTP receivers are unchanged.

## Collector connections

Node Collectors send logs, metrics and traces to their gateways over OTLP/gRPC
with mutual TLS.
The large-target pool sends metrics over gRPC, and the cluster-events reader
sends logs over gRPC. All three gateways send GOAWAY at a connection age of five
minutes, with +/-10% jitter, so clients reconnect through the Service and can
reach newly added replicas. In-flight exports get another 30 seconds to finish
before forced closure. See
[gRPC connection management](https://github.com/grpc/proposal/blob/master/A9-server-side-conn-mgt.md).

Five minutes limits routine connection churn while periodically redistributing
telemetry. Tune `max_connection_age` under
`<gateway>.otelcol.config.receivers.otlp.protocols.grpc.keepalive.server_parameters`
based on gateway headroom and acceptable delay after scale-up. Lower it for
faster relief; raise it to reduce TCP setup and TLS handshakes.
Reconnects can select the same gateway, so redistribution can take multiple
rotations. Increase `max_connection_age_grace` if in-flight exports need longer
to finish. Explicitly configured HTTP receivers do not use these gRPC age settings.

Exporter overrides use `daemonset.otelcol.config.exporters.otlp/logs`,
`otlp/metrics` and `otlp/traces`, and `large_target_metrics.otelcol.config.exporters.otlp`
and `cluster_events.otelcol.config.exporters.otlp`.

Use `<gateway>.otelcol.config` for Collector configuration and
`<gateway>.kubernetes` for Kubernetes workload configuration. Autoscaling
requires the Kubernetes resource metrics API. Retain `GOMEMLIMIT` and `GOGC` when
replacing `<gateway>.kubernetes.env`.

Each gateway sets `k8s.cluster.name` from `global.clusterName` with its
`transform/cluster` processor, unless the telemetry already has one. When
`global.clusterName` is empty, the value is `unknown`. The chart passes the name
in the `K8S_CLUSTER_NAME` environment variable, and the processor reads it
inside an OTTL string, so names such as `42` or `true` stay strings. The name
must not contain a double quote or a backslash.

To export to a production backend, see
[Set a backend](../getting-started.md#set-a-backend) and
[Secure gateway export](../guides/secure-gateway-export.md).
