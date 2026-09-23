# nkectl

NKE control-plane CLI. Bootstraps site controllers and manages tenant cluster lifecycle.

**Module:** `github.com/dsx-ai-factory/nke/cli`  
**Binary:** `nkectl`  
**Releases:** `cli/v*` tags

## What it does

- **Site bootstrap** — `nkectl bootstrap site` creates an ephemeral Kind cluster, deploys CWE onto it, then drives kubeadm init/join across pre-provisioned nodes via `nke-agent` gRPC; no SSH required
- **Cluster lifecycle** — `nkectl get/create/patch/delete clusters` manages tenant clusters through the CWE API
- **Site spec** — `nkectl generate sitespec` writes a site spec from a node CSV; `nkectl bootstrap site --dry-run` validates one without touching a node
- **Config** — `nkectl config set-site` points the CLI at a site controller.

## Installation

```bash
# linux/amd64
curl -fsSL -o nkectl \
  https://github.com/dsx-ai-factory/nke/releases/latest/download/nkectl-linux-amd64
chmod +x nkectl && sudo mv nkectl /usr/local/bin/

# linux/arm64
curl -fsSL -o nkectl \
  https://github.com/dsx-ai-factory/nke/releases/latest/download/nkectl-linux-arm64
chmod +x nkectl && sudo mv nkectl /usr/local/bin/

# macOS (amd64 / arm64)
curl -fsSL -o nkectl \
  https://github.com/dsx-ai-factory/nke/releases/latest/download/nkectl-darwin-arm64
chmod +x nkectl && sudo mv nkectl /usr/local/bin/
```

```bash
nkectl version
```

## Quick reference

```bash
# Bootstrap a new site controller
NKECTL_CWE_IMAGE=nvcr.io/<org>/nke-controller:edge \
NKECTL_CAPI_PROVISIONED_IMAGE=nvcr.io/<org>/cluster-api-provider-provisioned:edge \
  nkectl bootstrap site -f sitespec.yaml
nkectl bootstrap site -f sitespec.yaml --dry-run              # validate only
nkectl bootstrap site -f sitespec.yaml --keep-bootstrap-cluster     # leave Kind cluster for debugging

# Point the CLI at a site controller
nkectl config set-site --url https://cwe.example.com --name default \
  --kubeconfig ~/.kube/site.kubeconfig

# Infrastructure providers (alpha)
nkectl get providers                   # NOT-INSTALLED / NOT-CONFIGURED / DEGRADED / CONFIGURED
nkectl get providers -o json

# Tenant cluster lifecycle
nkectl get clusters                    # table with AGE and status
nkectl get clusters -o json            # JSON output
nkectl get clusters <name>... -o yaml  # full detail for named clusters
nkectl create cluster -f <spec-file>   # or: cat spec.yaml | nkectl create cluster -f -
nkectl patch cluster my-cluster --patch 'controlPlaneKubernetesVersion: 1.31.0'
nkectl patch cluster my-cluster --patch-file patch.yaml
nkectl delete cluster <name>           # deletes immediately; there is no prompt

# Node pools
nkectl create nodepool -f <spec-file>  # or: cat pool.yaml | nkectl create nodepool -f -
nkectl get nodepools                   # every pool on the site
nkectl get nodepools --cluster <name>  # pools of one cluster
nkectl get nodepools <name>... --cluster <name> -o yaml  # full detail for named pools
nkectl patch nodepool <name> --cluster <name> --patch 'desiredNodeCount: 8'
nkectl patch nodepool <name> --cluster <name> --patch-file patch.yaml
nkectl delete nodepool <name> --cluster <name>
```

## Authenticating to CWE

`nkectl config set-site` command configures the management-cluster kubeconfig with the site.
nkectl commands use it to mint an audience-scoped token for the admitted
identity `nke-clients/nke-client`. The audience comes from ConfigMap
`nke-auth-config` in `cloud-workflow-engine`.

If that ConfigMap is missing, the CLI treats authentication as off and sends no
`Authorization` header — the same as a CWE that was installed with
`apiAuth.enabled` unset.

The kubeconfig identity needs:

- `create` on `serviceaccounts/token` for `nke-client` in `nke-clients`
- `get` on `secrets` in `nke-system`, for `nkectl get kubeconfig` and
  `nkectl get providers`, which read the management cluster directly rather
  than through CWE

A cluster-admin kubeconfig already has both. Least-privilege grants, and
creating the service account on a real site, are in
[Configuring NKE API Authentication](../controller/docs/configuring-api-authentication.md).

## Infrastructure providers

`nkectl get providers` reports which Cluster API infrastructure providers a site
controller can actually use. It reads the site's Kubernetes cluster directly
rather than the CWE API, so it needs the kubeconfig registered with
`nkectl config set-site --kubeconfig` and `kubectl` on `PATH`.

| State | Meaning |
|---|---|
| `NOT-INSTALLED` | The provider's CRDs are absent, or present but not established. Not an error — a site may legitimately run one provider, both, or neither. |
| `NOT-CONFIGURED` | CRDs are established but no credentials Secret was found. For LaunchLayer this is a supported configuration: without one its client is unauthenticated. |
| `DEGRADED` | Configured, but the provider's manager is not available or was not found. |
| `CONFIGURED` | CRDs established, manager available, credentials in place. This does **not** mean the credentials work — nothing here authenticates against the provider's backend. |
| `UNKNOWN` | A probe failed for a reason other than absence, most often a kubeconfig that may not read Secrets in the manager's namespace. |

The providers listed are the ones nkectl knows how to describe. That is not a
restriction: CWE accepts any infrastructure manifest written by hand.

## Configuration

What a bootstrap is allowed to trust describes the *site*, so it comes from the
site spec file rather than from flags or the environment. See
[`docs/sitespec.md`](docs/sitespec.md).

| Field | |
|---|---|
| `cluster.*.tpmEkThumbprint` | Which nodes may attest to SPIRE. No node listed, no node admitted; a site spec with none deploys no TPM attestor at all. |
| `spire.caCertFile` / `caKeyFile` | A CA for SPIRE to chain its own to, so a node given that certificate when it was provisioned verifies the server on first contact. Without it SPIRE is self-signed. |

### What gets installed

`bootstrap site` deploys a SPIRE server, so nodes with a hardware identity have
something to attest to and CWE gets a workload identity of its own. It comes
from the SPIFFE project's own hardened charts, pinned alongside the Temporal pin
in [`internal/cwedeploy/cwedeploy.go`](internal/cwedeploy/cwedeploy.go)
and installed with `helm --repo`, so nothing is added to your global Helm
repository list. Everything it names is public, so a bootstrap needs no registry
credential.

Everything static about the deployment lives in
[`internal/cwedeploy/manifests/spire-values.yaml`](internal/cwedeploy/manifests/spire-values.yaml).
`nkectl` sets only what it cannot know ahead of time: the trust domain and
cluster name, CWE's admin identity, the CA to chain to, and the TPM thumbprints
allowed to attest.

Two of the values it sets have to agree with CWE and are wrong by default on
both sides, so both halves are set from one constant in
[`internal/cwedeploy/cwedeploy.go`](internal/cwedeploy/cwedeploy.go):

- **the controller-manager class.** CWE stamps its `ClusterStaticEntry` objects
  with its configured class and the SPIRE controller manager only acts on
  objects carrying its own. The chart derives `<namespace>-<release>` when the
  class is unset; CWE defaults to `spire-mgmt-spire`. Left alone they differ,
  and CWE writes entries nobody reads — with both components healthy.
- **the server port.** The chart's Service defaults to 443, while CWE dials
  `spire-server.spire-server.svc.cluster.local:8081` for join tokens. The
  Service is moved to 8081 so that default is right without configuring CWE.
  The agent's server port has to move with it: the chart keeps that as its own
  value, also defaulting to 443, and does not derive it from the Service. Move
  one without the other and the agent dials a port the Service does not
  publish, which a ClusterIP blackholes rather than refuses — so it surfaces as
  a node-attestation `i/o timeout` that names nothing useful.

Note that the spire server is not yet accessible outside the kind cluster. We will wire that in as the final PR.

| Variable | Default | Purpose |
|---|---|---|
| `NKECTL_SPIFFE_TRUST_DOMAIN` | `<cluster>.nke.local` | Trust domain for the identities issued |

The cluster name SPIRE is told is the ephemeral cluster's own name, from
`NKECTL_BOOTSTRAP_CLUSTER_NAME` — there is no separate SPIFFE variable for it,
because the bootstrap cluster has one name and two ways to say it could only
ever disagree.

When SPIRE is enabled, `bootstrap site` exports the live `ClusterStaticEntry`
CRD definition and CWE's cluster-scoped workload registration policy before it
removes the bootstrap cluster. It writes `clusterstaticentry-crd.yaml` and
`clusterstaticentries.yaml` in the same directory as the site kubeconfig. The
files contain no source UIDs, resource versions, status, or other server-owned
metadata.

### Bootstrap cluster and CWE

| Variable | Default | Purpose |
|---|---|---|
| `NKECTL_BOOTSTRAP_CLUSTER_NAME` | `nkectl-bootstrap` | Name of the ephemeral cluster |
| `NKECTL_CWE_IMAGE` | required | CWE image to deploy. Side-loaded from the local image store, so it must already be pulled. |
| `NKECTL_CWE_TEMPORAL_CHART` | `temporal` from `https://go.temporal.io/helm-charts` | Temporal chart, as an OCI reference or a local path. Set, it replaces the repo lookup. |
| `NKECTL_CWE_SPIRE_CHART` | `spire` from `https://spiffe.github.io/helm-charts-hardened/` | SPIRE chart, same semantics |
| `NKECTL_CWE_SPIRE_CRDS_CHART` | `spire-crds` from `https://spiffe.github.io/helm-charts-hardened/` | SPIRE CRDs chart, same semantics |
| `NKECTL_CWE_HELM_TIMEOUT` | `5m` | Bounds each `helm upgrade --install --wait` |
| `NKECTL_CWE_READY_TIMEOUT` | `5m` | Bounds the wait for CWE's `/readyz` |
| `NKECTL_BOOTSTRAP_DIAGNOSTICS_DIR` | `cluster-diagnostics` | Where a failed bootstrap writes a cluster dump before tearing the cluster down (`--diagnostics-dir`) |

The controller and Postgres charts are carried in the binary and are never
pulled, so they have no variable. For the three that are pulled, the chart
version stays pinned in `nkectl` whichever source the chart comes from; helm
does not check a local path against the pin, so a tarball has to be the pinned
version. With those three variables set, no Helm chart is pulled from the
internet. The images those charts run still come from their
registries (`NKECTL_BOOTSTRAP_DOCKERHUB_MIRROR` covers Docker Hub), and
`clusterctl init` still fetches Cluster API and cert-manager from `github.com`.

## When a bootstrap fails

`bootstrap site` removes the ephemeral Kind cluster on every exit path, so by
the time the error reaches you there is nothing left to ask. To keep the
evidence, a failed run collects a `kubectl cluster-info dump` — every
namespace's objects and pod logs — plus the events, object state and helm
releases that explain a pod which never started, and writes it out *before* the
teardown.

It lands in `cluster-diagnostics/`, overridable with `--diagnostics-dir` or
`NKECTL_BOOTSTRAP_DIAGNOSTICS_DIR`. A successful run writes nothing. CI uploads
the directory as an artifact when the bootstrap E2E fails.

## Build from source

```bash
# From monorepo root
make build          # builds all binaries including cli/nkectl into bin/

# Or from this directory
make build
```

## Platform support

| OS | amd64 | arm64 |
|---|---|---|
| Linux | ✓ | ✓ |
| macOS | ✓ | ✓ |

## Related docs

- [Site Controller Bootstrap](../docs/site-bootstrap/overview.md)
- [Architecture](../docs/architecture.md)
- [Authenticating to the NKE API](../controller/docs/authenticating-to-the-api.md)
- [Configuring NKE API Authentication](../controller/docs/configuring-api-authentication.md)
