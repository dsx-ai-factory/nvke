# NKTP Helm Chart

NVKE telemetry pipelines (NKTP) runs OpenTelemetry Collectors on a Kubernetes cluster.

The chart deploys a pipeline that collects a cluster's metrics, logs and traces
and exports them over OTLP: a Collector on every node, a scrape pool for large
metrics endpoints, a Kubernetes Event reader, one gateway per signal, and a
watchdog for the pipeline's own telemetry. The bundled OpenTelemetry Operator
manages the Collectors.

This README covers installing, configuring, and developing the chart. The NKTP
documentation, linked at the [end of this README](#more-information), covers
each component in depth.

## Prerequisites

Provide the following before installing NKTP.

- **Kubernetes 1.29+.** Every Collector Pod runs its self-telemetry sidecar as
  a native sidecar, an init container with `restartPolicy: Always`, which
  Kubernetes enables by default from 1.29. The bundled OpenTelemetry Operator
  0.159.0 itself supports 1.25+.
- **Helm 4.** The repository pins the version in `make/tool-versions.mk`.
- **cert-manager.** Required with default values; install it separately.
  NKTP does not bundle or install it. It issues certificates for internal
  Collector mTLS, the Operator's admission webhook, and
  Collector-to-Target-Allocator mTLS. Running without it requires replacing all
  three integrations. See [More information](#more-information) for the
  documentation covering installation and configuration.
- **Image registry access** to `docker.io`, `ghcr.io`, `quay.io` and
  `registry.k8s.io`, or mirrors of them. The uninstall hook also pulls
  `rancher/kubectl` from `docker.io`. See [Image registry](#image-registry).
- **Network access** between components. The Kubernetes API server must reach the
  Operator's admission webhook on TCP 9443 on the nodes; managed clusters can
  block this with firewall rules or security groups. Node collectors scrape their
  node's kubelet on TCP 10250 and node-exporter on TCP 9100. Collectors send OTLP
  to the gateways and watchdog across nodes. Control plane component metrics
  depend on the distribution, which determines whether those components expose
  reachable metrics endpoints.
- **Kubernetes resource metrics API** (`metrics.k8s.io`). Required for gateway,
  large-target and watchdog autoscaling. NKTP does not install metrics-server by
  default; set `metrics-server.enabled: true` if nothing else in the cluster
  provides the API.
- **Privileged Pod Security level for the release namespace.** The node collector
  mounts host paths for pod logs, journald and `/proc`. node-exporter uses the
  host network and PID namespaces, host paths, and host port 9100. The
  `baseline` and `restricted` Pod Security levels reject these Pods.
- **Capacity.** The defaults are sized for large clusters: about 10 CPU and
  26 GiB of memory in requests for the cluster-wide components, plus about
  1.1 CPU and 2.2 GiB on every node for the per-node DaemonSets.

## Install

```bash
kubectl create namespace nktp
kubectl label namespace nktp pod-security.kubernetes.io/enforce=privileged
helm install nktp oci://ghcr.io/dsx-ai-factory/nvke/charts/nktp \
  --version <version> --namespace nktp --values my-values.yaml --wait
```

Released versions are listed on the repository's GitHub Releases, as
`nktp/vX.Y.Z` tags. `--create-namespace` would create the namespace without the
Pod Security label. `my-values.yaml` holds your overrides; leave it out to
install with the defaults. The Operator creates the Collectors after `helm install --wait`
returns, so check their Pods as well.

### Upgrading

Helm never upgrades CRDs it installed from a chart's `crds/` directory. The
OpenTelemetry and Prometheus CRDs come from there, so a chart version that bumps
the Operator leaves the old CRDs in place, and fields only the new version knows
are silently dropped. Apply the new CRDs before `helm upgrade`.

## Components

| Component | Values key | Runs as | Purpose |
| --- | --- | --- | --- |
| Node collector | `daemonset` | DaemonSet | Receives OTLP from workloads on its node, and collects pod logs, the journal, host and audit logs, kubelet and cAdvisor metrics, and scrape targets on its node. |
| Logs, metrics, and traces gateways | `gateway_logs`, `gateway_metrics`, `gateway_traces` | One StatefulSet each | Export each signal to the backend. |
| Large-target metrics pool | `large_target_metrics` | StatefulSet | Scrapes monitors labeled `nktp.nvidia.io/large-target: "true"`, including the Kubernetes control plane. |
| Cluster event reader | `cluster_events` | StatefulSet | Reads Kubernetes Events, with one elected replica active at a time. |
| Watchdog | `watchdog` | StatefulSet | Receives the pipeline's own telemetry and exports it to the backend. |
| kube-state-metrics | `kube-state-metrics` | DaemonSet and Deployments | Exposes Kubernetes object state as metrics. |
| OpenTelemetry Operator and CRDs | `opentelemetry-kube-stack` | Deployment | Creates the Collectors and their Target Allocators. |
| node-exporter | `node-exporter` | DaemonSet | Exposes host metrics from every node. |
| metrics-server | `metrics-server` | Deployment, off by default | Serves the resource metrics API that the autoscalers read. |
| LGTM | `otel-lgtm` | Deployment | Development backend with Grafana, Prometheus, Loki and Tempo. Evaluation only. |

The node collectors, the large-target pool, and the event reader send each
signal to its gateway over OTLP/gRPC with mutual TLS, and the gateways export to
the backend.
Each Collector Pod runs a sidecar Collector that buffers the Collector's own
telemetry and samples its traces. All sidecars except watchdog's send to watchdog
over OTLP/gRPC with mTLS on port 4317. The DaemonSet also sends collected Operator
and Target Allocator logs directly to that endpoint. Target Allocators push their
own metrics to watchdog over plaintext OTLP/gRPC on port 4320. Watchdog exports
received telemetry to the backend without passing through the gateways; its own
sidecar exports watchdog's telemetry to the backend.

Gateways and watchdog rotate connections about every five minutes so clients
can reach newly added replicas while limiting connection churn. Configure the
interval under each receiver's `otelcol.config`.

## Sending telemetry to NKTP

- **OTLP:** send to the node collector on the sender's node, at
  `nktp-daemonset-collector.<namespace>:4317` over gRPC or
  `http://nktp-daemonset-collector.<namespace>:4318` over HTTP. If no local
  Collector is ready, traffic fails rather than reaching another node.
- **Prometheus metrics:** create a `ServiceMonitor` or `PodMonitor`. It can live
  in any namespace and needs no NKTP-specific label. If a NetworkPolicy guards
  the metrics port, admit the release namespace. If a monitor references a
  Secret or CA ConfigMap, list its namespace in
  `daemonset.kubernetes.targetAllocator.prometheusCR.secretNamespaces` so the
  node Target Allocator can read it.
- **Large scrape responses:** label the monitor
  `nktp.nvidia.io/large-target: "true"` to scrape it from the large-target pool,
  which reserves more memory per scrape, instead of the node collector.
- **Logs:** Pod stdout and stderr are collected from every node without
  configuration.

## Configuring

Each Collector component has the same values structure:

| Key | Contents |
| --- | --- |
| `<component>.otelcol.config` | The complete, native Collector configuration. |
| `<component>.otelcol.image` | An image override for this component only. |
| `<component>.kubernetes` | `OpenTelemetryCollector` settings such as replicas, resources, env, volumes, autoscaler, and Target Allocator. |

The chart renders each Collector configuration exactly as written. No other value
changes it, so to change a pipeline, change its configuration.

Helm replaces lists in your values file as a whole. When overriding a list
such as `kubernetes.env`, `kubernetes.volumes`, or a receiver's
`scrape_configs`, copy the default list from `values.yaml` and change your copy.
Keep `GOMEMLIMIT` and `GOGC` in every replacement `env` list. The cluster event
reader also needs `POD_NAMESPACE`.

The watchdog, node collector, and large-target pool require their Target
Allocators. Setting `targetAllocator.enabled: false` for any of them fails
rendering.

### Telemetry redaction

Enabled in the default Collector configurations in `values.yaml`.

- Masks recognized credentials in log bodies, including nested bodies, and in log attributes, metric data point attributes, span attributes and events, and resource/scope attributes. Uses `****`, including for quoted JSON and Basic/Bearer text. Preserves keys and counters such as `token_count`. No hashing.
- Runs last in the default source and gateway pipelines. Customize rules under `<workload>.otelcol.config.processors`, or disable redaction by removing its references from the relevant pipeline processor lists. Custom pipelines must include redaction explicitly.
- Basic matching does not cover OTLP `Metric.metadata`, exemplar `filtered_attributes`, span names, span link attributes, recursive key matching inside attribute maps and arrays, unknown credential names, or complex and encoded secrets. Attribute maps and arrays can still match value patterns through their text representation.
- Watchdog redacts other collectors' diagnostic exports (`service.telemetry`) because internal logs can include samples of rejected data. Sending these exports directly to a backend bypasses redaction.
- Watchdog's sidecar redacts watchdog's own logs, metrics, and traces, plus its own logs and metrics. Configure it under `watchdog.sidecar.otelcol.config`. Watchdog keeps `basic` tracing and the sidecar's own tracing stays disabled.
- Redaction changes exported copies. Original log files and container stdout, including `kubectl logs`, can still contain secrets.

Following OWASP logging guidance, exclude sensitive data from logs where possible.
Omit fields at the source when their names or presence are sensitive.

### Image registry

Set `global.imageRegistry` to override the image registry of all the components,
including the Collectors (`otelcol_image`) and the Target Allocators
(`targetallocator_image`). Caveat: `metrics-server` dependency, the nested OpenTelemetry Operator, and the
uninstall hook's kubectl image (`opentelemetry-kube-stack.cleanupJob.image`) do **not**
read this setting. Configure their image repositories explicitly when required.

### Cluster name

Set `global.clusterName` to add `k8s.cluster.name` to telemetry as it leaves
NKTP for the backend. Telemetry that already has `k8s.cluster.name` keeps it.
When `global.clusterName` is empty, the value is `unknown`.

## Before production

- **Disable the bundled LGTM backend.** It keeps data in memory, grants
  anonymous admin access to Grafana, and does not scale. Set
  `otel-lgtm.enabled: false`, then set your backend's endpoint in the
  `otlp` exporter of each gateway, the watchdog, and the watchdog's sidecar.
  Use `otlp` for gRPC or `otlp_http` for HTTP, matching your backend's supported
  ingestion protocol. When changing protocol, update each affected pipeline's
  `exporters` list too.
- **Secure backend export.** The default LGTM backend uses plaintext OTLP for
  local development. Enable TLS before sending gateway traffic
  to a production backend, and mount client certificates into the gateways if
  the backend requires mTLS.
- **Keep credentials in Secrets.** Add the Secret to the Collector's
  `kubernetes.env` and reference it from the exporter with `${env:NAME}`, for
  example in `headers`.
- **Provide the resource metrics API** so the gateways, large-target pool, and
  watchdog can autoscale.
- **Review resource requests and limits** under `<component>.kubernetes.resources`
  and `<component>.kubernetes.targetAllocator.resources`. Autoscaling targets
  are relative to requests.

## Bundled dependencies

| Chart | Version | Installs | Enabled by default | Values key |
| --- | --- | --- | --- | --- |
| opentelemetry-kube-stack | 0.22.0 | OpenTelemetry Operator chart 0.123.0, Operator 0.159.0, and CRDs | Yes | `opentelemetry-kube-stack` |
| prometheus-node-exporter | 4.56.1 | node-exporter | Yes | `node-exporter` |
| metrics-server | 3.13.1 | metrics-server 0.8.1 | No | `metrics-server` |

The Collector, Target Allocator, kube-state-metrics, and LGTM images are set in
`values.yaml` under `otelcol_image`, `targetallocator_image`,
`kube-state-metrics.image`, and `otel-lgtm.image`.

## Developing the chart

| Path | Contents |
| --- | --- |
| `values.yaml` | Defaults, including every Collector configuration |
| `templates/` | One template per component, plus RBAC and certificates |
| `dashboards/` | Grafana dashboards provisioned into LGTM, and examples |

Run the chart checks from the repository root:

```bash
make nktp-chart-deps   # download the dependencies into charts/
make nktp-chart-check  # lint, render and validate the chart; the merge gate runs this
```

In a checkout, `nktp/AGENTS.md` covers the chart's constraints, including
version bumps and dependency pins, and `nktp/hack/e2e-validate.sh` installs the
chart on a Kind cluster and asserts that every signal arrives. When a change
alters a component's behavior or values, update its page under
`docs/nktp/components/` in the same pull request.

This README is packaged into the chart, where links to other files do not
resolve. Keep it self-contained: it must not link to other documentation,
except the link below.

## More information

The [NKTP documentation](https://github.com/dsx-ai-factory/nvke/tree/main/docs/nktp)
covers each component, configuration guides, operations, and design documents.
