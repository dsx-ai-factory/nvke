# nvkectl

NVKE control-plane CLI. Bootstraps management clusters and manages tenant cluster lifecycle.

**Module:** `github.com/dsx-ai-factory/nvke/cli`  
**Binary:** `nvkectl`  
**Releases:** `cli/v*` tags

## What it does

- **Site bootstrap** — `nvkectl bootstrap site` creates an ephemeral Kind cluster on nerdctl/containerd, deploys Postgres, Temporal, SPIRE (skipped with `--disable-mtls`, which dials the agents in plaintext), Cluster API and CWE onto it, then drives kubeadm init/join across pre-provisioned nodes, or machines CAPNICo allocates with `infra.provider: nico`, via `nvke-agent` gRPC; no SSH required, and Docker is not supported
- **Cluster lifecycle** — `nvkectl get/create/patch/delete clusters` manages tenant clusters through the CWE API
- **Site spec** — `nvkectl generate sitespec` writes a site spec from a node CSV; `nvkectl bootstrap site --dry-run` validates one without touching a node
- **Config** — `nvkectl config set-site` points the CLI at an NVKE management plane.

## Installation

```bash
# Set RELEASE to a cli/ tag listed at https://github.com/dsx-ai-factory/nvke/releases
export RELEASE=cli/vX.Y.Z

# Detects linux/darwin and amd64/arm64 (uname reports x86_64, aarch64, or arm64)
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
curl -fsSL -o nvkectl \
  "https://github.com/dsx-ai-factory/nvke/releases/download/${RELEASE}/nvkectl-${OS}-${ARCH}"
chmod +x nvkectl && sudo mv nvkectl /usr/local/bin/
```

```bash
nvkectl version
```

## Command reference

[**Every command and flag**](docs/reference/README.md), one page per command,
generated from the command tree this binary runs. The quick reference below is
the short list of what operators reach for most.

## Quick reference

```bash
# Write a site spec from a node CSV, then bootstrap a new management cluster with
# the images `make -C cli images` built. Needs nerdctl+containerd, kind, helm,
# kubectl and clusterctl on PATH.
nvkectl generate sitespec my-site --nodes hosts.csv > sitespec.yaml
nvkectl bootstrap site -f sitespec.yaml
# ... or with images you pushed to a registry
nvkectl bootstrap site -f sitespec.yaml \
  --nvke-controller-image nvcr.io/<org>/nvke-controller:edge \
  --provisioned-provider-image nvcr.io/<org>/cluster-api-provider-provisioned:edge
nvkectl bootstrap site -f sitespec.yaml --dry-run                 # validate only
nvkectl bootstrap site -f sitespec.yaml --keep-bootstrap-cluster  # leave the Kind cluster for debugging
nvkectl bootstrap site -f sitespec.yaml --timeout 45m \
  --kubeconfig-out ./my-site.kubeconfig                          # longer provisioning wait; pick the output path
nvkectl bootstrap site -f sitespec.yaml --disable-mtls            # no SPIRE, plaintext agents; development only

# Point the CLI at a management plane
nvkectl config set-site --url https://cwe.example.com --name default \
  --kubeconfig ~/.kube/site.kubeconfig
nvkectl config use-site <name>          # switch between configured sites
nvkectl config view                     # print the config
nvkectl --site <name> get clusters      # override the active site for one call

# Infrastructure providers (alpha)
nvkectl get providers                   # STATE, CREDENTIALS and LAST CHECK
nvkectl get providers -o json

# Tenant cluster lifecycle
nvkectl get clusters                    # table with AGE and status
nvkectl get clusters -o json            # JSON output
nvkectl get clusters <name>... -o yaml  # full detail for named clusters
nvkectl create cluster -f <spec-file>   # or: cat spec.yaml | nvkectl create cluster -f -
nvkectl patch cluster my-cluster --patch 'controlPlaneKubernetesVersion: 1.31.0'
nvkectl patch cluster my-cluster --patch-file patch.yaml
nvkectl get kubeconfig <name> --output-file ./tenant.kubeconfig   # admin kubeconfig, read from the site cluster
nvkectl delete cluster <name>           # deletes immediately; there is no prompt

# Node pools
nvkectl create nodepool -f <spec-file>  # or: cat pool.yaml | nvkectl create nodepool -f -
nvkectl get nodepools                   # every pool on the site
nvkectl get nodepools --cluster <name>  # pools of one cluster
nvkectl get nodepools <name>... --cluster <name> -o yaml  # full detail for named pools
nvkectl patch nodepool <name> --cluster <name> --patch 'desiredNodeCount: 8'
nvkectl patch nodepool <name> --cluster <name> --patch-file patch.yaml
nvkectl delete nodepool <name> --cluster <name>

# Machine images and releases. Example files are in cli/examples/. Published
# content is immutable, so to change one, create a new name
nvkectl create machineimage -f machineimage.yaml
nvkectl create clusterrelease -f clusterrelease.yaml --dry-run=client
nvkectl create nodepoolrelease -f nodepoolrelease.yaml
nvkectl get machineimages
nvkectl get clusterreleases <name>... -o yaml
nvkectl deprecate nodepoolrelease <name>
nvkectl delete clusterrelease <name>
```

Global flags: `--site <name>` overrides the active site for one invocation, `-v`
enables debug logging, `--json-logs` emits logs as JSON. `cluster` and `clusters`
are aliases on `get`, `create`, `patch` and `delete`.

## Authenticating to CWE

`nvkectl config set-site` command configures the management-cluster kubeconfig with the site.
nvkectl commands use it to mint an audience-scoped token for the admitted
identity `nke-clients/nke-client`. The audience comes from the authentication
ConfigMap, looked up in namespace `nvke-controller` first, then `nke-controller`
for a site installed before the `nvke` rename, then `cloud-workflow-engine`,
and under two names in each: `nvke-auth-config`, then
`nke-auth-config` for a site that keeps the older name by pinning
`apiAuth.configMap.name`.

The first *readable* candidate is used: a site whose RBAC grants `get` on its
pinned name alone answers Forbidden for the other name, and the search moves
past that denial rather than stopping at it.

Only when neither name is found in either namespace does the CLI treat
authentication as off and send no `Authorization` header — the same as a CWE
that was installed with `apiAuth.enabled` unset. A denial is not that: it is
kept, and reported only if no candidate turns out to be readable, so an RBAC
problem never arrives as an unexplained 401.

The kubeconfig identity needs:

- `create` on `serviceaccounts/token` for `nke-client` in `nke-clients`
- `get` on `secrets` in the site's management namespace (`nvke-system`, or
  `nke-system` on a site installed before it was configurable), for
  `nvkectl get kubeconfig`, which reads the `<cluster>-admin-kubeconfig` Secret
  there directly rather than through CWE. The command tries both and takes
  `--namespace` for anything else
- `list` on `customresourcedefinitions`, `list` on `deployments` across
  namespaces, and `get` on `secrets` in each provider manager's namespace
  (`capnico-system` and `capll-system` by default), for `nvkectl get providers`
- `list` on `nicoidentities.infrastructure.cluster.x-k8s.io` in the namespace of
  CAPNICo's credentials Secret (`capnico-system` by default), for the
  `CREDENTIALS` column of `nvkectl get providers`. CAPNICo's
  `nicoidentity-viewer-role` grants it without access to the Secret

A cluster-admin kubeconfig already has all of these. The token grant, and
creating the service account on a real site, are in
[Configuring NVKE API Authentication](../controller/docs/configuring-api-authentication.md).

The machine image and release commands do not use CWE or a token. Their
kubeconfig identity needs `get`, `list`, `create` and `delete` on
`machineimages`, `clusterreleases` and `nodepoolreleases` in
`internal.nvke.nvidia.com`, plus `patch` for `nvkectl deprecate`.

## Infrastructure providers

`nvkectl get providers` reports which Cluster API infrastructure providers a site
controller can actually use. It reads the site's Kubernetes cluster directly
rather than the CWE API, so it needs the kubeconfig registered with
`nvkectl config set-site --kubeconfig` and `kubectl` on `PATH`.

| State | Meaning |
|---|---|
| `NOT-INSTALLED` | The provider's CRDs are absent, or present but not established. Not an error — a site may legitimately run one provider, both, or neither. |
| `NOT-CONFIGURED` | CRDs are established but no credentials Secret was found. For LaunchLayer this is a supported configuration: without one its client is unauthenticated. |
| `DEGRADED` | Configured, but the provider's manager is not available or was not found. |
| `CONFIGURED` | CRDs established, manager available, credentials in place. This does **not** mean the credentials work — nothing here authenticates against the provider's backend. |
| `UNKNOWN` | A probe failed for a reason other than absence, most often a kubeconfig that may not read Secrets in the manager's namespace. |

`CREDENTIALS` reports whether the credentials work. CAPNICo checks its own
credentials every few minutes and publishes the result on a `NicoIdentity` that
names the credentials Secret; nvkectl reads that result and never runs a check.
`LAST CHECK` is how long ago the latest check finished. The column reads the
Kubernetes API directly, not through `kubectl`.

| Credentials | Meaning |
|---|---|
| `HEALTHY` | The last check authenticated and read the current tenant, within the last 10 minutes. It does not prove that provisioning will succeed. |
| `FAILING (reason)` | The last check found the Secret missing (`CredentialsNotFound`) or malformed (`InvalidConfiguration`), or the backend rejected (`AuthenticationFailed`) or forbade (`AccessDenied`) the credentials. |
| `UNKNOWN (reason)` | The last check could not decide: the Secret could not be read (`SecretReadFailed`), or the backend timed out, was unreachable, or failed TLS verification (`ValidationFailed`). `UNKNOWN` with no reason means the check time is in the future or malformed. |
| `STALE` | No check has finished in the last 10 minutes. The provider's manager may be stopped, or the `NicoIdentity` is outside the namespace or `--watch-filter` the manager watches. `DETAIL` names the likely cause. |
| `PENDING` | The `NicoIdentity`, or its latest spec change, has not been checked yet. |
| `NO-IDENTITY` | No `NicoIdentity` in the Secret's namespace names the credentials Secret, so nothing checks it. |
| `UNSUPPORTED` | The installed CAPNICo does not serve `NicoIdentity`. |
| `UNREADABLE` | nvkectl could not list `NicoIdentity` objects, most often because the kubeconfig lacks the permission listed above. |
| `-` | The provider publishes no credential health, or is not installed. |

To observe credential health on a site that reads `NO-IDENTITY`, create a
`NicoIdentity` that names the credentials Secret CAPNICo uses:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: NicoIdentity
metadata:
  name: nico-default
  namespace: capnico-system
spec:
  credentialsRef:
    name: nico-credentials
```

`-o json` adds a `credentials` object to each provider that publishes credential
health, with the health, the `NicoIdentity` it was read from, its `Ready`
condition, `lastCheckedTime`, and `staleAfterSeconds`.

The providers listed are the ones nvkectl knows how to describe. That is not a
restriction: CWE accepts any infrastructure manifest written by hand.

## Configuration

Which nodes may attest describes the *site*, so it comes from the site spec
file. See [`docs/sitespec.md`](docs/sitespec.md).

| Field | |
|---|---|
| `cluster.*.tpmEkThumbprint` | Which nodes may attest to SPIRE. No node listed, no node admitted; a site spec with none deploys no TPM attestor at all. `provisioned` only: with `nico` the attestor is always on and CAPNICo supplies each machine's key. |
| `infra.provider`, `infra.nico` | `nico` allocates the control-plane machines from a NICo site instead of listing hosts. The NICo here is for bootstrap only; see [`docs/sitespec.md`](docs/sitespec.md#where-the-machines-come-from). |
| `spire.upstreamCA` | `true` when SPIRE chains to a CA you bring, from `NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE`. Every run then requires the two variables. |

### What gets installed

`bootstrap site` deploys a SPIRE server, so nodes with a hardware identity have
something to attest to and CWE gets a workload identity of its own. It comes
from the SPIFFE project's own hardened charts. The bootstrap manifest pins each
chart's version, and `nvkectl get bootstrap-manifest` prints it. Each chart is
installed with `helm --repo`, so nothing is added to your global Helm
repository list. Everything it names is public, so a bootstrap needs no
registry credential.

Everything static about the deployment lives in
[`internal/bootstrap/controllerdeploy/manifests/spire-values.yaml`](internal/bootstrap/controllerdeploy/manifests/spire-values.yaml).
`nvkectl` sets only what it cannot know ahead of time: the trust domain and
cluster name, CWE's admin identity, the CA to chain to, and the TPM thumbprints
allowed to attest, or, with `infra.provider: nico`, the `tpm-pubhashes`
ConfigMap CWE fills as the attestor's allowlist (see
[SPIRE with NICo nodes](docs/sitespec.md#spire-with-nico-nodes)).

Two of the values it sets have to agree with CWE and are wrong by default on
both sides, so both halves are set from one constant in
[`internal/bootstrap/controllerdeploy/controllerdeploy.go`](internal/bootstrap/controllerdeploy/controllerdeploy.go):

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

When any node in the site spec carries a TPM thumbprint, or the provider
is `nico`, the SPIRE server is also published on host port 30081 of the machine
running the bootstrap. With `provisioned`, each node's `spire-agent` dials the
SPIRE address it was given when the node was provisioned, so that address has to
be a floating VIP the operator points at the bootstrap machine for the duration
of the run; see [Your First Cluster](../docs/first-cluster.md#real-site). With
`nico`, the nodes dial `spire.externalAddress`, which the controller passes to
the user data template of the `ClusterRelease` they boot; that template has to
write it (see [SPIRE with NICo nodes](docs/sitespec.md#spire-with-nico-nodes)). With `provisioned` and no thumbprints, or with
`--disable-mtls`, nothing is published.

| Variable | Default | Purpose |
|---|---|---|
| `NVKECTL_SPIFFE_TRUST_DOMAIN` | `<cluster>.nke.local` | Trust domain for the identities issued; the controller stack install defaults to the same value for the default cluster name |
| `NVKECTL_SPIRE_EXTERNAL_ADDRESS` | unset | Overrides `spire.externalAddress`: the address nodes created by CAPNICo use to reach the bootstrap SPIRE server on NodePort 30081. Required with `infra.provider: nico` unless `--disable-mtls`, and rejected with `provisioned`. |
| `NVKECTL_SPIRE_CA_CERT_FILE` | unset | Certificate of a CA for SPIRE to chain its own to, so a node given that certificate when it was provisioned verifies the server on first contact. Set with `NVKECTL_SPIRE_CA_KEY_FILE` and `spire.upstreamCA: true` in the site spec; without them, SPIRE is self-signed. |
| `NVKECTL_SPIRE_CA_KEY_FILE` | unset | Private key of that CA. Set with `NVKECTL_SPIRE_CA_CERT_FILE`. |

The cluster name SPIRE is told is the ephemeral cluster's own name, from
`NVKECTL_BOOTSTRAP_CLUSTER_NAME` — there is no separate SPIFFE variable for it,
because the bootstrap cluster has one name and two ways to say it could only
ever disagree.

When SPIRE is enabled, `bootstrap site` exports the live `ClusterStaticEntry`
CRD definition and CWE's cluster-scoped workload registration policy before it
removes the bootstrap cluster. It writes `clusterstaticentry-crd.yaml` and
`clusterstaticentries.yaml` in the same directory as the site kubeconfig. The
files contain no source UIDs, resource versions, status, or other server-owned
metadata.

### Host prerequisites

The machine running `bootstrap site` needs `nerdctl` with a containerd for it to
talk to — rootful, run as root; Docker is not supported — plus `kind`, `helm`,
`kubectl` and `clusterctl` on `PATH`. Only nerdctl is checked before the run
starts. A missing `kind` fails in the pre-flight check for a leftover bootstrap
cluster, before anything is created; a missing `helm`, `kubectl` or `clusterctl`
surfaces mid-bootstrap, after the Kind cluster is already up. kind is driven
through `KIND_EXPERIMENTAL_PROVIDER=nerdctl`, which `nvkectl` sets itself.

### Bootstrap cluster and CWE

| Variable | Default | Purpose |
|---|---|---|
| `NVKECTL_BOOTSTRAP_CLUSTER_NAME` | `nvkectl-bootstrap` | Name of the ephemeral cluster |
| `NVKECTL_BOOTSTRAP_NODE_IMAGE` | kind's default | Node image for the ephemeral cluster |
| `NVKECTL_BOOTSTRAP_WAIT_READY` | `3m` | Bounds `kind create cluster --wait` |
| `NVKECTL_BOOTSTRAP_DOCKERHUB_MIRROR` | unset | Registry mirror the ephemeral cluster pulls Docker Hub images through |
| `NVKECTL_INFRA_PROVIDER` | `provisioned` | Overrides the site spec's `infra.provider`: `provisioned` takes the control-plane hosts from `cluster.nodes`, `nico` has CAPNICo allocate them from `infra.nico`. A value left exported in the shell switches every run. |
| `NVKECTL_NICO_CREDENTIALS` | unset | Path to the file holding the NICo API token, read on the bootstrap host. Required with `infra.provider: nico` by `bootstrap site` and `validate sitespec --bootstrap-only`; the token is not in the site spec. A bare token is supported; a file with `tokenURL`, `clientID` and `clientSecret` is recognised and refused, because CAPNICo's Secret takes only a bearer token. |
| `NVKECTL_NODE_USER`, `NVKECTL_NODE_PASSWORD` | unset | The login rendered into the cloud-init of every node CAPNICo creates. Required with `infra.provider: nico` by `bootstrap site` and `validate sitespec --bootstrap-only`; the stack install reads the same variables. |
| `NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT` | unset | Set to `1` to let `infra.nico.remote.endpoint` be an http URL. The bootstrap sends the NICo token there, so without it `bootstrap site` and `validate sitespec --bootstrap-only` require https; it is for a local or simulated NICo. |
| `NVKECTL_BOOTSTRAP_CLUSTER_TIMEOUT` | `30m` | How long to wait for the site cluster to finish provisioning (`--timeout`) |
| `NVKECTL_NVKE_CONTROLLER_IMAGE` | `nke-controller:latest` | Controller image to deploy. A reference without a registry is side-loaded from the local image store; `make -C cli images` builds the default with nerdctl. A reference with a registry host is pulled with `nerdctl pull` when it is not already in containerd's image store, then loaded into the ephemeral cluster; log in to the registry first (`nerdctl login`). For Docker Hub, write `docker.io/<org>/<name>`. `--nvke-controller-image` overrides it for one run. |
| `NVKECTL_NVKE_CONTROLLER_PROFILES_IMAGE` | unset | OCI content image whose `/profiles` tree is copied into the controller's profiles directory, which is otherwise empty. It needs a tag other than `latest`, or a digest; `bootstrap site` refuses one that does not, including with `--dry-run`. An image with no registry host is side-loaded from the local image store, so it must already be there; one with a registry host is pulled. The image must provide `/profiles` and `/bin/sh`: the chart's init container runs `cp -a /profiles/. /profiles-out/` through the shell. |
| `NVKECTL_CAPI_PROVISIONED_IMAGE` | `cluster-api-provider-provisioned:latest` | Provisioned-provider image, same semantics; unused with `infra.provider: nico`. `--provisioned-provider-image` overrides it for one run. |
| `NVKECTL_NVKE_CONTROLLER_PORT` | `30080` | Port of the controller API: its Service, NodePort, Kind host mapping and the address `nvkectl` calls |
| `NVKECTL_NVKE_CONTROLLER_TEMPORAL_CHART` | `temporal` from `https://go.temporal.io/helm-charts` | Temporal chart, as an OCI reference or a local path. Set, it replaces the repo lookup. |
| `NVKECTL_NVKE_CONTROLLER_SPIRE_CHART` | `spire` from `https://spiffe.github.io/helm-charts-hardened/` | SPIRE chart, same semantics |
| `NVKECTL_NVKE_CONTROLLER_SPIRE_CRDS_CHART` | `spire-crds` from `https://spiffe.github.io/helm-charts-hardened/` | SPIRE CRDs chart, same semantics |
| `NVKECTL_NVKE_CONTROLLER_HELM_TIMEOUT` | `5m` | Bounds each `helm upgrade --install --wait` |
| `NVKECTL_NVKE_CONTROLLER_READY_TIMEOUT` | `5m` | Bounds the wait for the controller's `/readyz` |
| `NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR` | `cluster-diagnostics` | Where a failed bootstrap writes a cluster dump before tearing the cluster down (`--diagnostics-dir`) |

Every `NVKECTL_*` variable above except `NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT`,
which turns a safety check off and so answers only to its own name, and the
`NVKECTL_NVKE_CONTROLLER_*` settings, was named
`NKECTL_*` while the binary was called `nkectl`. The old names still work for one release: when only the old
name is set, `nvkectl` uses it and logs a warning naming the replacement. When
both are set, the new name wins. Likewise
`make -C cli images` reads
`CONTROLLER_IMAGE`, and still reads `CWE_IMAGE` with a warning until
[nvke#1399](https://github.com/dsx-ai-factory/nvke/issues/1399) removes it.

The controller and Postgres charts are carried in the binary and are never
pulled, so they have no variable. For the three that are pulled, the chart
version stays pinned in `nvkectl` whichever source the chart comes from; helm
does not check a local path against the pin, so a tarball has to be the pinned
version. With those three variables set, no Helm chart is pulled from the
internet. The images those charts run still come from their
registries (`NVKECTL_BOOTSTRAP_DOCKERHUB_MIRROR` covers Docker Hub), and
`clusterctl init` still fetches Cluster API and cert-manager from `github.com`.

## When a bootstrap fails

`bootstrap site` removes the ephemeral Kind cluster on every exit path except
these: `--keep-bootstrap-cluster`; a failure after CWE has accepted the site
cluster, or with `nico` a create request that got no answer, so CWE may have
accepted it, when the cluster is retained for recovery and the error says what to do
next (the `KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster` command, preceded
with `nico`, for a failure before every control-plane node joined, by the steps that
release the NICo instances CAPNICo allocated, and, after they joined, by saying to
delete only the Kind cluster, because the site cluster is working; or, when the etcd
encryption key was not saved, the command that saves it first);
and a `kind create cluster` that succeeded but whose kubeconfig export or mirror
configuration failed, which is left for you to delete. In the ordinary failure
case, then, by the time the error reaches you there is nothing left to ask. To
keep the evidence, a failed run collects a `kubectl cluster-info dump` — every
namespace's objects and pod logs — plus the events, object state and helm
releases that explain a pod which never started, and writes it out *before* the
teardown.

It lands in `cluster-diagnostics/`, overridable with `--diagnostics-dir` or
`NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR`. A successful run writes nothing. CI uploads
the directory as an artifact when the bootstrap E2E fails.

## Build from source

```bash
# From monorepo root
make build          # builds all binaries including cli/nvkectl into bin/

# Or from this directory
make build
```

## Compatibility

`nvkectl` releases on its own `cli/vX.Y.Z` tags and follows
[the compatibility policy](docs/compatibility.md): what is contract and what is
not, the exit codes, signal and non-interactive behaviour, and how something
gets deprecated before it is removed.

The short version, for scripting:

| | |
|---|---|
| Exit `0` | Success |
| Exit `1` | Failure — the reason is on stderr |
| Exit `130` | Interrupted by `SIGINT` before finishing |
| Exit `143` | Interrupted by `SIGTERM` before finishing |
| stdout | Results only |
| stderr | Diagnostics, progress and logs only |

Parse `-o json`, not the table output: columns are presentation and may change
in a patch release. Match on the exit code, not on error text.

## Platform support

| OS | amd64 | arm64 |
|---|---|---|
| Linux | ✓ | ✓ |
| macOS | ✓ | ✓ |

## Related docs

- [Command reference](docs/reference/README.md) — every public command and flag
- [Compatibility and deprecation policy](docs/compatibility.md)
- [Management cluster bootstrap](../docs/site-bootstrap/overview.md)
- [Architecture](../docs/architecture.md)
- [Authenticating to the NVKE API](../controller/docs/authenticating-to-the-api.md)
- [Configuring NVKE API Authentication](../controller/docs/configuring-api-authentication.md)
