# Validating the NKTP chart on Kind

> **Note:** This is ad hoc, hands-on validation of the chart's default values on a
> cluster environment simulating an NKE site-controller. It will be replaced with more
> formal directives as the project grows and formal testing is established.

## What it validates

Installed with default values, the chart brings up the OpenTelemetry Operator,
seven Collectors, kube-state-metrics, node-exporter, metrics-server and the
bundled development backend. The validation confirms that:

- every Collector workload the Operator builds becomes Ready, with no restarts;
- the node Collector scrapes the kubelet and cAdvisor, and its Target Allocator
  discovers and scrapes the ServiceMonitors and PodMonitors in the cluster;
- kube-state-metrics object state reaches the backend through the large-target pool;
- pod logs and Kubernetes events reach Loki;
- the Collectors' own traces reach Tempo;
- every Collector exports with zero send failures;
- metrics-server serves `metrics.k8s.io` and the autoscalers report utilization.

## Environment

A single-node Kind cluster built from the same configuration and node image the
controller's end-to-end lane uses, `controller/hack/cwe-capi-kind.yaml`. Docker
Desktop or Colima, `kind`, `kubectl` and `helm` are all that is needed. The
cluster gets its own kubeconfig so nothing else on the machine is touched.

```console
kind create cluster --name nktp-validation \
  --config controller/hack/cwe-capi-kind.yaml \
  --kubeconfig ./nktp-validation.kubeconfig
export KUBECONFIG=$PWD/nktp-validation.kubeconfig
```

## Installing the chart

Build the chart's dependencies, create the release namespace with the Pod
Security level the node Collector needs, and install with default values.

```console
helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/
helm dependency build nktp/deploy/charts/nktp

kubectl create namespace nktp
kubectl label namespace nktp pod-security.kubernetes.io/enforce=privileged

helm install nktp nktp/deploy/charts/nktp --namespace nktp --wait \
  --set-json 'metrics-server.args=["--kubelet-insecure-tls"]'
kubectl -n nktp wait --for=condition=Ready pod --all --timeout=5m
```

The `metrics-server.args` override is for Kind only. Kind's kubelet serving
certificates are self-signed, so metrics-server cannot verify them and never
becomes Ready without it. A site's kubelet certificates are signed by the cluster
CA through the controller's CSR approval and need no such flag; do not carry it
into site values.

`helm install --wait` returns when the Helm-managed workloads are ready. The
Collectors are built by the Operator after that, hence the second wait.

## Checking the signals

The bundled backend exposes Prometheus, Grafana, Loki and Tempo from one pod.
Forward all four:

```console
kubectl -n nktp port-forward deploy/lgtm 3000:3000 9090:9090 3100:3100 3200:3200
```

| Validating | Command |
|---|---|
| Prometheus scrape targets | Prometheus: `min by (job) (up{job=~"kubelet\|kubelet-cadvisor\|nktp-operator\|node-exporter\|.*-targetallocator\|nktp/nktp-ksm-.*"})` |
| Export failures | Prometheus: `sum by (service_name) ({__name__=~"otelcol_exporter_send_failed.*"}) > 0` |
| Object-state metrics | Prometheus: `count(kube_pod_info) > 0 and count(kube_deployment_status_replicas_available) > 0` |
| Pod logs | Loki: `sum by (k8s_namespace_name) (count_over_time({k8s_namespace_name=~"kube-system\|nktp"}[15m]))` |
| Kubernetes events | Loki: `sum(count_over_time({service_name="unknown_service"} \| k8s_event_reason=~".+" [15m]))` |
| Traces | Tempo: `{ resource.service.name =~ "nktp-.*" }` |
| Autoscaler metrics | `kubectl -n nktp get hpa -o json \| jq -e '[.items[] \| select((.status.currentMetrics // []) \| length == 0)] \| length == 0'` |
| Collector errors | `kubectl -n nktp logs -l app.kubernetes.io/component=opentelemetry-collector --since=10m \| grep -c $'\terror\t'` |

The scrape-target query returns one row per job, each at 1. The export-failure
query returns nothing when healthy. The object-state, log, event and trace queries
return a result when the signal is arriving. The autoscaler command exits 0 when
every HPA has current metrics. The Collector error count should be 0 once start-up
is over.

Grafana on port 3000 has the chart's dashboards, one per Collector, with anonymous
admin access.

Job labels follow the Prometheus Operator conventions: a ServiceMonitor's targets
carry the Service name, a PodMonitor's targets carry `<namespace>/<monitor>`.

## Validating next to the controller

The same cluster can host the controller through the local Tilt loop described in
[../development.md](../development.md), which is how the integration boundary was
checked once by hand. Two things matter for that run. NKTP has to be installed
first, so the monitoring CRDs exist when the controller chart renders its
ServiceMonitor. And the controller chart's local-loop values turn its
ServiceMonitor and metrics NetworkPolicy off, so the controller release needs
these settings on top:

```yaml
serviceMonitor:
  enabled: true
metrics:
  networkPolicy:
    enabled: true
    additionalNamespaces: [nktp]
tracing:
  enabled: true
  endpoint: nktp-gateway-traces-collector.nktp.svc.cluster.local:4317
  insecure: true
```

With those in place, `up{job="cloud-workflow-engine"}` reaches 1 in Prometheus,
`{k8s_namespace_name="cloud-workflow-engine"}` returns lines in Loki once the
controller logs something, and `cloud-workflow-engine` appears among Tempo's
service names after any API request.

## Tearing down

```console
kind delete cluster --name nktp-validation --kubeconfig ./nktp-validation.kubeconfig
rm ./nktp-validation.kubeconfig
```
