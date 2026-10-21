# Large-target metrics

The large-target metrics pool (`large_target_metrics`) selects ServiceMonitor,
PodMonitor, Probe, and ScrapeConfig resources with the metadata label
`nktp.nvidia.io/large-target: "true"`.
Apply this label to monitoring resources for endpoints with large scrape
responses, such as kube-state-metrics.

A Target Allocator distributes scrapes across the pool's replicas.
The Target Allocator rejects ServiceMonitor and PodMonitor endpoints that reference
local bearer-token or TLS files in the Collector container.
Setting `large_target_metrics.kubernetes.targetAllocator.enabled` to `false`
will cause Helm rendering to fail. Target Allocator telemetry is sent to watchdog.
The allocator has its own security contexts and resource budget under
`large_target_metrics.kubernetes.targetAllocator`. Size its resources for the
discovered Kubernetes objects and targets.

Collected metrics go to `nktp-gateway-metrics-collector:4317` over OTLP/gRPC.
Each Collector sends its own telemetry through its
[sidecar](../architecture/self-telemetry.md) to watchdog.
Use `large_target_metrics.otelcol.config` for Collector internal configuration and
`large_target_metrics.kubernetes` for workload configuration.

## Sizing large scrapes

This pool reserves more memory than the node DaemonSet for large scrape
responses. Autoscaling requires the Kubernetes resource metrics API and
distributes whole targets. An individual endpoint must still fit in one
Collector. Scrape defaults limit each response to 1,000,000 samples and 1 GiB.
Split endpoints that exceed these limits into smaller scrape targets where
possible. If raising the scrape limits, size the exporter queue, memory request
and limit, `GOMEMLIMIT`, and memory limiter thresholds together. Response bytes
do not directly measure the memory needed to parse and export a scrape.
Retain `GOMEMLIMIT` and `GOGC` when replacing environment lists.
For large targets scraped every 60 seconds, consider `scrapeTimeout: 55s` on the
ServiceMonitor or PodMonitor endpoint. Keep the timeout below the scrape interval.

## Kubernetes component metrics

NVKE telemetry pipeline (NKTP) enables the kube-stack ServiceMonitors for kube-apiserver,
kube-controller-manager, kube-scheduler, etcd, CoreDNS, and kube-proxy. They are
labeled `nktp.nvidia.io/large-target: "true"` and selected by the large-target
metrics Target Allocator. Component availability, labels, ports, and network
exposure vary between Kubernetes distributions, so a rendered ServiceMonitor
does not by itself guarantee a reachable target. The large-target Collector's
service account has read-only authorization for the `/metrics` non-resource URL.

The ServiceMonitors from opentelemetry-kube-stack 0.25.1 reference bearer-token
and, for secure endpoints, CA files in the Collector container. The large-target
Target Allocator therefore sets `denyFSAccessThroughSMs: false`. Setting it to
`true` causes these ServiceMonitor endpoints to be dropped, as described in
[opentelemetry-helm-charts issue #2408](https://github.com/open-telemetry/opentelemetry-helm-charts/issues/2408).
