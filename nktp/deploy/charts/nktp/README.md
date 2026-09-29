# NKTP

NKTP is an OpenTelemetry pipeline for Kubernetes metrics, logs and traces,
packaged as one Helm chart. It deploys the OpenTelemetry Operator, its CRDs,
the Prometheus monitoring CRDs, and a set of Collectors with fixed roles. It
also bundles node-exporter, kube-state-metrics and an all-in-one development
backend, all on by default. The backend is for evaluation; a site points the
exporters at its own.

The default values run two replicas of every Collector except the node
DaemonSet, and the gateways, the watchdog and the large-target pool autoscale on
CPU and memory. Every signal is exported over OTLP/HTTP to one endpoint, the
bundled backend until a site sets its own. A smaller cluster, such as a
single-node site controller, lowers the replicas and requests in its values; the
templates carry the full topology.

The chart version is `version:` in [Chart.yaml](Chart.yaml). A release tag
`nktp/vX.Y.Z` has to equal it; see [RELEASE.md](../../../../RELEASE.md).

`make nktp-chart-check`, from the repository root, downloads the dependencies,
lints and renders the chart, and validates the rendered manifests with
kubeconform. The merge gate runs it on every pull request.

## Install

```bash
kubectl create namespace nktp
kubectl label namespace nktp pod-security.kubernetes.io/enforce=privileged
helm install nktp oci://ghcr.io/dsx-ai-factory/nke/charts/nktp \
  --version <version> --namespace nktp
```

The label is the Pod Security level in [Prerequisites](#prerequisites);
`--create-namespace` would create the namespace without it. Released versions
are listed on the repository's GitHub Releases, as `nktp/vX.Y.Z` tags.

## What it deploys

Every Collector below is an `OpenTelemetryCollector` resource that the bundled
Operator turns into a workload. Each one sends its own metrics, logs and traces
to the watchdog; the watchdog sends its own to the backend.

| Values key | Role |
|---|---|
| `watchdog` | Receives the other Collectors' own telemetry over OTLP at `nktp-watchdog-collector:4318` and forwards it to the backend. Scrapes the Operator and the Target Allocators. |
| `gateway_logs`, `gateway_metrics`, `gateway_traces` | The OTLP entry points, one per signal, at `nktp-gateway-<signal>-collector:4317` (gRPC) and `:4318` (HTTP). Forward to the backend. Two replicas each by default, autoscaling up to 64. |
| `daemonset` | One Collector per node. Reads pod logs under `/var/log/pods`, scrapes the local kubelet and its cAdvisor endpoint, collects per-process metrics from `/proc`, scrapes the targets a node-local Target Allocator assigns from every monitor in the cluster, and forwards each signal to its gateway. Runs as root because of the host log mount. |
| `large_target_metrics` | A pool with its own Target Allocator for monitors labelled `nktp.nvidia.io/large-target: "true"`, which is how kube-state-metrics is scraped. Two replicas by default, autoscaling up to 16. |
| `cluster_events` | Turns Kubernetes Events into logs. Two replicas by default: a Lease the chart creates in the release namespace elects the reader, and the other stands by. |
| `kube-state-metrics` | A DaemonSet for node-local Pod metrics, one Deployment for Pods awaiting a node, and five Deployments for the other object types, grouped as workloads, networking, storage, configuration and cluster-scoped objects. RBAC is derived from the resource types. Each group can be sharded for larger clusters. |
| `node-exporter` | The upstream DaemonSet on host network port 9100, with a ServiceMonitor the node Target Allocator picks up. The only source of node-level CPU, memory, filesystem and network metrics; the node Collector's host metrics receiver reports processes, not the node. |
| `metrics-server` | The resource metrics API the autoscalers read. Off by default, because most clusters already have a provider; see [Bundled components](#bundled-components). |
| `otel-lgtm` | Grafana, Loki, Tempo and Prometheus in one container, with the NKTP dashboards. On by default for evaluation; see [The backend](#the-backend). |

Footprint with default values on three nodes, before the Operator, the three
Target Allocators and the development backend:

| Workload | Pods | CPU request | Memory request |
|---|---|---|---|
| Node Collector | 3 | 300m | 768Mi |
| Watchdog | 2 | 200m | 512Mi |
| Gateways | 6 | 1200m | 3Gi |
| Large-target pool | 2 | 1000m | 16Gi |
| Cluster event reader | 2 | 400m | 1Gi |
| node-exporter | 3 | none set | none set |
| kube-state-metrics | 9 | 900m | 1.1Gi |

## Prerequisites

- Helm 4. The repository pins the version in `make/tool-versions.mk`.
- Network access to the chart repositories named in [Chart.yaml](Chart.yaml)
  when building dependencies.
- A cluster that accepts the `opentelemetry.io/v1beta1` API. The chart installs
  the OpenTelemetry and Prometheus Operator CRDs by default.
- A release namespace whose Pod Security admission level is `privileged`. The
  node Collector runs as root and mounts `/var/log/pods` and `/proc` from the
  host, which `baseline` rejects.
- For anything beyond evaluation, a backend that accepts OTLP over HTTP for
  metrics, logs and traces. The bundled Collector image exports OTLP only. See
  [The backend](#the-backend).

## Configuration

Top-level keys in [values.yaml](values.yaml):

| Key | What it sets |
|---|---|
| `global.imageRegistry` | A registry that replaces the registry of every image the chart and its dependencies pull, apart from two; see [Images](#images). |
| `otelcol_image` | The Collector image every workload uses unless it sets `<workload>.otelcol.image`, as `registry`, `repository` and `tag`. |
| `watchdog`, `gateway_logs`, `gateway_metrics`, `gateway_traces`, `daemonset`, `large_target_metrics`, `cluster_events` | One block per Collector. `otelcol.config` is the Collector configuration, and `daemonset.kubelet` selects the kubelet endpoints the node Collector scrapes; `kubernetes` is the workload: replicas, resources, environment, Target Allocator, scheduling and, for every Collector but the node DaemonSet and the event reader, the `autoscaler` bounds. |
| `kube-state-metrics` | `enabled`, the node DaemonSet, the central `deployments` with their `resource_types`, `shards` and `namespaces`, and the image. |
| `otel-lgtm` | The development backend: `enabled` and `image`. |
| `opentelemetry-kube-stack`, `node-exporter`, `metrics-server` | Values passed through to the bundled charts. |

### Images

Every image is set as `registry`, `repository` and `tag`. An empty `registry`
pulls from the image's default registry. `global.imageRegistry` replaces the
registry of every image at once, for a mirror. Two images do not read it: the
metrics-server dependency's and the OpenTelemetry Operator's, which is nested in
opentelemetry-kube-stack. Point those at the mirror through their own values:

```yaml
metrics-server:
  image:
    repository: <mirror>/metrics-server/metrics-server
opentelemetry-kube-stack:
  opentelemetry-operator:
    manager:
      image:
        repository: <mirror>/open-telemetry/opentelemetry-operator/opentelemetry-operator
```

### Kubelet endpoints

The node Collector scrapes its kubelet's `/metrics` and `/metrics/cadvisor`.
cAdvisor applies kube-prometheus-stack's metric relabelings, which drop
always-zero, overlapping and host-network-interface series and cgroups with no
Pod; `daemonset.kubelet.cadvisor.metricRelabelings` replaces them.
`/metrics/probes` and `/metrics/resource` are off. Each endpoint is switched on
or off independently:

```yaml
daemonset:
  kubelet:
    cadvisor:
      enabled: false
    probes:
      enabled: true
    resource:
      enabled: true
```

### The backend

Every exporter ships pointed at the bundled development backend at
`http://lgtm:4318` in the release namespace: the `otlp_http` exporter of the
watchdog and of each gateway, and the three `service.telemetry` exporters in
the watchdog block. Grafana is on port 3000 of the `lgtm` Service in the release
namespace, with anonymous admin access, and Prometheus on port 9090. That
backend keeps its data in memory, has no authentication and does not scale, so
it is for evaluation only.

A site points those endpoints at its own backend and turns the bundled one
off. The backend has to accept OTLP over HTTP for metrics, logs and traces;
the bundled Collector image exports OTLP and nothing else. A values file for a
site therefore sets at least:

```yaml
otel-lgtm:
  enabled: false
watchdog:
  otelcol:
    config:
      exporters:
        otlp_http:
          endpoint: https://<backend>
      service:
        telemetry:
          metrics:
            readers:
              - periodic:
                  exporter:
                    otlp:
                      protocol: http/protobuf
                      endpoint: https://<backend>
          logs:
            processors:
              - batch:
                  exporter:
                    otlp:
                      protocol: http/protobuf
                      endpoint: https://<backend>
          traces:
            processors:
              - batch:
                  exporter:
                    otlp:
                      protocol: http/protobuf
                      endpoint: https://<backend>
gateway_logs:
  otelcol:
    config:
      exporters:
        otlp_http:
          endpoint: https://<backend>
gateway_metrics:
  otelcol:
    config:
      exporters:
        otlp_http:
          endpoint: https://<backend>
gateway_traces:
  otelcol:
    config:
      exporters:
        otlp_http:
          endpoint: https://<backend>
```

Credentials belong in a Secret referenced from the workload's `kubernetes.env`,
read by the Collector through `${env:NAME}` substitution, never in values.

### Bundled components

- `kube-state-metrics` runs as one node DaemonSet for scheduled Pods, one
  Deployment for Pods awaiting a node, and one Deployment per group of the
  other object types under `kube-state-metrics.deployments`. Their PodMonitors
  carry the large-target label, so the pool scrapes them. Raise a group's
  `shards` and `large_target_metrics.kubernetes.resources` when the object
  count grows. Secret metrics stay opt-in because they need list
  and watch access to whole Secret objects; add `secrets` to a group's
  `resource_types` and scope its `namespaces` list to enable them.
  `kube-state-metrics.enabled: false` removes all of it.
- `node-exporter` runs the upstream DaemonSet on host port 9100.
  `node-exporter.enabled: false` when the cluster already runs one.
- `otel-lgtm` is the development backend described above.
  `otel-lgtm.enabled: false` for a site.
- `metrics-server` serves `metrics.k8s.io`, which is a cluster-wide singleton:
  one APIService object, owned by one provider. It is off by default. Turn it
  on with `metrics-server.enabled: true` only where nothing else provides the
  API, such as a bare control-plane cluster. Two providers flip the
  APIService between backends and the autoscalers lose their metrics, so turn
  it off again if the cluster gets another one.

## Bundled dependencies

| Dependency | Pin | Notes |
|---|---|---|
| opentelemetry-kube-stack | 0.22.0 | Operator chart 0.123.0, operator image 0.159.0, and the CRDs. Always installed. |
| prometheus-node-exporter, aliased `node-exporter` | 4.56.1 | On by default. `node-exporter.enabled: false` when the cluster already runs one. |
| metrics-server | 3.13.1 | Off by default. `metrics-server.enabled: true` when nothing else serves `metrics.k8s.io`. |

Pins live in [Chart.yaml](Chart.yaml) and [Chart.lock](Chart.lock). Bump them
in their own pull request with `helm dependency update` and a chart version bump.

When another mechanism owns the Operator or the CRDs, turn them off independently:

```yaml
opentelemetry-kube-stack:
  opentelemetry-operator:
    enabled: false
  crds:
    installOtel: false
    installPrometheus: false
```

Helm generates the Operator's webhook certificate and reuses it on upgrade. It
is valid for 365 days and not renewed. On clusters with cert-manager, set
`opentelemetry-kube-stack.opentelemetry-operator.admissionWebhooks.certManager.enabled: true`.
