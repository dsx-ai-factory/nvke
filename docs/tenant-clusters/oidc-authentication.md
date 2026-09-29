# Tenant Cluster OIDC Authentication

## Overview

OIDC authentication lets the NKE Controller and the Cluster API controllers call
a tenant cluster's Kubernetes API with short-lived SPIFFE JWTs. The tenant API
server validates each token against a site-wide OIDC issuer and maps its `sub`
claim to a Kubernetes username. [Tenant Kubernetes API
Access](kubernetes-api-access.md#service-identity-and-tenant-oidc) describes the
authentication and authorization model. This page covers turning it on.

OIDC authentication is for service identities. It does not give tenant
operators or users an OIDC login.

It is off by default. Both ways of turning it on depend on the same site
configuration:

- **New clusters** get OIDC authentication at creation while
  `nkeOIDCAuth.enabled` is `true`.
- **An existing cluster** gets it through an `UpdateCluster` API request, which
  rolls the configuration onto the running control plane.

## Site Configuration

Configure the site before enabling OIDC on any cluster, new or existing. Set
these values in the NKE Controller Helm chart. The [chart
README](../../controller/deploy/charts/controller/README.md) lists each one.

| Value | Environment variable | Purpose |
|---|---|---|
| `nkeOIDCAuth.enabled` | `NKE_OIDC_AUTH_ENABLED` | Turns OIDC authentication on for new clusters. Default `false`. |
| `nkeOIDCAuth.issuerURL` | `NKE_OIDC_AUTH_ISSUER_URL` | The OIDC issuer tenant API servers trust. Required. |
| `nkeOIDCAuth.discoveryURL` | `NKE_OIDC_AUTH_DISCOVERY_URL` | Optional. Set it when tenant API servers cannot reach the issuer's discovery document at `issuerURL`. |
| `nkeOIDCAuth.caBundleSecret.namespace`, `.name`, `.key` | `NKE_OIDC_AUTH_CA_BUNDLE_SECRET_*` | Optional. The Secret holding the issuer's PEM CA bundle, embedded in each tenant API server's configuration. Set all three or none. The Secret and key must exist and be non-empty. |

OIDC authentication also requires:

- `spiffe.enabled: true`, so the NKE Controller has a SPIFFE workload identity
  to request tokens with.
- `spiffe.trustDomain`, set to the SPIRE server's trust domain. The chart uses it
  to render the `nke-system/nke-rbac` ConfigMap, which names the SPIFFE
  identities NKE binds in each tenant cluster.
- `tenantAccess.enabled: true`, the default.
- Network access from every tenant API server to `issuerURL`, or to
  `discoveryURL` when set, to fetch the issuer's signing keys.

With `nkeOIDCAuth.enabled: true`, the chart fails to render when
`spiffe.trustDomain` is empty or `tenantAccess.enabled` is `false`. It does not
check `issuerURL` or the CA Secret. When either is wrong, cluster provisioning
and the `UpdateCluster` rollout fail.

Enabling OIDC on an existing cluster uses these same settings, so set
`nkeOIDCAuth.enabled: true` before you use it. The chart grants the NKE
Controller read access to the CA Secret only when `enabled` is `true`. Every
cluster created afterwards also gets OIDC authentication.

If `spiffe.jwtExchangeEndpoint` is set, tenant clusters must trust the
exchange's issuer, so `issuerURL` names that issuer. The chart then also
requires `spiffe.cweSubject` and the Cluster API subject overrides. See the
chart README.

The API has no per-cluster override: `CreateClusterRequest` has no OIDC field.
Every cluster uses the same issuer, and the token audience is the cluster name.
Changing the issuer settings later does not update existing control-plane nodes,
but new and replaced control-plane nodes get the new settings. Do not change
them while OIDC-enabled clusters exist.

## Enabling OIDC on an Existing Cluster

Send `enableOidc: true` to `PATCH /api/clusters/{clusterID}`. See
[Authenticating to the NKE API](../../controller/docs/authenticating-to-the-api.md)
for how to obtain `TOKEN`. `NKE_CONTROLLER_URL` is the NKE Controller's base URL.

```bash
curl -X PATCH \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"enableOidc": true}' \
  "${NKE_CONTROLLER_URL}/api/clusters/${CLUSTER_ID}"
```

The request returns `200` and starts a control-plane rollout. The NKE Controller
updates one control-plane node at a time, and each node's API server must
report ready before the next node starts. While the rollout runs,
`GET /api/clusters/{clusterID}` includes this under `conditions`:

```json
"updateInProgress": {
  "type": "oidc_enable",
  "message": "enabling OIDC authentication",
  "startedAt": "..."
}
```

When the rollout succeeds, `updateInProgress` is cleared. The cluster API
response has no field that shows OIDC is on. Use [Verifying OIDC on a
Cluster](#verifying-oidc-on-a-cluster) to confirm it.

When the rollout fails, `updateInProgress` is cleared and
`conditions.updateFailed` carries the message `OIDC enablement failed`. The
cause is in the NKE Controller logs. A failure can leave some or all API servers
already accepting OIDC tokens:

- When a node fails to apply the change, the NKE Controller tries to undo it on
  the nodes it already updated. The undo is best-effort.
- When a node fails its readiness check before it is changed, the rollout stops
  and nodes already updated keep the change.
- When every node was updated and a later step fails, nothing is undone.

To retry, send the same request again. Nodes that already have the change are
left as they are.

Enabling OIDC cannot be undone. If OIDC is already enabled, the request starts
no rollout and returns `200`, as long as it passes the checks below.

### Request Validation

The request returns `400` when:

- it sets `"enableOidc": false`;
- the cluster is not yet provisioned, or is being deleted;
- it also sets `enableEtcdEncryption: true`;
- it also sets a non-empty `controlPlaneKubernetesVersion`.

It returns `409` while another control-plane operation is running on the
cluster: an OIDC rollout, etcd encryption enablement or key rotation, a
Kubernetes upgrade, or control-plane node recycling. The message names the
running operation and when it started.

### Rollout Requirements

These are checked after the request is accepted. A failed check ends the
rollout with `updateFailed` set, not with an API error.

Before any node is changed:

- The cluster runs Kubernetes 1.30 or later.
- The cluster name is a valid DNS subdomain, because it is used as the token
  audience.
- Every control-plane node agent supports the `PatchAPIServerManifest` RPC.
  Upgrade older agents first.
- The node agents are reachable and the tenant API server is healthy.
- `issuerURL` is set, and the CA Secret, if configured, can be read.

Before each node is changed, its node agent must report kubeadm as ready.

## Verifying OIDC on a Cluster

Check the result on the Site Controller cluster. After OIDC is enabled, the
tenant-access controller applies the NKE RBAC into the tenant cluster and
publishes these Secrets in `nke-system`:

| Secret | Contents |
|---|---|
| `<cluster>-kubeconfig` | The NKE Controller's kubeconfig. It runs the SPIFFE exec-auth plugin to obtain a JWT. |
| `<cluster>-cluster-trust-bundle` | The tenant cluster CA, under key `ca.crt`. |
| `<cluster>-admin-kubeconfig` | The kubeadm `admin.conf`. It is published whether or not OIDC is on. |

The `nke.nvidia.com/kubeconfig-mode` label on `<cluster>-kubeconfig` shows which
mode a cluster is in: `spire` with OIDC, `admin` without.

```bash
kubectl -n nke-system get secret "${CLUSTER_NAME}-kubeconfig" \
  -o jsonpath='{.metadata.labels.nke\.nvidia\.com/kubeconfig-mode}'
```

To check the identity and permissions the NKE Controller receives, follow step 5
of the [manual validation
checklist](kubernetes-api-access.md#manual-validation-checklist).

If the label stays `admin` after the rollout succeeds, check the NKE Controller
logs for the tenant-access controller. It does not publish the OIDC kubeconfig
when it cannot read the `manifest.yaml` key of the `nke-rbac` ConfigMap, or
cannot apply that RBAC into the tenant cluster.
