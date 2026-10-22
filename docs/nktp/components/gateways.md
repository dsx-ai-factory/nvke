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
sends logs over gRPC. All three gateways send GOAWAY at a connection age of one
minute, with +/-10% jitter, so clients reconnect through the Service and can
reach newly added replicas. In-flight exports get another 30 seconds to finish
before forced closure. See
[gRPC connection management](https://github.com/grpc/proposal/blob/master/A9-server-side-conn-mgt.md).

Tune `max_connection_age` under
`<gateway>.otelcol.config.receivers.otlp.protocols.grpc.keepalive.server_parameters`
based on gateway headroom and acceptable delay after scale-up. Lower it for
faster relief. Raise it to reduce TCP setup and TLS handshakes.
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

Each gateway's sending queue holds approximately 0.48 GiB of serialized
telemetry, 12% of its default 4 GiB container limit. Processing and incoming
requests also consume heap; validate headroom with representative telemetry.
Set `exporters.otlp.sending_queue.queue_size` in the gateway's configuration
explicitly when changing its memory limit.
See OpenTelemetry's
[queue-sizing guidance](https://opentelemetry.io/docs/collector/resiliency/#sending-queue-in-memory-buffering)
and [sending queue configuration](https://github.com/open-telemetry/opentelemetry-collector/blob/v0.162.0/exporter/exporterhelper/README.md#sending-queue).

Gateways inherit `priorityClass.name` (default `nktp-telemetry-critical` at
`1000000000`, below system-critical Pods), including their sidecars; set
`priorityClass.preemptionPolicy: Never` to disable preemption. Use an existing
class with `priorityClass.create: false`, or override
`<gateway>.kubernetes.priorityClassName` per gateway (`""` opts out).

Each gateway sets `k8s.cluster.name` from `global.clusterName` with its
`transform/cluster` processor, unless the telemetry already has one. When
`global.clusterName` is empty, the value is `unknown`. The chart passes the name
in the `K8S_CLUSTER_NAME` environment variable, and the processor reads it
inside an OTTL string, so names such as `42` or `true` stay strings. The name
must not contain a double quote or a backslash.

The exporter queue is in memory by default. To keep it on a PersistentVolume,
see [Configure a persistent exporter queue](../guides/configure-persistent-queue.md).

To export to a production backend, see
[Set a backend](../getting-started.md#set-a-backend) and
[Secure gateway export](../guides/secure-gateway-export.md).
