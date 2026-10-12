# Telemetry on a site controller: monitoring your service

NKTP collects every Pod's logs, every Pod's state, and the node's metrics
without being asked. Application metrics are different: NKTP scrapes a
service only when the service declares how to be scraped, with a
`ServiceMonitor` or `PodMonitor`. NKTP ships no monitors for other
components. **Each service ships its own**, normally from the
service's Helm chart, so the monitor is versioned, labeled and removed with
the service it describes.

Today, the NVKE controller, Temporal and SPIRE do this, switched on by
their `deploy/nico` overlays. cert-manager, NodeLocal DNSCache, OVN-Kubernetes and
CAPNICo don't, so their metrics aren't collected until their owners add a
monitor.

## Add a monitor

Most charts already have a switch for it, and it is the first thing to look
for:

| Component | Value |
|---|---|
| This repository's controller chart | `serviceMonitor.enabled` |
| Temporal | `server.metrics.serviceMonitor.enabled` |
| SPIRE | `spire-server.telemetry.prometheus.podMonitor.enabled`, `spire-agent.telemetry.prometheus.podMonitor.enabled` |
| cert-manager | `prometheus.servicemonitor.enabled` |

Otherwise, ship one yourself. A `ServiceMonitor` selects a Service by label and
names the Service port that serves metrics:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: my-service
  namespace: my-namespace
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: my-service
  endpoints:
    - port: metrics        # the Service port's name, not its number
      interval: 30s
```

- **Use a `PodMonitor`** when no Service fronts the metrics port. It selects
  Pods and names a container port.
- **Put the monitor in your own namespace.** NKTP discovers monitors in every
  namespace, and a monitor needs no NKTP-specific label. A monitor that needs
  credentials is the exception; see
  [authenticated endpoints](#authenticated-or-https-endpoints).

**Ordering.** The monitoring CRDs come with NKTP. On a site controller, the
`nktp` item installs them before every item that follows it in
`deploy/nico/nvke-deploy.yaml`, so a chart installed later in
the path can create its monitor. A component installed before NKTP, or outside
the path (cert-manager and CAPI through `clusterctl init`, OVN and CoreDNS
through bootstrap), has to add its monitor after NKTP is up.

## Which Collector scrapes it

| Monitor | Scraped by | Use it for |
|---|---|---|
| No label (the default) | The Collector on the node where each target runs | Almost everything: one target per Pod |
| Labeled `nktp.nvidia.io/large-target: "true"` | The large-target pool, which spreads targets across its replicas | A target with a very large response, like kube-state-metrics, or one that doesn't run on a cluster node |

The node Collectors assign each target to the node it runs on, so a target
that doesn't resolve to a node, such as a static address outside the cluster,
stays unassigned. Label those monitors for the large-target pool.

## Let the Collectors in

If your service restricts who may reach its metrics port with a
NetworkPolicy, admit the `nktp` namespace. The controller chart does this with
`metrics.networkPolicy.additionalNamespaces: [nktp]`.

## Authenticated or HTTPS endpoints

- **Credentials come from Secrets, not files.** The node Collectors reject
  monitor endpoints that reference files inside the Collector container
  (`bearerTokenFile`, `tlsConfig.caFile` and the like), so a monitor cannot
  borrow the Collector's own ServiceAccount token. Reference a Secret instead,
  such as `authorization.credentials` or `tlsConfig.ca.secret`.
- **The allocator must be allowed to read those Secrets.** A monitor's Secret
  references resolve in the monitor's own namespace, and the allocator reads
  Secrets only in namespaces listed in
  `daemonset.kubernetes.targetAllocator.prometheusCR.secretNamespaces` in the
  NKTP values. The chart then creates a Role there granting it Secret and
  ConfigMap read access, and passes the credentials to its Collectors over
  mTLS.
- **Keep the monitor and its credentials together.** The grant covers every
  Secret in the namespace, so rather than listing your service's namespace,
  put the monitor and its credential Secret in a dedicated namespace, list
  that one, and point the monitor back at your service with
  `spec.namespaceSelector.matchNames: [<service-namespace>]`.
- **A self-signed certificate** can be scraped with
  `tlsConfig.insecureSkipVerify: true`. The connection is still encrypted, but
  the target isn't authenticated. Serving a certificate from a CA the monitor
  can reference is better.

## Check it

A few minutes after the monitor exists, its targets should report `up`. Port-forward
the backend and define the `promql` helper as in
[verification](verification.md#2-the-signals-arrive), then:

```bash
promql 'up{job="<service-name>"}'     # 1 per target; a PodMonitor's job is <namespace>/<monitor>
promql 'count({job="<service-name>", __name__!="up"})'   # the service's own series
```

- **No `up` series at all:** the monitor matched nothing (check its selector
  and port name), or the target was left unassigned.
- **`up` is 0:** the Collector tried the target and failed. The Collectors
  scrape and push over OTLP, so the backend has no record of why. The
  Collector that scraped it logs each failure as `Failed to scrape Prometheus
  endpoint`, with the error and the target's labels:

```bash
kubectl -n nktp logs -l app.kubernetes.io/component=opentelemetry-collector \
  --prefix --max-log-requests 20 --since 10m \
  | grep 'Failed to scrape Prometheus endpoint' | grep '<service-name>'
```
