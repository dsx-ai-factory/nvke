# spire

SPIRE for an NVKE cluster: the hardened SPIFFE charts, the OIDC upstream CA
chain, and the two NodePorts that node agents and tenant API servers reach.

```bash
helm dependency build deploy/charts/spire
helm upgrade --install nvke-spire deploy/charts/spire \
  --namespace spire-server --create-namespace
```

cert-manager must already be in the cluster: this chart creates a `Certificate`
and two `Issuer` objects.

## The trust domain is a placeholder

`global.spire.trustDomain` and `spire.spire-server.adminIDs` both ship as
`example.org`, and they must be overridden **together, in one values file**. An
adminID whose trust domain is not the server's matches nothing, and the NVKE
controller then has no admin access to SPIRE without saying so. `NOTES.txt`
reports a trust domain still left at the placeholder.

## Namespaces

| Namespace | Holds | PodSecurity |
|---|---|---|
| `spire-server` | spire-server, the controller manager, the OIDC discovery provider | `restricted` |
| `spire-system` | spire-agent, the SPIFFE CSI driver | `privileged` — both need hostNetwork, hostPID and privileged containers |

The split is the upstream chart's recommended layout, which is only consulted
when `global.spire.recommendations.enabled` is true. Pinning the agent beside
the server fails: the `restricted` label rejects it outright.

## CRDs

The SPIRE CRDs are vendored into [`crds/`](crds/) rather than taken as a chart
dependency. Helm validates every manifest in a release against the API server
before it applies any of them, so a `ClusterSPIFFEID` and the CRD that defines
it cannot be in one release:

```
no matches for kind "ClusterSPIFFEID" in version "spire.spiffe.io/v1alpha1"
ensure CRDs are installed first
```

A `crds/` directory is applied first, with a discovery refresh after it.

Helm **never** upgrades a `crds/` directory, on install or on upgrade. Moving
to a newer SPIRE chart means refreshing them and applying the result by hand:

```bash
deploy/charts/spire/hack/sync-crds.sh <spire-crds version>
kubectl apply --server-side -f deploy/charts/spire/crds/
```

`helm install --skip-crds` skips them where something else owns them.

## NodePorts

A node agent dials the SPIRE server, and a tenant kube-apiserver fetches the
OIDC discovery document, before the cluster has any ingress. Both reach a node
address.

| Service | Port | Must match |
|---|---|---|
| `spire-server-np` | 30440 | The NVKE controller's `spiffe.spire.serverPort` |
| `spire-oidc-provider-np` | 30443 | — |

The OIDC provider rejects any request whose `Host` header is not in
`spire.spiffe-oidc-discovery-provider.config.additionalDomains`, and a tenant
API server fetches the document by IP. Add the control-plane VIP and any other
literal address tenants use. A values file **replaces** a list rather than
appending to it, so an override has to restate the three entries that ship.

## The TPM allowlist

`tpm-pubhashes` is created empty and stays that way. A node's TPM EK thumbprint
cannot be known before the node exists, and the NVKE controller derives the
ConfigMap from the `ClusterStaticEntry` objects it writes. A `helm upgrade`
resets it; the controller watches it to repair exactly that drift.

## On kind

`values-kind.yaml` changes one thing, the OIDC provider replica count. The
tpmDirect node attestor stays on: a kind node has no TPM, but nothing on a kind
cluster attests by TPM — the seed's own workloads use k8sPSAT, and the TPM
allowlist is only read for nodes the controller provisions.
