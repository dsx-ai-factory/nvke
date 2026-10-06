# Telemetry on a site controller: configuration

The NKTP chart's defaults are for a multi-node cluster, and they know nothing
about NKE. Everything a site controller needs differently is in
[`controller/deploy/nke-sbom/nktp/values.yaml`](../../../controller/deploy/nke-sbom/nktp/values.yaml),
which the `nktp` item applies over the chart. This page explains what it
changes and why, and how a site overrides it.

## What the site values change

| Setting | Why |
|---|---|
| **The Operator's finalizer grant** (`opentelemetry-kube-stack.opentelemetry-operator.extraObjects`) | A Role and RoleBinding granting the Operator `update` on its own Deployment's `deployments/finalizers`. Without it, the Operator crash-loops on NKE clusters; see [troubleshooting](troubleshooting.md#the-operator-crash-loops-with-cannot-set-blockownerdeletion). |
| **`k8s.cluster.name`** on the watchdog and the three gateways | A `K8S_CLUSTER_NAME` environment variable read from ConfigMap `nktp-cluster`, which `nke-base-config` renders from BootstrapConfig's `ClusterName`, plus a `resource/cluster` processor in every pipeline. NKE's kubeadm configuration names every cluster `kubernetes`, so the name has to come from outside the cluster. |
| **The API audit log path** (`filelog/kube-api-audit.include`) | NKE's kubeadm configuration writes the API server's audit log to `/var/log/kubernetes_audit/audit.log`, not the chart's default path. |
| **Control-plane monitors** (`kubeScheduler`, `kubeControllerManager`, `kubeProxy` off) | NKE binds the scheduler and controller manager to `127.0.0.1`, and OVN clusters run no kube-proxy. Left on, they would show as permanently down targets. The API server, etcd and CoreDNS monitors stay on and work. |
| **metrics-server on** | Nothing else serves `metrics.k8s.io` on a site controller, and the Collector autoscalers read it. |
| **Sizing** | One replica of everything, autoscalers starting at one, small requests and limits for every Collector and Target Allocator, and one kube-state-metrics Deployment for all object types instead of five. Sized for the smaller clusters a site controller runs on. |

## Connecting a real backend

By default every exporter points at the development backend, `lgtm`, in the
`nktp` namespace. For anything beyond evaluation, point them at your own
backend and turn `lgtm` off:

1. **Write a values file for the site.** The keys are listed in the chart
   README's [The backend](../../../nktp/deploy/charts/nktp/README.md#the-backend):
   the `otlp_http` exporter of the watchdog and each gateway, and the
   watchdog's three `service.telemetry` exporters, plus
   `otel-lgtm.enabled: false`.
2. **Pass it to the deploy path** with `--values-for nktp=<file>`. It is
   applied after the item's own values, so it wins.
3. **Keep credentials out of values.** Put them in a Secret in `nktp`,
   referenced from the workload's `kubernetes.env` and read through
   `${env:NAME}` in the exporter configuration.

The backend has to accept:

- **OTLP over HTTP,** for metrics, logs and traces.
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
   - the node Collector's `env`, `volumes`, `volumeMounts` and
     `securityContext.capabilities.add`
   - the watchdog's and each gateway's `env`, which carries `K8S_CLUSTER_NAME`
   - the watchdog's and each gateway's pipeline `processors`, which carry
     `resource/cluster`
   - `opentelemetry-kube-stack.opentelemetry-operator.extraObjects`, which
     carries the Operator's finalizer grant

   If you override one of these, copy the whole list from
   `nktp/values.yaml` and change it there. Otherwise you silently drop the
   cluster name, the journal mount, or the grant the Operator needs to start.
2. **Per-site values belong in the per-site file.** The same applies to every
   other item. The deploy path passes a site's own values, such as the site IP
   in SPIRE's OIDC `additionalDomains`, last, with `--values-for`. Never
   upgrade a single release by hand with only an nke-sbom overlay and
   `--reuse-values`: the overlay's list replaces the per-site one. See
   [troubleshooting](troubleshooting.md#spire-oidc-discovery-rejects-requests-by-ip-after-a-hand-upgrade).

## A different host layout

The node Collector assumes the NKE reference node image's layout:

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
