# Monitor file-access restriction: rationale and compatibility

Collectors can use their mounted ServiceAccount tokens to authenticate to
protected metrics endpoints. For example, the vLLM and Dynamo operators use
controller-runtime's authenticated metrics server, documented by
[Kubebuilder](https://book.kubebuilder.io/reference/metrics.html), which checks
the token's identity and its permission to `get /metrics`.

A user who can create or update a selected ServiceMonitor can abuse
`bearerTokenFile` to send the Collector's token to an endpoint they control.
Setting `denyFSAccessThroughSMs: true` mitigates this credential disclosure,
described in [CVE-2026-47701](https://www.cve.org/CVERecord?id=CVE-2026-47701),
by dropping generated scrape jobs that reference bearer-token or TLS files in
the Collector container. See the
[upstream advisory](https://github.com/open-telemetry/opentelemetry-operator/security/advisories/GHSA-cxh2-4639-vmc5).

The node and large-target Target Allocators default this setting to `false`
for compatibility with monitors that use file references, including:

- [Dynamo's operator ServiceMonitor at commit `02da7f4`](https://github.com/ai-dynamo/dynamo/blob/02da7f44861b65758b960770e0763e9faa8e4ca4/deploy/helm/charts/platform/components/operator/templates/operator-servicemonitor.yaml#L46).
- [opentelemetry-kube-stack's API-server ServiceMonitor at commit `042e2bb`](https://github.com/open-telemetry/opentelemetry-helm-charts/blob/042e2bb5c33639aade252ba72251e5ad414af4af/charts/opentelemetry-kube-stack/templates/exporters/kube-api-server/servicemonitor.yaml#L20-L38).
- [vLLM production-stack's operator ServiceMonitor at commit `7b1164d`](https://github.com/vllm-project/production-stack/blob/7b1164dbe71b1f02ede85152bdfc2f383376dc2d/operator/config/prometheus/monitor.yaml#L16).

All three reference `bearerTokenFile` at
`/var/run/secrets/kubernetes.io/serviceaccount/token`; the kube-stack monitor
also references `tlsConfig.caFile` at
`/var/run/secrets/kubernetes.io/serviceaccount/ca.crt`.

Secret and ConfigMap references preserve authenticated scraping without allowing
monitors to choose local file paths. Follow
[Restrict monitor file access](../guides/restrict-monitor-file-access.md) to
migrate either allocator pool and enable the restriction.
