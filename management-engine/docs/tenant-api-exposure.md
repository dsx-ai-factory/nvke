# Expose the tenant API

> **Draft:** this page is not complete until the Cluster Service chart's route settings and the Cluster Service authentication contract exist. When they do, add the values, resolve the items under [Open questions](#open-questions), then delete this banner and that section.

This page describes what the tenant API expects from your site, so that you can run the Cluster Service on its own and expose its API outside the cluster. The chart installs the API, and you provide the entry point, the network policy and the identity provider around it. The tenant API is the REST API that customers use to list and manage their clusters. The operator API is separate, and this page does not expose it. Neither API is the Kubernetes API of a workload cluster, which [Access a workload cluster](../../docs/workload-clusters/access.md) covers.

Before you start, install the Cluster Service and enable the tenant API, as [Install Cluster Service with Helm](helm-installation.md#tenant-and-operator-apis) describes.

## What you provide

| Requirement | What the chart supplies | What you supply |
|---|---|---|
| Route to the API | An optional HTTPRoute, off by default, that matches `/v1/` paths only | The route settings, or your own proxy configuration |
| Entry point, hostname, DNS record and certificate | Nothing | A Gateway API implementation or proxy that terminates TLS, the hostname and its DNS record, and a certificate for it |
| Hostname ownership | Nothing | A control that lets only one namespace publish the hostname, so that no other route can receive callers' tokens |
| Network access | Nothing | A NetworkPolicy, or an equivalent control, that allows the traffic described below |
| Authentication | The settings that the Cluster Service authentication page describes | An identity provider that issues the tokens the API trusts |

## What the entry point and the tenant API agree on

- The entry point forwards the caller's `Authorization` header unchanged, because the tenant API verifies every request itself. Keep the header out of all logs.
- The entry point routes only paths under `/v1/`. The health endpoints, `/healthz` and `/readyz`, stay inside the cluster.
- The entry point applies a rate limit. The tenant API limits request bodies to 1 MiB and sets server timeouts, but it does not limit the request rate.
- Your network policy allows ingress from the entry point's pods to the tenant API pods on the tenant Service's `http` port, which is 8080 by default. It also allows egress from the tenant API to the discovery and JSON Web Key Set (JWKS) endpoints of every token issuer it trusts. The chart supplies no NetworkPolicy for the API.

> **Warning:** The entry point reaches the tenant API over plain HTTP, and the chart does not provide TLS on that hop. Every bearer token crosses it unencrypted. Any pod in a namespace that your policy admits can send requests with a token it holds, so keep that namespace limited to the entry point.

## Use your own API layer

If you put your own API layer in front of the tenant API, point it at the tenant Service and leave the chart route off. How NVKE trusts what the layer sends is defined by the Cluster Service authentication contract.

## Open questions

Resolve each of these, then delete this section.

- Chart values: add the route values and the commands from the chart once the route settings merge.
- Authentication page: link the published authentication contract where this page says "the Cluster Service authentication page".
- Install guide: [Install Cluster Service with Helm](helm-installation.md) has a "Tenant API integration responsibilities" section that points to the operator contract as tracked work. Replace that pointer with a link to this page, and align its "TLS, hostname and DNS" row with the combined "Entry point" row here. Decide whether this content belongs as a section of that guide instead of a separate page.
