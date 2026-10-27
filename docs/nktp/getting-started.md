# Getting started with NKTP

NVKE telemetry pipeline (NKTP) is one Helm chart that collects a Kubernetes cluster's metrics, logs and
traces and exports them over OTLP. This guide installs it, connects it to a
backend, and covers the settings that most often differ from one cluster to the
next. The [chart README](https://github.com/dsx-ai-factory/nvke/blob/main/nktp/deploy/charts/nktp/README.md) summarizes what the chart
deploys, and the [topology](architecture/topology.md) links the page that covers
each component and its values. On an NVKE management cluster, `deploy/nico` deploys
NKTP for you; see [Management cluster telemetry: getting started](site-telemetry/getting-started.md).

## Before you install

The full list is in [Prerequisites](https://github.com/dsx-ai-factory/nvke/blob/main/nktp/deploy/charts/nktp/README.md#prerequisites). The ones
that most often need preparing:

- **cert-manager.** Required with default values; install it separately using
  the [cert-manager installation instructions](https://cert-manager.io/docs/installation/).
  To run without it, see
  [Install without cert-manager](guides/configure-cert-manager.md#installing-nktp-without-cert-manager).
- **The resource metrics API** (`metrics.k8s.io`), which the Collector
  autoscalers read. NKTP can install metrics-server if nothing provides it.
- **The Operator's admission webhook must be reachable** from the API server on
  TCP 9443. Managed clusters can block it with firewall rules or security groups.
- **Capacity.** The defaults are sized for large clusters. Budget for the
  Collectors, [self-telemetry sidecars](architecture/self-telemetry.md),
  Target Allocators and bundled dependencies, including autoscaling growth.
  See [Size resources](guides/size-resources.md).

## Install

Install a released chart from GHCR. If `helm` answers `unauthorized`, run
`helm registry login ghcr.io` first:

```bash
kubectl create namespace nktp
kubectl label namespace nktp pod-security.kubernetes.io/enforce=privileged
helm install nktp oci://ghcr.io/dsx-ai-factory/nvke/charts/nktp \
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
- **Wait for the Collectors too.** `--wait` covers only the release's own
  resources, and the Operator creates the Collector workloads after it returns.
  Run
  `kubectl get opentelemetrycollector -n nktp` and repeat until every
  Collector's `READY` column shows all expected replicas ready, such as `2/2`.
  If readiness stalls, inspect the Collector Pods with
  `kubectl get pods -n nktp`.
  When cert-manager is installed, also run `kubectl get certificate -n nktp`
  and wait for every NKTP Certificate to show `READY=True`. If certificate
  readiness stalls, inspect the conditions
  with `kubectl describe certificate -n nktp`.
- **`my-values.yaml`** holds your settings from the sections below. Leave it
  out to install with the defaults.
- **Versions:** each release is an `nktp/vX.Y.Z` tag, published as the GitHub
  release `nktp X.Y.Z` on the repository's
  [Releases](https://github.com/dsx-ai-factory/nvke/releases) page. Pass `X.Y.Z`
  to `--version`. [RELEASE.md](https://github.com/dsx-ai-factory/nvke/blob/main/RELEASE.md)
  describes the process.

<details>
<summary>Example Collector and certificate readiness with cert-manager installed</summary>

```bash
kubectl get opentelemetrycollector -A
kubectl get certificate -A
```

Example output for chart version `0.1.0`; names and replica counts depend on the
chart version and values. `READY=2/2` means both expected Collector replicas
are ready; certificates should show `READY=True`.

```text
NAMESPACE   NAME                  MODE          VERSION   READY   AGE   IMAGE                                      MANAGEMENT
nktp        nktp-cluster-events   statefulset   0.161.0   2/2     23h   otel/opentelemetry-collector-k8s:0.162.0   managed
...
```

```text
NAMESPACE   NAME                         READY   SECRET                          AGE
nktp        nktp-cluster-events-client   True    nktp-cluster-events-client-tls  23h
...
```

</details>

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

See [the default dashboards](https://github.com/dsx-ai-factory/nvke/blob/main/nktp/deploy/charts/nktp/dashboards/README.md#default-autoloaded-into-lgtm)
for the dashboards that are included by default.

## Set a backend

Everything leaves the cluster through five Collectors: the three gateways, one
per signal, the watchdog, which carries the other Collectors' own telemetry,
and the watchdog's [sidecar](architecture/self-telemetry.md), which carries the
watchdog's own telemetry. Point all of them at your backend and turn off the
default LGTM backend. The default exporters use OTLP/gRPC. Check which protocol
your backend accepts for each signal; see [Secure gateway export](guides/secure-gateway-export.md)
for HTTP configuration. The metrics backend must also accept delta-temporality
metrics, which the Target Allocators send.

```yaml
otel-lgtm:
  enabled: false
watchdog:
  otelcol: {config: {exporters: {otlp: {endpoint: "<backend>:4317", tls: {insecure: false}}}}}
  sidecar:
    otelcol: {config: {exporters: {otlp: {endpoint: "<backend>:4317", tls: {insecure: false}}}}}
gateway_logs:
  otelcol: {config: {exporters: {otlp: {endpoint: "<backend>:4317", tls: {insecure: false}}}}}
gateway_metrics:
  otelcol: {config: {exporters: {otlp: {endpoint: "<backend>:4317", tls: {insecure: false}}}}}
gateway_traces:
  otelcol: {config: {exporters: {otlp: {endpoint: "<backend>:4317", tls: {insecure: false}}}}}
```

- **TLS and mTLS** to the backend: see [Secure gateway export](guides/secure-gateway-export.md).
- **Credentials** go in a Secret, never in values. Add it to the Collector's
  `kubernetes.env` and reference it from the exporter with `${env:NAME}`, for
  example in `headers`. `env` is a list, so restate the chart's `GOMEMLIMIT`
  and `GOGC` entries.

## Set the cluster name

Set `global.clusterName` so that telemetry from this cluster carries
`k8s.cluster.name`. Without it, NKTP sets `unknown`.

```yaml
global:
  clusterName: <cluster>
```

The gateways, the watchdog and the watchdog sidecar add the attribute to
telemetry that does not already have one.

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
