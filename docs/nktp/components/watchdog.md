# Watchdog

The watchdog receives internal metrics, logs and traces from NKTP collectors over
OTLP/HTTP at `http://nktp-watchdog-collector:4318` in the Helm release namespace.
It forwards received telemetry to LGTM by default. Its own telemetry goes through
its [sidecar](../architecture/self-telemetry.md), which exports to LGTM by default.
It also scrapes the Operator's metrics endpoint with the `nktp-operator` job in
its `prometheus` receiver. See [Topology](../architecture/topology.md#self-telemetry)
for every source of self-telemetry.

The bundled OpenTelemetry Operator manages the watchdog. Autoscaling requires the
Kubernetes resource metrics API, commonly provided by metrics-server.
Setting `watchdog.kubernetes.targetAllocator.enabled` to `false` will cause Helm
rendering to fail.

Configure the Collector through `watchdog.otelcol.config`.

Use `watchdog.kubernetes` for Collector CR settings and `watchdog.otelcol.image`
to override the global `otelcol_image`.
Include `GOMEMLIMIT` and `GOGC` in any replacement `watchdog.kubernetes.env` list
to retain these settings.
