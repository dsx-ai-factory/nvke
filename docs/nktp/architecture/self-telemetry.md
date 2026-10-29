# Self-Telemetry Sidecars

Each Collector Pod in NVKE telemetry pipeline (NKTP) runs a sidecar Collector. The main Collector sends its
[internal telemetry](https://opentelemetry.io/docs/collector/internal-telemetry/)
to the sidecar at `http://127.0.0.1:4319`, and the sidecar exports it to the
watchdog over OTLP/gRPC at `nktp-watchdog-collector:4317`. The watchdog's sidecar
exports to the backend.

The sidecar buffers and retries the main Collector's telemetry, and samples its
traces.

The sidecar's exporter is a normal Collector exporter, so if mTLS is enabled
between Collectors, it can reload rotated certificates.

## Why the sidecar is inline

For other services in the cluster, we recommend sidecars injected by the
OpenTelemetry Operator. However, injection does not work for Collectors that
the Operator manages. The Operator
[skips injection](https://github.com/open-telemetry/opentelemetry-operator/blob/ca0b832a4cb071d144d374b453d9560b77a7d8d7/pkg/sidecar/pod.go#L79)
when a Pod already has a container named `otc-container`, and it gives that
name to the main container of every Collector it manages. NKTP therefore
defines each sidecar in its Collector's Pod spec.

## Startup and shutdown

The sidecar is a native sidecar, an init container with `restartPolicy: Always`.
Kubernetes starts the main Collector after the sidecar's startup probe succeeds.
At shutdown, the main Collector stops first and flushes into the sidecar. A main
Collector does not start if its sidecar never becomes healthy.

The sidecar's receiver listens on `127.0.0.1` only. Its health check uses port
13134.

## The sidecar's own telemetry

Each sidecar sends its own metrics and logs to its own receiver, so they share
its queue and exporter with the main Collector's telemetry. The two streams
describe one pair of Collectors: the sidecar's queue and export metrics explain
gaps in the main Collector's data, so they are buffered and delivered together.

The sidecar does not emit traces (`level: none`). Its metrics and logs already
record failed and dropped exports, and its own spans would more than double the
spans it must sample.

## Trace selection

The sidecar's `tail_sampling` processor keeps a trace when one of these policies
matches:

- `errors`: a span has status `ERROR`.
- `slow`: a single span is longer than 1 second. Trace duration is not used,
  because export batching adds up to 1 second to most traces.

A Collector's self-telemetry spans go to the sidecar in its Pod. The sidecar
decides 5 seconds after a trace's first span arrives. The main Collector
flushes completed spans every second (`schedule_delay: 1000`), but an operation
that is still running has not sent its span yet. Late spans of a kept trace
are also kept.

When an export batches several requests, its span starts a new trace with links
to the requests' spans. A kept export trace does not keep the linked traces.

A late ERROR or slow span can be dropped if its trace was already rejected.
If this limits troubleshooting, increase `processors.tail_sampling.decision_wait`
in the affected workload's `sidecar.otelcol.config`, reviewing `num_traces` and
the memory budget together. A longer window without enough trace capacity can
increase loss through eviction before evaluation. See the
[upstream tuning guidance](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/v0.162.0/processor/tailsamplingprocessor/README.md#monitoring-and-tuning).

## Sizing

| Sidecar of | CPU / memory request | CPU / memory limit | Queue per signal | Traces held for sampling |
| --- | --- | --- | --- | --- |
| Watchdog and gateways | 20m / 64 MiB | 200m / 256 MiB | 16 MiB | 5,000 |
| Node collector, large-target collector, cluster event reader | 10m / 32 MiB | 100m / 128 MiB | 4 MiB | 1,000 |

Each sidecar has one queue per signal. Thirty minutes of a Collector's own
metrics is well under 1 MiB, so most of each queue is for error logs and spans
during an incident.

With all three queues full, queued data took 1.2 to 1.4 times its serialized
size in memory. Each limit covers the base process, three full queues and the
traces held for sampling, below the memory limiter's refusal threshold at 55%
of the limit.

The queues are in memory and do not survive a Pod restart. Autoscaler
utilization includes the sidecar's request.

## Dashboards

Each Collector's default dashboard has a "Self-telemetry sidecar" row with the
sidecar's queue use, export failures and refused items for all three signals,
sampling decisions, CPU and memory. The NKTP Health dashboard has a separate
"Self-telemetry sidecars" row. Sidecars report as their own `job`, for example
`nktp-gateway-logs-sidecar`, and no query mixes a sidecar with its Collector.

## Configuration

Each workload has a `sidecar` section:

- `<workload>.sidecar.kubernetes`: container settings such as `resources`, `env`
  and probes. The chart always sets `name`, `restartPolicy`, `image` and
  `args`, and adds `POD_NAME` to `env` when the list omits it, plus `NODE_NAME`
  for the daemonset sidecar.
- `<workload>.sidecar.otelcol.config`: the sidecar's complete Collector
  configuration, passed unchanged.
- `<workload>.sidecar.otelcol.image`: an image for this sidecar only. Defaults
  to `otelcol_image`.

To send NKTP's own telemetry to another backend, set the watchdog sidecar's
exporter endpoint, not the watchdog's `service.telemetry`:

```yaml
watchdog:
  sidecar:
    otelcol:
      config:
        exporters:
          otlp:
            endpoint: <backend>:4317
            tls:
              insecure: false
```
