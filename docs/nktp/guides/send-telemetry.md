# Send telemetry to NKTP

This guide is for teams whose workloads run on a cluster with NKTP. NKTP
collects Pod logs without any action from you. Traces, pushed metrics, and
scraped metrics each need one step.

## Logs

The node collector reads every Pod's stdout and stderr from `/var/log/pods` on
its node and sends them to the logs gateway. Collection follows new records and
does not need any configuration in your workload.

## OTLP

Send OTLP logs, metrics, and traces to the node collector at
`nktp-daemonset-collector:4317` over gRPC or
`http://nktp-daemonset-collector:4318` over HTTP. Qualify the Service name with
the NKTP namespace when sending from another namespace, for example
`nktp-daemonset-collector.nktp:4317`.

The Service routes traffic only to the Collector on the sender's node. If no
local Collector is ready, traffic fails rather than reaching another node. The
Collector sets `k8s.node.name` and forwards each signal to its gateway.

## Prometheus metrics

A service's own metrics are scraped only when it has a `ServiceMonitor` or
`PodMonitor`, which can live in any namespace and needs no NKTP-specific label.
If a NetworkPolicy guards the metrics port, admit the NKTP release namespace.

The node Target Allocator assigns each target to the node collector on the
target's node. Targets without a matching collector node remain unassigned.

The Target Allocator rejects ServiceMonitor and PodMonitor endpoints that
reference local bearer-token or TLS files in the Collector container. Reference
a Secret instead. The allocator reads Secrets and CA ConfigMaps only in the
namespaces the NKTP operator lists in
`daemonset.kubernetes.targetAllocator.prometheusCR.secretNamespaces`, so ask
them to add your namespace. See
[Monitor credentials](../components/node-collector.md#monitor-credentials) for
the access this grants.

## Large scrape responses

Label the monitor `nktp.nvidia.io/large-target: "true"` when an endpoint
returns a large scrape response. The [large-target pool](../components/large-target-metrics.md)
scrapes labeled monitors instead of the node collector and reserves more memory
for each scrape.

An individual endpoint must still fit in one Collector. Scrape defaults limit
each response to 1,000,000 samples and 1 GiB. Split endpoints that exceed these
limits into smaller scrape targets where possible. For large targets scraped
every 60 seconds, consider `scrapeTimeout: 55s` on the ServiceMonitor or
PodMonitor endpoint. Keep the timeout below the scrape interval.
