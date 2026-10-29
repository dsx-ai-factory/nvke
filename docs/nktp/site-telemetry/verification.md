# Management cluster telemetry: verification

A Collector can be Ready and still export nothing, so this checklist checks
the signals themselves, not just the Pods. Run it after the management plane is
installed through the `deploy/nico` deploy path, or after any change to its NVKE telemetry pipeline (NKTP)
values. It assumes the development backend, `lgtm`. Against your own backend,
run the same queries there.

On an existing site, the NVKE controller runs under its pre-rename name, either
`nke-controller` or, on an older site, `cloud-workflow-engine`. Replace
`nvke-controller` with whichever name that site has throughout this page.

All commands run against the management cluster:

```bash
export KUBECONFIG=<site-kubeconfig>
```

## 1. The pipeline is up

```bash
kubectl -n nktp get pods                          # every Pod Running, no restarts climbing
kubectl -n nktp get opentelemetrycollectors       # seven, each READY 1/1
kubectl -n nktp get certificates                  # 21, all Ready
kubectl -n nktp get configmap nktp-cluster -o jsonpath='{.data.name}'; echo   # <site-name>
```

The 21 certificates are the Operator's webhook certificate, a CA, server and
client certificate for each of the three Target Allocators, and the eleven
internal TLS certificates that the chart's `certManager.internalTLS` default
adds: one CA, four server and six client certificates.
[Configure cert-manager](../guides/configure-cert-manager.md) lists them.

**The Operator:** it must be running with no restarts, and its finalizer
grant must be owned by Helm:

```bash
kubectl -n nktp get pods -l app.kubernetes.io/name=opentelemetry-operator
kubectl -n nktp get role,rolebinding nktp-operator-deployment-finalizers \
  -o custom-columns=KIND:.kind,MANAGED:.metadata.labels.app\\.kubernetes\\.io/managed-by
kubectl -n nktp logs deploy/nktp-operator | grep -ciE 'forbidden|blockOwnerDeletion'   # 0
```

**The monitors:**

```bash
kubectl get servicemonitors,podmonitors -A
```

| Namespace | Monitors |
|---|---|
| `nktp` | `nktp-apiserver`, `nktp-kube-etcd`, `nktp-coredns`, `nktp-node-exporter`, and PodMonitors `nktp-ksm-objects`, `nktp-ksm-pods`, `nktp-ksm-unscheduled-pods` |
| `temporal` | `temporal-frontend`, `temporal-history`, `temporal-matching`, `temporal-worker` |
| `spire-server` | PodMonitor `spire-server` |
| `spire-system` | PodMonitor `spire-agent` |
| `nvke-controller` | `nvke-controller-metrics` |

**The resource metrics API,** which the Collector autoscalers read:

```bash
kubectl get apiservice v1beta1.metrics.k8s.io   # Available True
kubectl top nodes
kubectl -n nktp get hpa                         # every HPA shows current CPU and memory
```

## 2. The signals arrive

Port-forward the backend, bound to localhost, and define two shell helpers
the rest of this page uses:

- **`promql '<query>'`** runs a PromQL query against the backend's Prometheus.
- **`logql '<query>'`** runs a LogQL query against its Loki.

Each prints one line per result: the series' identifying label (its `job`,
`service_name` or `k8s_cluster_name`), then its value. Run them in the same
shell as the port-forward.

```bash
kubectl -n nktp port-forward deploy/lgtm 19090:9090 13100:3100 13200:3200 --address 127.0.0.1 &
promql() { curl -fsSG http://127.0.0.1:19090/api/v1/query --data-urlencode "query=$1" | jq -r '.data.result[] | [(.metric.job // .metric.service_name // .metric.k8s_cluster_name // "value"), .value[1]] | @tsv'; }
logql() { curl -fsSG http://127.0.0.1:13100/loki/api/v1/query --data-urlencode "query=$1" | jq -r '.data.result[] | [(.metric.service_name // .metric.k8s_cluster_name // "value"), .value[1]] | @tsv'; }
```

Scrapes run every 30 seconds and exports are batched, so give a fresh install
a few minutes before these queries.

### Metrics

```bash
promql 'count by (job) (up == 0)'          # empty: no target is down
promql 'count by (job) (up == 1)'
```

| Job | What it is |
|---|---|
| `apiserver`, `kube-etcd`, `coredns` | Control-plane monitors |
| `kubelet`, `kubelet-cadvisor` | The node's kubelet, scraped by the node Collector |
| `node-exporter` | Node CPU, memory, filesystem and network |
| `nktp/nktp-ksm-objects`, `nktp/nktp-ksm-pods`, `nktp/nktp-ksm-unscheduled-pods` | Object state from kube-state-metrics |
| `nktp-operator` | The OpenTelemetry Operator, scraped by the watchdog |
| `nvke-controller` | The controller |
| `temporal-frontend-headless`, `temporal-history-headless`, `temporal-matching-headless`, `temporal-worker-headless` | Temporal's server roles |
| `spire-server/spire-server`, `spire-system/spire-agent` | SPIRE |

A PodMonitor's job is `<namespace>/<monitor>`; a ServiceMonitor's is the
Service's name. `otelcol-contrib` also appears: it is the development
backend's own Collector.

```bash
promql 'count by (k8s_cluster_name) (up)'                           # <site-name> on nearly every series
promql 'count(node_cpu_seconds_total)'                              # > 0
promql 'count(kube_pod_info)'                                       # > 0
promql 'sum by (service_name) ({__name__=~"otelcol_exporter_send_failed_.*", service_name=~"nktp-.*"})'   # empty
```

A handful of series without `k8s_cluster_name` is expected. The development
backend scrapes itself without going through NKTP.

### Logs

```bash
logql 'sum by (k8s_namespace_name) (count_over_time({k8s_namespace_name=~".+"}[15m]))'   # every namespace with Pods
logql 'sum by (service_name) (count_over_time({service_name=~"journald|auth|kube-apiserver"}[15m]))'
logql 'sum(count_over_time({service_name=~".+"} | k8s_event_reason=~".+" [15m]))'  # Kubernetes Events
logql 'sum by (k8s_cluster_name) (count_over_time({k8s_cluster_name=~".+"}[15m]))'       # <site-name>
```

- **`journald`** is the node's systemd journal.
- **`auth`** is `/var/log/auth.log` or `/var/log/secure`.
- **`kube-apiserver`** is the API server's audit log.

The log classes can be checked the same way, through the
`nvidia_telemetry_class` label: `system.auth`, `system.audit`,
`k8s.control_plane` and `k8s.api.audit`.
[Log classification](../components/node-collector.md#log-classification)
lists what each covers. `/var/log/audit/audit.log` exists only where auditd
runs, so `system.audit` may be empty.

### Traces

The controller sends a span per API request:

```bash
for i in 1 2 3; do curl -fsS -o /dev/null http://<site-ip>:30080/api/version; done; sleep 10
curl -fsSG http://127.0.0.1:13200/api/search --data-urlencode 'q={ resource.service.name = "nvke-controller" }' | jq '.traces | length'   # > 0
curl -fsS http://127.0.0.1:13200/api/search/tag/service.name/values | jq -r '.tagValues[]'   # includes nvke-controller and the nktp-* Collectors
```

## 3. The rest of the site is unaffected

NKTP's install touches other components' configuration, so check they still
work:

```bash
curl -fsS http://<site-ip>:30080/readyz; echo                                        # database and temporal ok
curl -sk -o /dev/null -w '%{http_code}\n' https://<site-ip>:30443/.well-known/openid-configuration   # 200
nvkectl get providers                                                                  # CONFIGURED
```

- **The first:** the controller's metrics NetworkPolicy must not have cut off
  its API.
- **The second:** SPIRE's OIDC discovery must still answer at the site IP.
- **The third:** CAPNICo is still configured.

## 4. A repeat install changes nothing

The deploy path is meant to be re-run. Record Pod start times, re-run it with
the same inputs, and compare:

```bash
kubectl get pods -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,START:.status.startTime > before.txt
# re-run the deploy path
kubectl get pods -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,START:.status.startTime > after.txt
diff before.txt after.txt   # no NKTP, Temporal or SPIRE Pod restarted
```

Helm revisions go up on every run; that is expected.
