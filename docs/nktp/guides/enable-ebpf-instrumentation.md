# Enable eBPF instrumentation

[OpenTelemetry eBPF Instrumentation](https://opentelemetry.io/docs/zero-code/obi/)
(OBI) observes requests, such as HTTP and gRPC, that services on a node send
and receive. It produces traces and request rate, error, and duration (RED)
metrics for them, for services in any language and without changes to their
code or images.

OBI runs as a privileged DaemonSet and needs a Linux kernel with BTF support on
each node. Install it with its Helm chart, as described in
[Deploy OBI in Kubernetes with Helm](https://opentelemetry.io/docs/zero-code/obi/setup/kubernetes-helm/).
Point its OTLP exporter at the node collector,
`http://nktp-daemonset-collector.nktp:4318`, and
[select the workloads](https://opentelemetry.io/docs/zero-code/obi/configure/service-discovery/)
to observe. In the endpoint, replace only the namespace after the dot
(`.nktp`) with the release namespace. Keep the `nktp-daemonset-collector` prefix.

When a service also uses [auto-instrumentation](enable-auto-instrumentation.md),
both report its requests. Distinguish the two sources in dashboards to avoid
counting requests twice.
