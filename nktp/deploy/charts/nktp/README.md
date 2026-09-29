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
to the watchdog, and so does each Target Allocator its metrics, over OTLP. The
node Collectors send the Target Allocators' and the Operator's container logs
there too. The watchdog sends its own telemetry to the backend.

| Values key | Role |
|---|---|
| `watchdog` | Receives the other Collectors' own telemetry over OTLP at `nktp-watchdog-collector:4318` and forwards it to the backend. Scrapes the Operator. |
| `gateway_logs`, `gateway_metrics`, `gateway_traces` | The OTLP entry points, one per signal, at `nktp-gateway-<signal>-collector:4317` (gRPC) and `:4318` (HTTP). Forward to the backend. Two replicas each by default, autoscaling up to 64. |
| `daemonset` | One Collector per node. Reads pod logs under `/var/log/pods`, the node's systemd journal and its authentication and audit logs (see [Node logs](#node-logs)), scrapes the local kubelet and its cAdvisor endpoint, collects per-process metrics from `/proc`, scrapes the targets a node-local Target Allocator assigns from every monitor in the cluster, and forwards each signal to its gateway. Runs as root because of the host log mount. |
| `large_target_metrics` | A pool with its own Target Allocator for monitors labelled `nktp.nvidia.io/large-target: "true"`, which is how kube-state-metrics is scraped. Two replicas by default, autoscaling up to 16. |
| `cluster_events` | Turns Kubernetes Events into logs. Two replicas by default: a Lease the chart creates in the release namespace elects the reader, and the other stands by. |
| `kube-state-metrics` | A DaemonSet for node-local Pod metrics, one Deployment for Pods awaiting a node, and five Deployments for the other object types, grouped as workloads, networking, storage, configuration and cluster-scoped objects. RBAC is derived from the resource types. Each group can be sharded for larger clusters. |
| `node-exporter` | The upstream DaemonSet on host network port 9100, with a ServiceMonitor the node Target Allocator picks up. The only source of node-level CPU, memory, filesystem and network metrics; the node Collector's host metrics receiver reports processes, not the node. |
| `metrics-server` | The resource metrics API the autoscalers read. Off by default, because most clusters already have a provider; see [Bundled components](#bundled-components). |
| `otel-lgtm` | Grafana, Loki, Tempo and Prometheus in one container, with the NKTP dashboards in [dashboards/default](dashboards/README.md). On by default for evaluation; see [The backend](#the-backend). |

Footprint with default values on three nodes, before the Operator and the
development backend. Requests are per workload; a Collector's are set under
`<workload>.kubernetes.resources` and its Target Allocator's under
`<workload>.kubernetes.targetAllocator.resources`.

| Workload | Pods | CPU request | Memory request |
|---|---|---|---|
| Node Collector | 3 | 300m | 768Mi |
| Watchdog | 2 | 200m | 512Mi |
| Gateways | 6 | 1200m | 3Gi |
| Large-target pool | 2 | 1000m | 16Gi |
| Cluster event reader | 2 | 400m | 1Gi |
| Target Allocators | 3 | 750m | 4Gi |
| node-exporter | 3 | none set | none set |
| kube-state-metrics | 9 | 900m | 1.1Gi |

## Prerequisites

- Helm 4. The repository pins the version in `make/tool-versions.mk`.
- Network access to the chart repositories named in [Chart.yaml](Chart.yaml)
  when building dependencies.
- A cluster that accepts the `opentelemetry.io/v1beta1` API. The chart installs
  the OpenTelemetry and Prometheus Operator CRDs by default.
- cert-manager. It issues the Operator's webhook certificate and the mTLS
  certificates between each Collector and its Target Allocator; see
  [Certificates](#certificates) to run without it.
- A release namespace whose Pod Security admission level is `privileged`. The
  node Collector runs as root with `SYS_CHROOT`, and mounts `/var/log/pods`,
  `/proc`, `/var/log`, the node's `journalctl` with its libraries and
  `/etc/machine-id` from the host, which `baseline` rejects.
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

### Node logs

The node Collector reads the systemd journal at `info` priority and above by
running the node's own `/usr/bin/journalctl`, which is why it has `SYS_CHROOT`
and read-only mounts of the node's libraries. It expects the journal in
`/var/log/journal` and reads `/etc/machine-id`. Collection starts with new
entries; journal cursors are not kept across Collector restarts.

For a different host layout, set `root_path`, `journalctl_path` and `directory`
under `daemonset.otelcol.config.receivers.journald`, and change
`daemonset.kubernetes.volumes` and `volumeMounts` to match. Both are lists that
replace the chart's whole list, so keep its other entries. A node that keeps
its journal in memory needs a `/run/log/journal` host volume mounted at
`/hostfs/run/log/journal` and the receiver's `directory` set to
`/run/log/journal`, as [nktp/hack/e2e-values.yaml](../../../hack/e2e-values.yaml)
does for Kind.

### Log classification

Logs from some sources carry a `nvidia.telemetry.class` resource attribute, so
the backend can route them without parsing their content. Every record from a
classified source gets the same class.

| Source | Class | `service.name` | Recognized by |
| --- | --- | --- | --- |
| sudo and SSH journal entries | `system.auth` | `journald` | Journal identifier `sudo` or `sshd` |
| `/var/log/auth.log`, `/var/log/secure` | `system.auth` | `auth` | Every record in the file |
| `/var/log/audit/audit.log` | `system.audit` | `auditd` | Every record in the file |
| Kubernetes control-plane Pods | `k8s.control_plane` | not set | `kube-apiserver`, `kube-controller-manager`, `kube-scheduler` and `etcd` containers in `kube-system` Pods with matching name prefixes |
| `/var/log/kubernetes/audit/audit.log` | `k8s.api.audit` | `kube-apiserver` | Every record in the file |

Other logs carry no class. The API server writes an audit log only where audit
logging is on, and only at the path its `audit-log-path` sets; point
`daemonset.otelcol.config.receivers.filelog/kube-api-audit.include` at that
path when it differs.

### Kubernetes component metrics

The chart turns on opentelemetry-kube-stack's monitors for the API server, the
controller manager, the scheduler, etcd, CoreDNS and kube-proxy. They carry the
large-target label, so the large-target pool scrapes them, and its Collectors'
ServiceAccount may read the `/metrics` non-resource URL. The monitors reference
the Collector's ServiceAccount token and CA files, so that pool's Target
Allocator sets `denyFSAccessThroughSMs: false`; with `true` it drops those
endpoints, as
[opentelemetry-helm-charts#2408](https://github.com/open-telemetry/opentelemetry-helm-charts/issues/2408)
describes.

A monitor does not guarantee a reachable target. kubeadm binds the controller
manager and the scheduler to `127.0.0.1` by default, some clusters do the same
for etcd's metrics, and a cluster whose CNI replaces kube-proxy runs none.
Turn off the monitors a cluster cannot serve:

```yaml
opentelemetry-kube-stack:
  kubeScheduler:
    enabled: false
  kubeControllerManager:
    enabled: false
  kubeProxy:
    enabled: false
```

### Certificates

cert-manager issues and renews two sets of certificates by default: the
Operator's admission webhook certificate, and a CA with server and client
certificates for each Collector and its Target Allocator, so the Collectors
fetch their scrape assignments over mTLS. The Operator provisions the second
set; see the Operator's
[Target Allocator documentation](https://github.com/open-telemetry/opentelemetry-operator/blob/main/docs/target-allocator/README.md#with-cert-manager-default).

A cluster without cert-manager turns both off. Helm then generates a
self-signed webhook certificate, reuses it on upgrade, and never renews it; it
expires after 365 days. Each allocator's `mtls` block can instead reference
certificates that already exist in the release namespace, with
`useCertManager: false` and a `tls` block, as the Operator's
[user-provided certificate documentation](https://github.com/open-telemetry/opentelemetry-operator/blob/main/docs/target-allocator/README.md#without-cert-manager-user-provided-certificates)
describes.

```yaml
opentelemetry-kube-stack:
  opentelemetry-operator:
    admissionWebhooks:
      certManager:
        enabled: false
      autoGenerateCert:
        recreate: false
watchdog:
  kubernetes:
    targetAllocator:
      mtls:
        enabled: false
daemonset:
  kubernetes:
    targetAllocator:
      mtls:
        enabled: false
large_target_metrics:
  kubernetes:
    targetAllocator:
      mtls:
        enabled: false
```

### Monitor credentials

With mTLS between them, the node Target Allocator can hand its Collectors the
Secret-backed credentials a monitor references, such as a bearer token or a
client certificate. It reads them only in the namespaces listed in
`daemonset.kubernetes.targetAllocator.prometheusCR.secretNamespaces`:

```yaml
daemonset:
  kubernetes:
    targetAllocator:
      prometheusCR:
        secretNamespaces: [metrics-monitors]
```

For each listed namespace the chart creates a Role granting `get`, `list` and
`watch` on Secrets and `get` on ConfigMaps, bound to the allocator's
ServiceAccount. The grant covers every Secret in the namespace, so list only
namespaces whose monitor authors are trusted. The namespaces must already
exist. The list is empty by default, which creates nothing;
`daemonset.rbac.create: false` leaves the grants to you.

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
the bundled Collector image exports OTLP and nothing else. It also has to
accept delta metrics: the Target Allocators export some counters as delta even
though the chart asks for cumulative temporality, which the bundled Prometheus
converts with `--enable-feature=otlp-deltatocumulative`. A values file for a
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
  Kubernetes labels are not exposed as series labels by default;
  `kube-state-metrics.metricLabelsAllowlist` selects some, with entries such
  as `pods=[app.kubernetes.io/name]`. `kube-state-metrics.enabled: false`
  removes all of it.
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

The Operator's webhook certificate comes from cert-manager; see
[Certificates](#certificates).
