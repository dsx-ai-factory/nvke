# Telemetry on a site controller: configuration

The NKTP chart's defaults are for a multi-node cluster, and they know nothing
about NVKE. Everything a site controller needs differently is in
[`deploy/nico/nktp/values.yaml`](../../../deploy/nico/nktp/values.yaml),
which the `nktp` item applies over the chart. This page explains what it
changes and why, and how a site overrides it.

## What the site values change

| Setting | Why |
|---|---|
| **The Operator's finalizer grant** (`opentelemetry-kube-stack.opentelemetry-operator.extraObjects`) | A Role and RoleBinding granting the Operator `update` on its own Deployment's `deployments/finalizers`. Without it, the Operator crash-loops on NVKE clusters; see [troubleshooting](troubleshooting.md#the-operator-crash-loops-with-cannot-set-blockownerdeletion). |
| **`k8s.cluster.name`** on the watchdog, its sidecar and the three gateways | A `K8S_CLUSTER_NAME` environment variable read from ConfigMap `nktp-cluster`, which `nvke-base-config` renders from BootstrapConfig's `ClusterName`. The chart's `transform/cluster` processor reads it, and the chart sets it from `global.clusterName` only when the `env` list omits it. NVKE's kubeadm configuration names every cluster `kubernetes`, so the name has to come from outside the cluster. |
| **The API audit log path** (`filelog/kube-api-audit.include`) | NVKE's kubeadm configuration writes the API server's audit log to `/var/log/kubernetes_audit/audit.log`, not the chart's default path. |
| **Control-plane monitors** (`kubeScheduler`, `kubeControllerManager`, `kubeProxy` off) | NVKE binds the scheduler and controller manager to `127.0.0.1`, and OVN clusters run no kube-proxy. Left on, they would show as permanently down targets. The API server, etcd and CoreDNS monitors stay on and work. |
| **metrics-server on** | Nothing else serves `metrics.k8s.io` on a site controller, and the Collector autoscalers read it. |
| **Sizing** | One replica of everything, autoscalers starting at one, small requests and limits for every Collector and Target Allocator, and one kube-state-metrics Deployment for all object types instead of five. Sized for the smaller clusters a site controller runs on. |

## Connecting a real backend

By default every exporter points at the development backend, `lgtm`, in the
`nktp` namespace. For anything beyond evaluation, point them at your own
backend and turn `lgtm` off:

1. **Write a values file for the site.** The keys are listed in
   [Set a backend](../getting-started.md#set-a-backend):
   the `otlp` exporter of the watchdog, the watchdog's sidecar and each
   gateway, plus `otel-lgtm.enabled: false`.
2. **Pass it to the deploy path** with `--values-for nktp=<file>`. It is
   applied after the item's own values, so it wins.
3. **Keep credentials out of values.** Put them in a Secret in `nktp`,
   referenced from the workload's `kubernetes.env` and read through
   `${env:NAME}` in the exporter configuration.

The backend has to accept:

- **OTLP over gRPC,** for metrics, logs and traces, or over HTTP if you switch
  the exporters to `otlp_http`.
- **Delta metrics.** Target Allocator 0.159.0 exports some counters as delta
  even though cumulative temporality is requested. The development backend
  converts them with Prometheus's `otlp-deltatocumulative` feature; a
  Prometheus-based backend needs the same.
- **The `k8s.cluster.name` resource attribute,** the only thing that tells one
  site's data from another's once several sites export to one backend.

## Overriding site values safely

A `--values-for nktp=<file>` is merged over the site values by Helm. That has
two consequences:

1. **Maps merge, lists replace.** Setting one entry of a list replaces the
   whole list, including every entry the chart or the site values put there.
   The site values already restate several of the chart's lists, because they
   add to them:
   - the node Collector's `env`
   - the watchdog's, its sidecar's and each gateway's `env`, which carries
     `K8S_CLUSTER_NAME`
   - `opentelemetry-kube-stack.opentelemetry-operator.extraObjects`, which
     carries the Operator's finalizer grant

   If you override one of these, copy the whole list from
   `nktp/values.yaml` and change it there. Otherwise you silently drop the
   cluster name or the grant the Operator needs to start.
2. **Per-site values belong in the per-site file.** The same applies to every
   other item. The deploy path passes a site's own values, such as the site IP
   in SPIRE's OIDC `additionalDomains`, last, with `--values-for`. Never
   upgrade a single release by hand with only a `deploy/nico` overlay and
   `--reuse-values`: the overlay's list replaces the per-site one. See
   [troubleshooting](troubleshooting.md#spire-oidc-discovery-rejects-requests-by-ip-after-a-hand-upgrade).

## A different host layout

The node Collector assumes the NVKE reference node image's layout:

- the journal in `/var/log/journal`
- `/usr/bin/journalctl`
- `/etc/machine-id`
- auth logs in `/var/log/auth.log` or `/var/log/secure`

For a node that keeps its journal only in memory, add a `/run/log/journal`
host volume mounted at `/hostfs/run/log/journal`, and set
`daemonset.otelcol.config.receivers.journald.directory: /run/log/journal`. The
volume lists replace the site values' lists, so restate them in full, as
[`nktp/hack/e2e-values.yaml`](../../../nktp/hack/e2e-values.yaml) does for
Kind.

## Sizing up

The site values size NKTP for the smaller clusters a site controller runs on,
with a handful of workloads. As a site grows:

- **The large-target pool** carries kube-state-metrics and the control-plane
  monitors. Raise `large_target_metrics.kubernetes.resources` and its
  `GOMEMLIMIT` together when object counts grow.
- **kube-state-metrics** is one `objects` Deployment in the site values. The
  chart's own default splits it into five groups (`workloads`, `networking`,
  `storage`, `configuration`, `cluster`), each of which can be sharded.
- **The gateways** autoscale up to 64 replicas on CPU and memory, so their
  limits matter more than their replica count.
