# Getting started with NKTP

NKTP is one Helm chart that collects a Kubernetes cluster's metrics, logs and
traces and exports them over OTLP. This guide installs it, connects it to a
backend, and covers the settings that most often differ from one cluster to the
next. The [chart README](../../nktp/deploy/charts/nktp/README.md) summarizes what the chart
deploys, and the [topology](architecture/topology.md) links the page that covers
each component and its values. On an NKE site controller, `nke-sbom` deploys
NKTP for you; see [Telemetry on a site controller](site-telemetry/getting-started.md).

## Before you install

The full list is in [Prerequisites](../../nktp/deploy/charts/nktp/README.md#prerequisites). The ones
that most often need preparing:

- **cert-manager.** The default values need it. You can run without it; see
  [Install without cert-manager](guides/configure-cert-manager.md#installing-nktp-without-cert-manager).
- **The resource metrics API** (`metrics.k8s.io`), which the Collector
  autoscalers read. NKTP can install metrics-server if nothing provides it.
- **The Operator's admission webhook must be reachable** from the API server on
  TCP 9443. Managed clusters can block it with firewall rules or security groups.
- **Capacity.** The defaults are sized for large clusters: about 10 CPU and
  26 GiB of memory in requests for the cluster-wide components, plus about
  1.1 CPU and 2.2 GiB on every node. These totals include each Collector's
  [self-telemetry sidecar](architecture/self-telemetry.md). See
  [Size resources](guides/size-resources.md).

## Install

Install a released chart from GHCR:

```bash
kubectl create namespace nktp
kubectl label namespace nktp pod-security.kubernetes.io/enforce=privileged
helm install nktp oci://ghcr.io/dsx-ai-factory/nke/charts/nktp \
  --version <version> --namespace nktp --values my-values.yaml --wait
```

To install from a checkout instead, build the chart's dependencies first, from
the repository root. Their archives aren't committed:

```bash
make nktp-chart-deps
kubectl create namespace nktp
kubectl label namespace nktp pod-security.kubernetes.io/enforce=privileged
helm install nktp ./nktp/deploy/charts/nktp --namespace nktp --values my-values.yaml --wait
```

- **The namespace needs the `privileged` Pod Security level,** because the node
  Collector and node-exporter use host paths and namespaces. `--create-namespace`
  would create it without the label.
- **Wait twice.** `helm install --wait` returns once Helm's own workloads are
  ready, but the Operator creates the Collectors after that, so a green Helm
  install can still have crash-looping Collectors.
- **`my-values.yaml`** holds your settings from the sections below. Leave it
  out to install with the defaults.
- **Versions:** each release is an `nktp/vX.Y.Z` tag; see
  [RELEASE.md](../../RELEASE.md).

## First look

By default everything exports to a bundled development backend, `lgtm`, in the
release namespace: Grafana, Prometheus, Loki and Tempo in one Pod. It keeps data
in memory and Grafana grants anonymous admin access, so it is for evaluation
only.

Grafana is on port 3000 of the `lgtm` Service, with Prometheus, Loki and Tempo
as data sources for metrics, logs and traces. Port-forward it, bound to
localhost:

```bash
kubectl -n nktp port-forward svc/lgtm 3000:3000 --address 127.0.0.1
```

See [the default dashboards](../../nktp/deploy/charts/nktp/dashboards/README.md#default-autoloaded-into-lgtm)
for the dashboards that are included by default.

## Set a backend

Everything leaves the cluster through five Collectors: the three gateways, one
per signal, the watchdog, which carries the other Collectors' own telemetry,
and the watchdog's [sidecar](architecture/self-telemetry.md), which carries the
watchdog's own telemetry. Point all of them at your backend and turn off the
default LGTM backend. Your backend must accept OTLP over HTTP for metrics, logs
and traces, and accept delta-temporality metrics, which the Target Allocators
send.

```yaml
otel-lgtm:
  enabled: false
watchdog:
  otelcol: {config: {exporters: {otlp_http: {endpoint: https://<backend>}}}}
  sidecar:
    otelcol: {config: {exporters: {otlp_http: {endpoint: https://<backend>}}}}
gateway_logs:
  otelcol: {config: {exporters: {otlp_http: {endpoint: https://<backend>}}}}
gateway_metrics:
  otelcol: {config: {exporters: {otlp_http: {endpoint: https://<backend>}}}}
gateway_traces:
  otelcol: {config: {exporters: {otlp_http: {endpoint: https://<backend>}}}}
```

- **TLS and mTLS** to the backend: see [Secure gateway export](guides/secure-gateway-export.md).
- **Credentials** go in a Secret, never in values. Add it to the Collector's
  `kubernetes.env` and reference it from the exporter with `${env:NAME}`, for
  example in `headers`. `env` is a list, so restate the chart's `GOMEMLIMIT`
  and `GOGC` entries.

## Scraping your services

NKTP collects every Pod's logs and state, and the nodes' metrics, without
being asked. A service's own metrics are scraped only when it has a
`ServiceMonitor` or `PodMonitor`, which can live in any namespace and needs no
NKTP-specific label. If a NetworkPolicy guards the metrics port, admit the
release namespace. A monitor that needs credentials references a Secret, which
the node Target Allocator reads only in the namespaces listed in
`daemonset.kubernetes.targetAllocator.prometheusCR.secretNamespaces`; see
[Node collector](components/node-collector.md#monitor-credentials).
[Send telemetry to NKTP](guides/send-telemetry.md) is the guide to share
with service owners.

## Upgrading

Helm never upgrades CRDs it installed from a chart's `crds/` directory. The
OpenTelemetry and Prometheus CRDs come from there, so a chart version that bumps
the Operator leaves the old CRDs in place, and fields only the new version knows
are silently dropped. Apply the new CRDs before `helm upgrade`.
