# Managed dependencies

NKTP installs these upstream charts as Helm dependencies. Their versions are
pinned in `nktp/deploy/charts/nktp/Chart.yaml` and `nktp/deploy/charts/nktp/Chart.lock`. Settings for each pass
through under its values key using the upstream chart's own values.

## OpenTelemetry Operator and CRDs

NKTP depends on the published [opentelemetry-kube-stack 0.22.0](https://github.com/open-telemetry/opentelemetry-helm-charts/tree/opentelemetry-kube-stack-0.22.0/charts/opentelemetry-kube-stack)
chart. It supplies OpenTelemetry Operator chart 0.123.0, operator image 0.159.0,
and native CRDs. NKTP disables its default Collector and general Collector RBAC.
Settings pass through under `opentelemetry-kube-stack` using upstream values.

Helm installs missing OpenTelemetry CRDs (OpenTelemetryCollector, Instrumentation,
TargetAllocator, OpAMPBridge) and Prometheus CRDs (ServiceMonitor, PodMonitor, Probe,
ScrapeConfig) before the chart's resources.

The kube-stack chart also supplies the Kubernetes component ServiceMonitors that
the large-target pool scrapes. See
[Kubernetes component metrics](large-target-metrics.md#kubernetes-component-metrics).

The minimum supported API version for `OpenTelemetryCollector` resources is `opentelemetry.io/v1beta1`.

Use these independent settings when another mechanism manages the Operator or CRDs.

```yaml
opentelemetry-kube-stack:
  opentelemetry-operator:
    enabled: false
  crds:
    installOtel: false
    installPrometheus: false
```

Override the Operator image with
`opentelemetry-kube-stack.opentelemetry-operator.manager.image.tag`.

The Operator's admission webhook certificate comes from cert-manager by default.
See [Configure cert-manager](../guides/configure-cert-manager.md#admission-webhook) for the
alternative.

## Node exporter

NKTP installs [prometheus-node-exporter](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus-node-exporter) by default
to collect host-level metrics from every node. It runs as a DaemonSet with host networking and binds port 9100 on each node,
and ships a ServiceMonitor so the exporter is discoverable as a scrape target.

Disable it, for example when the cluster already runs a node exporter:

```yaml
node-exporter:
  enabled: false
```

## Metrics server

[metrics-server](https://github.com/kubernetes-sigs/metrics-server) serves the Kubernetes
resource metrics API (`metrics.k8s.io`), which `kubectl top` and Horizontal Pod Autoscalers
read. It is what drives horizontal autoscaling of the three signal gateways, the
large-target metrics pool, and the watchdog: every workload with an `autoscaler` block
scales on CPU and memory utilization, which the HPA controller can only read from this API.

NKTP does **not** install it by default, because many clusters already get it from a
managed control plane or a cluster add-on, and a second copy conflicts over the
cluster-singleton `v1beta1.metrics.k8s.io` APIService. With no provider at all, those HPAs
cannot compute a recommendation and stop scaling, holding each workload at whatever
replica count it currently has. Enable it when nothing else supplies the API:

```yaml
metrics-server:
  enabled: true
```

The bundled chart is pinned to metrics-server chart 3.13.1, which installs metrics-server
0.8.1.

Settings pass through under `metrics-server` using the upstream chart's own values, none
of which NKTP overrides. See the [chart's values reference](https://github.com/kubernetes-sigs/metrics-server/blob/metrics-server-helm-chart-3.13.1/charts/metrics-server/README.md)
for what is configurable, including the TLS defaults for the APIService and for kubelet
certificate verification.
