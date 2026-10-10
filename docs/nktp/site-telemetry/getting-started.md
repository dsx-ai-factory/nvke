# Telemetry on a site controller: getting started

NKTP, the NVIDIA Kubernetes Telemetry Pipeline, collects a site controller's
metrics, logs and traces and exports them over OTLP. On a site controller it is
installed by the `deploy/nico` deploy path, the same way as the rest of the
management plane. There is nothing to install by hand.

This guide covers what NKTP gives a site controller, what the deploy path needs
from you, and how to take a first look. The other guides in this directory
cover [configuration](configuration.md), [verification](verification.md) and
[troubleshooting](troubleshooting.md).

## What you get

The `nktp` item in [`deploy/nico`](../../../deploy/nico/README.md)
installs the [NKTP chart](../../../nktp/deploy/charts/nktp/README.md) from the
same checkout, into namespace `nktp`:

- **The OpenTelemetry Operator** and its CRDs, plus the Prometheus monitoring
  CRDs (`ServiceMonitor`, `PodMonitor` and the rest).
- **Collectors**, each an `OpenTelemetryCollector` the Operator runs:
  - one on every node, for pod logs, the systemd journal, host auth and audit
    logs, the kubelet and cAdvisor, and every monitor scheduled to its node
  - a gateway per signal (metrics, logs, traces), the OTLP entry points that
    export to the backend
  - a large-target pool for kube-state-metrics and the control-plane monitors
  - a Kubernetes Event reader
  - a watchdog that receives every Collector's own telemetry
- **kube-state-metrics, node-exporter and metrics-server.** metrics-server is
  the site's only provider of `metrics.k8s.io`, which the Collector
  autoscalers read.
- **A development backend,** `lgtm`: Grafana, Prometheus, Loki and Tempo in one
  Pod, with the NKTP dashboards. It keeps data in memory and has no
  authentication, so it is for evaluation. See
  [Connecting a real backend](configuration.md#connecting-a-real-backend).

The deploy path also wires the rest of the management plane into it:

| Component | Signal | How |
|---|---|---|
| NKE controller | metrics, logs, traces | ServiceMonitor, pod logs, OTLP traces to the node Collector; its metrics NetworkPolicy admits `nktp` |
| Temporal | metrics | a ServiceMonitor per server role, from Temporal's own chart |
| SPIRE server and agent | metrics | PodMonitors, from SPIRE's own chart |
| API server, etcd, CoreDNS | metrics | the chart's control-plane monitors |
| API server | audit log | read from the node, classified `k8s.api.audit` |
| Every Pod | logs, state | pod logs from the node; state from kube-state-metrics |
| The node | metrics, journal | node-exporter, kubelet and cAdvisor, the systemd journal |

Everything exported carries `k8s.cluster.name` set to the site's name.

**Not scraped:**
- **cert-manager, NodeLocal DNSCache, OVN-Kubernetes and CAPNICo.** NKTP ships
  no monitors for other components: each service's owner ships its own. These
  four don't yet; see [monitoring your service](monitoring-a-service.md).
- **CAPI core and the kubeadm providers,** which serve metrics only to a
  caller with a bearer token.
- **The scheduler and controller manager,** which listen on `127.0.0.1` on
  NKE clusters.
- **Postgres,** which runs no metrics exporter.

The state of all their Pods still arrives through kube-state-metrics.

## Before you deploy

The deploy path installs NKTP with the rest of the management plane, so its
prerequisites are the path's own
([Before this runs](../../../deploy/nico/README.md#before-this-runs)),
plus:

- **cert-manager.** `clusterctl init` installs it. It issues the Operator's
  webhook certificate and the mTLS certificates between each Collector and
  its Target Allocator.
- **`ClusterName` in the BootstrapConfig** passed with `--config`. It becomes
  `k8s.cluster.name` on every signal. The install stops at the `nke-base-config`
  item if it is missing:
  ```json
  { "ClusterName": "<site-name>", "Nico": { "...": "..." } }
  ```
- **Egress from the node running the install:**
  - to `open-telemetry.github.io`, `prometheus-community.github.io` and
    `kubernetes-sigs.github.io`, because `deploy/nke-deploy.sh` builds the
    chart's dependencies from its `Chart.lock` before installing
  - to `docker.io`, `ghcr.io`, `quay.io` and `registry.k8s.io`, for the images
- **Room on the nodes.** The site values size NKTP for the smaller clusters a
  site controller runs on. Its Pods request about 1.6 CPU and 3.9Gi of memory
  in total, plus about 125m and 320Mi for each node beyond the first: the node
  Collector and kube-state-metrics' node Pod run on every node. The development
  backend sets no requests but used about 1.2Gi at idle.

Nothing else. The deploy path creates the `nktp` namespace with the
`privileged` Pod Security level the node Collector needs, and every setting a
site controller needs is in
[`nktp/values.yaml`](../../../deploy/nico/nktp/values.yaml).

## Install order

`nktp` comes straight after `capnico` and before everything that creates a
monitor:

```text
nke-base-config -> spire-tpm-pubhashes -> spire-server-nodeport
  -> spire-oidc-provider-nodeport -> capnico -> nktp
  -> postgresql -> spire-crds -> temporal -> spire -> controller
```

- **Why this position:** the chart installs the monitoring CRDs, which the
  `temporal`, `spire` and `controller` items need for their monitors.
- **A side effect:** installing it first means NKTP also records the rest of
  the management plane coming up.
- **The cost:** a failed `nktp` install stops the path there.

## First look

On the site controller, with its kubeconfig:

```bash
export KUBECONFIG=<site-kubeconfig>
kubectl -n nktp get pods                       # all Running
kubectl -n nktp get opentelemetrycollectors    # seven, each READY 1/1
kubectl -n nktp get certificates               # all Ready
```

To open Grafana, port-forward the backend. Bind it to localhost only: Grafana
gives anonymous visitors admin rights.

```bash
kubectl -n nktp port-forward deploy/lgtm 3000:3000 --address 127.0.0.1
```

If you are working through a jump host, forward that port over SSH rather than
binding it on the site network. The **NKTP Health** dashboard shows whether the
pipeline itself is healthy.

### NKE reference dashboards

NKE also ships [reference Grafana dashboards](../../../controller/deploy/dashboards/README.md)
as JSON files. These are starting points for investigating a site, not a
complete monitoring or alerting setup. Review their **Requirements and scope**
panels, check that your backend collects the required metrics, and adapt the
dashboards and alerts to your operating needs. An empty optional panel can mean
its metric is not scraped.

To use these dashboards, open Grafana **Dashboards → New → Import**, upload its
JSON file (or paste the JSON), and select a Prometheus datasource when prompted.
The files are not installed automatically with the site controller. You can
import them into the bundled `lgtm` Grafana while developing, but import them
into your [real telemetry backend](configuration.md#connecting-a-real-backend)
for site operations: `lgtm` keeps data in memory and has no authentication.
See the [dashboard README](../../../controller/deploy/dashboards/README.md)
for the full catalog and import details.

For the full checklist, see [verification](verification.md).

## Next steps

- [Configuration](configuration.md): what the site values change, connecting
  a real backend, and overriding values safely.
- [Monitoring your service](monitoring-a-service.md): how a service gets its
  own metrics scraped.
- [Verification](verification.md): checking every signal end to end.
- [Troubleshooting](troubleshooting.md): the failures seen on real site
  controllers, and their fixes.
- [Observability](../../operations/observability.md): the Temporal UI,
  controller logs and provider diagnostics, which NKTP complements rather than
  replaces.
