# NKTP

NKTP is an OpenTelemetry pipeline for Kubernetes metrics, logs and traces,
packaged as one Helm chart. It deploys the OpenTelemetry Operator, its CRDs,
the Prometheus monitoring CRDs, and a set of Collectors with fixed roles. It
also bundles node-exporter, kube-state-metrics and an all-in-one development
backend, all on by default. The backend is for evaluation; a site points the
exporters at its own.

The default values are sized for the site-controller
with minimal telemetry: every Collector starts at one replica and autoscales
on CPU and memory, and every signal is exported over OTLP/HTTP to one
endpoint, the bundled backend until a site sets its own. Everything larger is a values change; the templates carry
the full topology.

The chart version is `version:` in [Chart.yaml](Chart.yaml). A release tag
`nktp/vX.Y.Z` has to equal it; see [RELEASE.md](../../../../RELEASE.md).

`make nktp-chart-check`, from the repository root, downloads the dependencies,
lints and renders the chart, and validates the rendered manifests with
kubeconform. The merge gate runs it on every pull request.

## What it deploys

Every Collector below is an `OpenTelemetryCollector` resource that the bundled
Operator turns into a workload. Each one sends its own metrics, logs and traces
to the watchdog; the watchdog sends its own to the backend.

| Values key | Role |
|---|---|
| `watchdog` | Receives the other Collectors' own telemetry over OTLP at `nktp-watchdog-collector:4318` and forwards it to the backend. Scrapes the Operator and the Target Allocators. |
| `gateway_logs`, `gateway_metrics`, `gateway_traces` | The OTLP entry points, one per signal, at `nktp-gateway-<signal>-collector:4317` (gRPC) and `:4318` (HTTP). Forward to the backend. One replica each by default, autoscaling up to 64. |
| `daemonset` | One Collector per node. Reads pod logs under `/var/log/pods`, scrapes the local kubelet including cAdvisor, collects per-process metrics from `/proc`, with `SYS_PTRACE`, `DAC_READ_SEARCH` and the node's `/etc/passwd` mounted so executable paths, I/O counters and host-user owners resolve, and records each process's cgroup so container processes attribute to their pod, scrapes the targets a node-local Target Allocator assigns from every monitor in the cluster, and forwards each signal to its gateway. Runs as root because of the host log mount. |
| `large_target_metrics` | A pool with its own Target Allocator for monitors labelled `nktp.nvidia.io/large-target: "true"`, which is how kube-state-metrics is scraped. One replica by default, autoscaling up to 16, sized for a small cluster. |
| `cluster_events` | Turns Kubernetes Events into logs. One replica by default; a Lease the chart creates in the release namespace elects the reader when there are more. |
| `kube-state-metrics` | A DaemonSet for node-local Pod metrics, one Deployment for Pods awaiting a node, and one Deployment for every other object type. RBAC is derived from the resource types. The chart can split the central types across sharded Deployments for larger clusters. |
| `node-exporter` | The upstream DaemonSet on host network port 9100, with a ServiceMonitor the node Target Allocator picks up. The only source of node-level CPU, memory, filesystem and network metrics; the node Collector's host metrics receiver reports processes, not the node. |
| `metrics-server` | The resource metrics API the autoscalers read. On by default because a control-plane cluster has no other provider of it. |
| `otel-lgtm` | Grafana, Loki, Tempo and Prometheus in one container, with the NKTP dashboards. On by default for evaluation; see [The backend](#the-backend). |

Footprint with default values on three nodes, before the Operator and the three
Target Allocators, which run with the Operator's own defaults:

| Workload | Pods | CPU request | Memory request |
|---|---|---|---|
| Node Collector | 3 | 300m | 768Mi |
| Watchdog | 1 | 100m | 256Mi |
| Gateways | 3 | 600m | 1.5Gi |
| Large-target pool | 1 | 100m | 256Mi |
| Cluster event reader | 1 | 200m | 512Mi |
| node-exporter | 3 | none set | none set |
| kube-state-metrics | 5 | 125m | 320Mi |
| Development backend | 1 | none set | none set |
| metrics-server | 1 | 100m | 200Mi |

## Prerequisites

- Helm 4. The repository pins the version in `make/tool-versions.mk`.
- Network access to the chart repositories named in [Chart.yaml](Chart.yaml)
  when building dependencies.
- A cluster that accepts the `opentelemetry.io/v1beta1` API. The chart installs
  the OpenTelemetry and Prometheus Operator CRDs by default.
- A release namespace whose Pod Security admission level is `privileged`. The
  node Collector runs as root with `SYS_PTRACE` and `DAC_READ_SEARCH` and mounts `/var/log/pods`,
  `/proc` and `/etc/passwd` from the host, which `baseline` rejects.
- For anything beyond evaluation, a backend that accepts OTLP over HTTP for
  metrics, logs and traces. The bundled Collector image exports OTLP only. See
  [The backend](#the-backend).

## Configuration

Top-level keys in [values.yaml](values.yaml):

| Key | What it sets |
|---|---|
| `otelcol_image` | The Collector image every workload uses unless it sets `<workload>.otelcol.image`. |
| `watchdog`, `gateway_logs`, `gateway_metrics`, `gateway_traces`, `daemonset`, `large_target_metrics`, `cluster_events` | One block per Collector. `otelcol.config` is the Collector configuration; `kubernetes` is the workload: replicas, resources, environment, Target Allocator, scheduling and, for every Collector but the node DaemonSet and the event reader, the `autoscaler` bounds. |
| `kube-state-metrics` | `enabled`, the node DaemonSet, the central `deployments` with their `resource_types`, `shards` and `namespaces`, and the image. |
| `otel-lgtm` | The development backend: `enabled` and `image`. |
| `opentelemetry-kube-stack`, `node-exporter`, `metrics-server` | Values passed through to the bundled charts. |

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
  Deployment for Pods awaiting a node, and one Deployment named `objects` for
  every other object type, each with small requests. Their PodMonitors carry
  the large-target label, so the pool scrapes them. Split `objects` into
  several groups and raise `large_target_metrics.kubernetes.resources` when
  the object count grows. Secret metrics stay opt-in because they need list
  and watch access to whole Secret objects; add `secrets` to a group's
  `resource_types` and scope its `namespaces` list to enable them.
  `kube-state-metrics.enabled: false` removes all of it.
- `node-exporter` runs the upstream DaemonSet on host port 9100.
  `node-exporter.enabled: false` when the cluster already runs one.
- `otel-lgtm` is the development backend described above.
  `otel-lgtm.enabled: false` for a site.
- `metrics-server` serves `metrics.k8s.io`, which is a cluster-wide singleton:
  one APIService object, owned by one provider. With the default on, this
  chart owns it. A cluster that already has a provider, or gets one later
  through another path, must run exactly one of the two, or the APIService
  flips between backends and the autoscalers lose their metrics.
  `metrics-server.enabled: false` hands it to the other provider.

## Bundled dependencies

| Dependency | Pin | Notes |
|---|---|---|
| opentelemetry-kube-stack | 0.22.0 | Operator chart 0.123.0, operator image 0.159.0, and the CRDs. Always installed. |
| prometheus-node-exporter, aliased `node-exporter` | 4.56.1 | On by default. `node-exporter.enabled: false` when the cluster already runs one. |
| metrics-server | 3.13.1 | On by default. `metrics-server.enabled: false` when the cluster already serves `metrics.k8s.io`. |

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
