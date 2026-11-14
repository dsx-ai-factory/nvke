# Expose the tenant API

> **Draft:** this page is not complete until the Cluster Service chart's route
> and NetworkPolicy settings and the Cluster Service authentication contract
> exist. When they do, fill in the steps, resolve the items under
> [Open questions](#open-questions), then delete this banner and that section.

This page describes how to make the tenant API reachable from outside the cluster that runs the Cluster Service, through a TLS-terminating entry point that you operate. Do this after you install the Cluster Service and configure its authentication, and before you give any customer the hostname.

The Cluster Service serves two APIs. The tenant API is the REST API that customers use to list and manage their clusters. The `tenant-api` binary serves it over plain HTTP on port 8080. The operator API is separate, and this page does not expose it. Neither is the Kubernetes API of a workload cluster, which [Access a workload cluster](../../docs/workload-clusters/access.md) covers.

## Before you begin

- You have installed the NVIDIA Kubernetes Engine (NVKE) Cluster Service with its Helm chart, as [Install Cluster Service with Helm](helm-installation.md) describes.
- You have set up authentication for the Cluster Service, as the Cluster Service authentication page describes, so that it rejects requests without a valid token.
- You run a Gateway API implementation, or another proxy that terminates TLS, in a namespace that holds nothing else.
- You have the other items under [What you provide](#what-you-provide).

## What you provide

The chart supplies the Service, an optional route and a NetworkPolicy rule, all set from chart values. You supply everything else. Nothing in the chart installs a Gateway, a certificate or a DNS record.

| Requirement | What the chart supplies | What you supply |
|---|---|---|
| Route to the API | An optional HTTPRoute, off by default, that matches `/v1/` paths only | The route settings, or your own proxy configuration |
| Entry point | Nothing | A Gateway API implementation or proxy that terminates TLS |
| Hostname | A route setting | The hostname customers use |
| Hostname ownership | Nothing | A control that lets only one namespace publish the hostname, so that no other route can receive callers' tokens. Limiting which namespaces may attach routes is not enough on its own |
| DNS record | Nothing | A record that points the hostname at the entry point |
| Certificate | Nothing | A certificate for the hostname on the entry point |
| Client access | Nothing | Whether the entry point is private or public, which source networks may connect, and the filtering in front of it |
| Authentication | The settings that the Cluster Service authentication page describes | An identity provider that issues the tokens the API trusts |
| Network access to the API | A NetworkPolicy rule that admits one namespace | The namespace of the entry point's pods |

## Configure the entry point

The entry point and `tenant-api` agree on the following when the entry point passes requests through, which is what the chart route does.

- It forwards the caller's `Authorization` header unchanged, because `tenant-api` verifies and authorizes every request itself. Keep the header out of all logs.
- It routes only paths under `/v1/`. The health endpoints, `/healthz` and `/readyz`, stay inside the cluster.
- It applies a rate limit. `tenant-api` limits request bodies to 1 MiB and sets server timeouts, but it does not limit the request rate, so an entry point without a rate limit leaves the API open to floods.

`tenant-api` fetches the discovery document and the JSON Web Key Set (JWKS) of every configured issuer to verify tokens, so allow that egress from its namespace. The API returns relative URLs only, so one deployment works behind any hostname.

> **Warning:** The entry point reaches `tenant-api` over plain HTTP on port 8080, and the chart does not provide TLS on that hop. Every bearer token crosses it unencrypted. The NetworkPolicy rule admits all pods in the namespace you name and no pods from other namespaces, so any pod in that namespace can connect and send requests with a token it holds. Keep that namespace limited to the entry point. Reading the traffic in transit takes access to the node or the network, not only a pod in the namespace.

## Expose the API through your Gateway

1. Set up the entry point, hostname, certificate, DNS record and hostname ownership from the table above.

2. In the Cluster Service chart values, enable the route and set the hostname and the parent Gateway: its name, its namespace and the listener to attach to.

3. In the chart values, set the namespace that the NetworkPolicy admits API traffic from to the namespace where the Gateway's data-plane pods run. Some Gateway API implementations run those pods outside the namespace of the Gateway object, and the NetworkPolicy matches pods.

4. Upgrade the Helm release with the new values.

   The chart creates no route for the operator API, and the operator API's NetworkPolicy does not admit the Gateway's namespace, so a route attached to your Gateway cannot reach it.

If you use a proxy that is not a Gateway API implementation, leave the route off, point the proxy at the tenant API's Service on port 8080, follow [Configure the entry point](#configure-the-entry-point), and set the NetworkPolicy's admitted namespace to the proxy's namespace.

## Put your own API layer in front

You can put your own API layer between callers and NVKE. The layer authenticates the caller, then calls the Service with a credential of its own, so the caller's token is not passed through. Point the layer at the Service and leave the chart route off. How NVKE trusts the layer's credential is defined on the Cluster Service authentication page.

NVKE sees the layer as the caller, not the person behind it, so the layer must keep its own record of who made each request. The layer's credential crosses the same plain-HTTP hop as any other token: set the NetworkPolicy's admitted namespace to the layer's namespace, and never log the credential.

## Verify

Run these from outside the cluster.

1. List clusters with a valid token.

   ```bash
   curl -H "Authorization: Bearer <TOKEN>" https://<HOSTNAME>/v1/clusters
   ```

   `<TOKEN>` is a token from a trusted issuer and `<HOSTNAME>` is the API hostname. The response lists the customer's clusters.

2. Send the same request without a token.

   ```bash
   curl -i https://<HOSTNAME>/v1/clusters
   ```

   The response is `401`.

3. Request `https://<HOSTNAME>/healthz` and `https://<HOSTNAME>/readyz`. Neither returns a health response, and typically both return `404`. The operator API's paths are not reachable either.

## Open questions

Resolve each of these, then delete this section.

- Chart values: the tenant API Deployment and Service are in draft pull request #2309 (CLI-126), which is not merged. It adds the `tenantAPI` values, including `tenantAPI.service.port` and `tenantAPI.namespace`, and names the Service `<release>-tenant-api` with a named `http` port. The route and NetworkPolicy values are CLI-130, which it blocks, so they do not exist yet. Name the values and add the commands for the steps above from the merged chart.
- Authentication page: the authentication and authorization story is CLI-103, in progress. The identity, org routing and permission contract (CLI-50) and the authenticated API service (CLI-51) are not started. Link the published contract where this page says "the Cluster Service authentication page", and confirm that its issuer and tenant claim settings are covered by that link and not restated here.
- Install guide: [Install Cluster Service with Helm](helm-installation.md) has a "Tenant API integration responsibilities" section that points to the operator contract as tracked work. Pull request #2309 rewrites part of that section, so rebase on it first. Then replace the pointer with a link to this page, and align its "TLS, hostname and DNS" row, which says the chart takes certificates through integration settings, with the "Certificate" row here, which says the chart supplies nothing.
- Operator-specific example: add a worked example for a deployment that has a shared Gateway, once its hostname governance, client reachability and limits are decided.
