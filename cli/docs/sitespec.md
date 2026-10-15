# nvkectl bootstrap site — site spec file spec

`nvkectl bootstrap site -f <sitespec-file>` reads one YAML site spec file. The
file is the single source of truth for a bootstrap run: it lists the
pre-provisioned nodes and the vanilla Kubernetes cluster to build on them.

The command loads the file, applies defaults, and validates every field before
it dispatches any work. Any invalid or missing field fails fast with a clear
error and no node is touched.

A ready-to-copy example is in [`examples/sitespec.yaml`](../examples/sitespec.yaml).

## Generating one

`nvkectl generate sitespec` writes a complete site spec, so the file does not
have to be assembled by hand:

```bash
nvkectl generate sitespec my-tier0-site --nodes hosts.csv > sitespec.yaml
```

The hosts come from a CSV, because a real site has more of them than a flag per
machine can carry. The header names the columns in any order; `name` and `ip`
are required, `tpm_ek_thumbprint` is optional per row:

```csv
name,ip,tpm_ek_thumbprint
cp-01,10.10.50.2,0000000000000000000000000000000000000000000000000000000000000000
cp-02,10.10.50.3,
cp-03,10.10.50.4,
```

An unknown column is an error rather than being ignored, so a misspelled header
does not silently drop a field. Pass `--nodes -` to read the CSV from standard
input.

This is a different format from the `NVKECTL_CLUSTER_NODES` environment variable
below, which spells the same list as `name:ip,name:ip`. The two are not
interchangeable: `name:ip` has nowhere to put a TPM thumbprint.

Anything the CSV does not carry is set with `--set`, using the same field paths
this document uses. Run it with an unknown path to list what it accepts:

```bash
nvkectl generate sitespec my-tier0-site --nodes hosts.csv \
  --set cluster.controlPlaneVIP=10.10.50.10 \
  --set cluster.podCidr=10.244.0.0/16 > sitespec.yaml
```

Four fields are deliberately not settable. `cluster.nodes` comes from `--nodes`.
`registry.apiKey` is a credential and this file is meant to be committed — pass
it to `nvkectl bootstrap site` with `--registry-api-key` or `REGISTRY_API_KEY`
instead. `infra.provider` and `spire.externalAddress` belong to a nico site spec,
which `generate sitespec` does not write: it writes a `provisioned` one from
`--nodes`, so write a nico file by hand from `examples/sitespec-nico.yaml`.

Unlike `bootstrap site`, `generate sitespec` ignores the environment variables
listed below, so a generated file never carries a value nobody typed. Every
field is validated before anything is written, so a file this command produces
is a file `bootstrap site` accepts.

## Where the machines come from

By default the file lists the hosts (`cluster.nodes`) and the bootstrap builds
the cluster on them. Setting `infra.provider: nico` swaps that list for
`infra.nico`: CAPNICo allocates the same number of control-plane machines from
a NICo site, configured by the profiles the block names. Everything else in the
file means what it did. [`examples/sitespec-nico.yaml`](../examples/sitespec-nico.yaml)
is a complete example.

**This NICo is for bootstrap only.** It is the NICo that allocates the site
controller's own control-plane machines, through a CAPNICo installed in the
throwaway Kind cluster. It is not the NICo the finished site controller uses to
create tenant clusters; that is `controllerStack.nico` below and may be a
different NICo, with different credentials, org and site. A site spec can carry
both.

The Kind cluster holds the CAPNICo, the `nico-credentials` Secret (the NICo
token) and the `dgxc-tenant-credentials` Secret (the tenant password). It is
deleted when the bootstrap ends, except in three cases: when you pass
`--keep-bootstrap-cluster`, when the run fails after the site cluster was
created, and when the create request got no answer, so the controller may have
created it. In each, the cluster and those Secrets stay on the bootstrap host until
you delete the cluster with
`KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nvkectl-bootstrap`
(the failure message prints the command). Treat the host as holding the tokens
until then. After a failure before every control-plane node joined, release the
NICo instances CAPNICo allocated before you delete the cluster, as the failure
message and
[the retained bootstrap cluster](bootstrap-troubleshooting.md#release-the-nico-instances-first)
describe: deleting the Kind cluster alone leaves them allocated and unmanaged.
After every control-plane node joined the site cluster is working and its NICo
machines belong to it, so release nothing: recover the kubeconfig and key, then
delete only the Kind cluster.

`infra.nico.controlPlane` is built from the struct the controller's REST API
takes for a NICo control plane (`api.NicoControlPlaneProviderCreate`), with the
API's field names, plus `count`. The request's `nico` block
(`api.NicoClusterProvider`) is assembled from `orgID` and the `remote` IDs. The
NICo is described by one mode block, `remote`, an existing NICo; another mode
would be a sibling of it, so nothing here changes when one is added.

| Field | Type | Required | Notes |
|---|---|---|---|
| `infra.provider` | string | no | `provisioned` (default) or `nico`. `NVKECTL_INFRA_PROVIDER` overrides it. |
| `infra.nico.orgID` | string | with `nico` | The NICo organization the machines are created in. |
| `infra.nico.remote.endpoint` | string | with `nico` | The existing NICo's API base URL. The bootstrap sends the NICo token to it, so `bootstrap site` and `validate sitespec --bootstrap-only` require https unless `NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT=1` is set, for a local or simulated NICo. |
| `infra.nico.remote.siteID`, `vpcID` | UUID | with `nico` | The NICo site and the VPC the cluster is created in. |
| `infra.nico.controlPlane.count` | int | with `nico` | Odd, at least 1. Takes the place of the length of `cluster.nodes`. |
| `infra.nico.controlPlane.instanceTypePreference` | UUID list | with `nico` | Ordered; index 0 is the first choice. |
| `infra.nico.controlPlane.vpcPrefixIDs` / `subnetIDs` | UUID list | with `nico` | Exactly one of the two: native networking or not. |
| `infra.nico.controlPlane.sshKeyGroupIDs` | UUID list | with `nico` | At least one. |
| `infra.nico.controlPlane.release` | string | with `nico` | The `ClusterRelease` the nodes boot from, which `infra.nico.releases` must hold. See Release manifests. |
| `infra.nico.controlPlane.configProfile` | string | no | A config profile name. The bootstrap controller starts with an empty profiles directory and serves none, so it treats any name as missing and logs a warning: its hooks and labels never reach the nodes. Leave it unset. |
| `infra.nico.tenant` | `StackTenant` | with `nico` | The login the controller renders into the user data of every node CAPNICo creates: `user` and `passwordFile`. The controller refuses to render user data without it. See below. |
| `infra.nico.releases` | path | with `nico` | A file, or a directory of `.yaml`, `.yml` and `.json` files, holding the `MachineImage` and `ClusterRelease` manifests. See below. |
| `spire.externalAddress` | string | with `nico`, unless `--disable-mtls` | IP address or DNS name nodes use to reach the bootstrap host's SPIRE server, on NodePort 30081. See below. |

The NICo token is not a field: see NICo credentials below.

`cluster.nodes` must be empty with `nico`, and `infra.nico` must be absent
with `provisioned`; a file that names both is rejected rather than half-obeyed.

With `nico` the nodes' names and addresses are results, not inputs. The
bootstrap does not print them; read them from the finished cluster with
`kubectl get nodes -o wide`.

### SPIRE with NICo nodes

Nodes are attested by TPM, as on a site of listed hosts, but the allowlist is
not in the file. A host's endorsement key cannot be known before the machine
exists, so there is no per-node `tpmEkThumbprint` to write. CAPNICo reports each
machine's key when NICo allocates it, and CWE adds it to the allowlist the
bootstrap SPIRE server reads (the `tpm-pubhashes` ConfigMap in `spire-server`).
A node attests once that entry has synced into the SPIRE pod, which can take up
to about a minute; its agent retries until then.

CWE passes the SPIRE server's address to each node's user data template, so it
has to be one a node can reach before any cluster exists. Set
`spire.externalAddress` to the address of the host running the bootstrap, as
seen from the network the nodes are created on. The bootstrap publishes the
server there on NodePort 30081.

The values reach a node only if the `ClusterRelease` in `infra.nico.releases`
writes them: its `userDataTemplate` must carry the SPIRE `write_files` block in
[What NVKE puts in the user data](../../docs/operate/build-node-images.md#what-nvke-puts-in-the-user-data),
which `examples/clusterrelease.yaml` does not. Without it the nodes never attest
and the bootstrap waits until `--timeout`. That page describes the values the
finished site controller fills in. The bootstrap controller fills in others:

| Template value | From the bootstrap |
|---|---|
| `{{ .SpireServerAddress }}` | `spire.externalAddress` |
| `{{ .SpireServerPort }}` | `30081` |
| `{{ .TrustDomain }}`, `{{ .TrustBundle }}` | The bootstrap SPIRE server's trust domain and bundle |
| `{{ .Username }}`, `{{ .HashedPassword }}` | `infra.nico.tenant.user` and the password in `infra.nico.tenant.passwordFile` |
`nvkectl bootstrap site` requires it with `nico` unless `--disable-mtls` is
passed, since nothing reads it without SPIRE. `--dry-run` applies the same check.
Setting `spire.externalAddress` with `provisioned` is rejected: those hosts were
given their SPIRE address when they were provisioned.

### NICo credentials

The token is not in the site spec. `NVKECTL_NICO_CREDENTIALS` names a file that
holds it, read on the host that runs the bootstrap, relative to the working
directory. `bootstrap site` and `validate sitespec --bootstrap-only` require it
with `nico`; a full `validate sitespec` reports it without failing on it, and it
is not read by commands that never run the bootstrap, such as
`render deploy-path`. The bootstrap creates the `nico-credentials` Secret in the
Kind cluster from it, with `infra.nico.remote.endpoint` and `infra.nico.orgID`
from the spec and the API name `forge`.

The file's format is told from its content. A bare token, one value with no
whitespace, is the bearer token. A file that carries `tokenURL`, `clientID` or
`clientSecret` is the OAuth client-credentials form, which is recognised and
refused for now, because CAPNICo's `nico-credentials` Secret takes only a bearer
token. The Secret on the site cluster is not created by the bootstrap: the
controller stack install creates it, from `controllerStack.nico`.

The bootstrap installs CAPNICo into its Kind cluster the way the site
controller's deploy path does: the published release manifest
(`deploy/nico/capnico/capnico.yaml`), with the `nico-credentials` Secret in
`capnico-system`. No registry credential is created: the image is public and the
manifest names no pull secret. It is the same file at the
same pin, embedded in `nvkectl` by `make -C cli capnico-manifest`, so a CAPNICo
bump in the deploy path is a bump here. It is not the Helm chart: the manifest
carries the reboot and repair annotation prefixes the controller writes, which
the chart does not, and needs only `kubectl`.

By default the image is the digest that `bootstrap-manifest.yaml` lists for the
tag the manifest names (`nvkectl get bootstrap-manifest`), so a retagged image
does not change what runs. Pass `--release <file>` to
pin it to an NVKR release's `capnico-controller` artifact instead, as
`nvkectl render deploy-path --release` does for the site controller's CAPNICo:
the manifest's one image is replaced with the artifact's `repository@digest`.
The release description is not verified, here or there: run `nvkr release
verify` on the same reference first.
`--arch` (default the bootstrap host's) picks the artifact's architecture, since
the bootstrap CAPNICo runs in the Kind cluster on that host. It is refused
without `--release`, whatever the provider, and `--release` is itself refused
with the `provisioned` provider, where no CAPNICo is installed. The Kind node pulls
the image anonymously, so the release's `capnico-controller` image must be
publicly pullable; staging images from a release run directory, as the deploy path does for the
node, is not supported here. The Kind host must reach `ghcr.io`
and the NICo API and each new node's agent port, and the nodes must reach the
SPIRE server the bootstrap publishes on the host. After every node joins, the
bootstrap calls the site's Kubernetes API to write the kubeconfig, so the host
must also reach the control-plane endpoint, `cluster.controlPlaneVIP` or the
node's address, on port 6443.

`infra.nico.tenant` is the same struct as `controllerStack.tenant`. The bootstrap
puts it in a `dgxc-tenant-credentials` Secret in the controller's namespace and
points the controller chart at it (`secrets.dgxcTenantCredSecretName`); without
it the controller fails every node's user-data rendering and no node is created.
`passwordFile` is a path to a file holding the password, like the tokens.

### Release manifests

With `nico` the controller resolves `infra.nico.controlPlane.release` from
its own API: a `ClusterRelease` of that name and the `MachineImage` it names.
The bootstrap controller starts with neither, so `infra.nico.releases` points at
the manifests, written as in
[Get NVKE artifacts from NVKR and write release manifests](../../docs/operate/build-node-images.md),
and the bootstrap applies them to the Kind cluster after the controller's CRDs
exist and before it creates the site cluster. `bootstrap site` and
`nvkectl validate sitespec --bootstrap-only` fail, before any Kind cluster exists,
if the files cannot be read or hold no `ClusterRelease` named for the `release` or not the
`MachineImage` it references. A relative `infra.nico.releases` resolves from
the site spec's directory. Loading the site spec for any other command, such as
`render deploy-path` on the site node, does not read the manifests.

Each document must be a full manifest, with
`apiVersion: internal.nvke.nvidia.com/v1` and its `kind`; a document missing
either is refused, because `kubectl apply` cannot apply it. The request
documents that page has you edit in `releases/` carry neither, because
`nvkectl create` adds them when it publishes them, so they do not work here as
they are. From the directory that holds `releases/`, have `nvkectl create` write
the full manifests into a directory of their own, and point
`infra.nico.releases` at that one:

```bash
mkdir bootstrap-releases
nvkectl create machineimage -f releases/machineimage.yaml --dry-run=client -o yaml > bootstrap-releases/machineimage.yaml
nvkectl create clusterrelease -f releases/clusterrelease.yaml --dry-run=client -o yaml > bootstrap-releases/clusterrelease.yaml
```

Keep the two directories apart. `releases/` keeps the request documents that
`nvkectl create` publishes to the finished site controller, and the bootstrap
applies every file in `infra.nico.releases`.

## Schema

| Field | Type | Required | Notes |
|---|---|---|---|
| `clusterName` | string | yes | Lowercase RFC 1123 label. |
| `cluster.nodes` | list | yes, with `provisioned` | Must contain an odd number of control-plane nodes and at least one node. With `nico` it must be empty and `infra.nico.controlPlane.count` sets the count; see [Where the machines come from](#where-the-machines-come-from). |
| `cluster.nodes[].name` | string | yes, per entry | Lowercase RFC 1123 label. Unique across all nodes. |
| `cluster.nodes[].ip` | string | yes, per entry | Valid IPv4 or IPv6 address. Unique across all nodes. |
| `cluster.nodes[].tpmEkThumbprint` | string | no | SHA-256 of the node's TPM endorsement key: 64 lowercase hex digits. See below. |
| `cluster.k8sVersion` | string | yes | Plain semantic version, e.g. `1.32.0` (no leading `v`). |
| `cluster.cniName` | string | no | Defaults to and must be `ovn-kubernetes`. |
| `cluster.cniVersion` | string | no | CNI plugin version. Empty uses the node-agent default. |
| `cluster.controlPlaneVIP` | string | for multiple nodes | IPv4 control-plane VIP. Required when `cluster.nodes` has more than one entry, or `infra.nico.controlPlane.count` is more than one. A single-node cluster may omit it and use that node's IP as its fixed endpoint, except with `nico`: the node has no address until NICo allocates it, so the controller stack install needs the VIP even for one node (the bootstrap does not, and warns). Set it before the bootstrap: it cannot be added once the site cluster exists. `validate sitespec` reports it missing in the `controllerStack` section. See below. |
| `cluster.podCidr` | string | yes | Valid CIDR. A comma-separated list of CIDRs is allowed. Must not overlap `serviceCidr`. |
| `cluster.serviceCidr` | string | yes | Valid CIDR. |
| `cluster.nodeAgentPort` | string | no | Port number 1–65535. Defaults to `15005`. |
| `cluster.etcdEncryption` | bool | no | Encrypt the Kubernetes API's etcd data at rest. Defaults to `true`. Decided at bootstrap only: a cluster built with it `false` cannot be encrypted afterwards. See below. |
| `cluster.kubeletConfig.systemReservedMemory` | string | no | e.g. `16Gi`. |
| `cluster.oidcProviders` | list | no | OIDC providers for site cluster. Cannot be modified afterwards. Ignored when absent. Rejected when `cluster.k8sVersion` is older than `1.34.0`. Each entry needs `name`, `issuerURL`, `audiences`, `usernameClaim`, `usernamePrefix`, and `caBundle` (inline PEM). Optional: `groupsClaim`, `groupsPrefix`, `requiredClaims`.|
| `registry.bootstrapRepo` | string | yes | OCI repository holding the bootstrap engine chart. The controller image is published separately, to `<registry>/<org>/controller`. |
| `registry.bootstrapChart` | string | yes | Bootstrap engine chart name within `bootstrapRepo`. |
| `registry.apiKey` | string | no | Registry credential. Never defaulted — see below. |
| `spire.trustDomain` | string | no | SPIFFE trust domain, shared by the bootstrap and the controller stack install. Set it once, before the bootstrap, or leave it unset: both then use `nvkectl-bootstrap.nke.local`, and the install says so. A bootstrap run with `NVKECTL_BOOTSTRAP_CLUSTER_NAME` set derives another, so set it then. |
| `spire.caCertFile` | string | no | Path to a CA certificate for SPIRE to chain its own CA to. See below. |
| `spire.caKeyFile` | string | no | Path to that CA's private key. Required with `caCertFile`. |
| `controllerStack` | section | for the stack install | Read by the controller stack install, not by `bootstrap site`. `generate sitespec` writes it with `REPLACE_*` placeholders; the install refuses a placeholder. See below. |
| `controllerStack.arch` | string | yes, in `controllerStack` | Node CPU architecture: `amd64` or `arm64`. |
| `controllerStack.nico.endpoint` | string | yes, in `controllerStack` | NICo API base URL CAPNICo calls, `http` or `https`. |
| `controllerStack.nico.orgID` | string | yes, in `controllerStack` | NICo organisation ID. |
| `controllerStack.nico.apiName` | string | yes, in `controllerStack` | NICo API name, for example `forge`. |
| `controllerStack.nico.tokenFile` | string | yes, in `controllerStack` | Path to a file holding the NICo API token. |
| `controllerStack.tenant.user` | string | yes, in `controllerStack` | Login user rendered into every tenant node's cloud-init. |
| `controllerStack.tenant.passwordFile` | string | yes, in `controllerStack` | Path to a file holding that user's password. |
| `controllerStack.images.controller` | string | no | Overrides the controller image; defaults to the release file's `controller` item, or without a release to the overlay's pinned image (the newest controller release). |
| `controllerStack.images.profiles` | string | no | Overrides the profiles content image; defaults to the release file's `profiles` item. |
| `controllerStack.database.passwordFile` | string | yes, in `controllerStack` | Path to a file holding the Postgres password. The render creates it with a generated password when it does not exist. |
| `controllerStack.mirror` | string | no | Registry the external images are pulled from instead of upstream, `host[:port][/prefix]`, no scheme. |
| `controllerStack.nktpValuesFile` | string | no | Values file for the nktp release, applied last. |

A site spec with a `controllerStack.pullSecret` block does not load; delete the block. The file has no unknown-field tolerance, and nothing reads it. CAPNICo's image is public on GHCR, or comes from the release.

The version (image/chart tag) is baked into the `nvkectl` build, so it is not
part of the site spec.

Unknown fields are rejected. A typo in a field name fails the load rather than
being silently ignored.

## The controllerStack section

The controller stack install reads `controllerStack:` after the bootstrap; `bootstrap
site` does not. `generate sitespec` writes the section with every value a
`REPLACE_*` placeholder, and `bootstrap site` ends by listing the placeholders
still in the file. Fill them in before installing the stack: the install
refuses a value that is still a placeholder.

`nvkectl validate sitespec -f sitespec.yaml` checks both halves of the file
and lists what is still a placeholder; `--bootstrap-only` checks only what
`bootstrap site` reads. A site spec field that is invalid fails either check.
Otherwise the exit status of the full check is the controller stack's, because
the install gates on it from a host that does not hold what only the bootstrap
host does: with `nico`, the token file, the tenant password
and release manifests, and `spire.externalAddress`. The full check reports
those; `--bootstrap-only` fails on them. A file whose bootstrap half is valid can bootstrap
the cluster before the `controllerStack` section is filled in; the output
says so.

The section is filled in by hand. `generate sitespec --set` reaches only the
bootstrap fields listed under "Environment variable overrides", not
`controllerStack.*`.

Tokens and passwords are paths to files that hold them, one secret per file,
relative paths resolved from the site spec, so the site spec holds no
credential and can be committed. The site name, the trust domain, the
control-plane VIP and the SPIRE CA are the fields above; the install reads
them from there and they are not repeated in `controllerStack:`.

## Validation rules

- All problems are reported together in one error, each naming the offending
  field, so a bad file can be fixed in one pass.
- The site spec must contain an odd number of nodes and at least one node: the
  length of `cluster.nodes` with `provisioned`, `infra.nico.controlPlane.count`
  with `nico`.
- Node names and IPs must be unique across all nodes (`provisioned` only; `nico`
  has none to list).
- `podCidr` / `serviceCidr` are parsed as CIDRs; `podCidr` also accepts a
  comma-separated list and validates each entry. No `podCidr` entry may
  overlap `serviceCidr`.
- `controlPlaneVIP`, when set, must parse as a routable unicast IPv4 address,
  and must not fall inside `podCidr` or `serviceCidr`. It may equal a declared
  node IP.
- `controlPlaneVIP` is required when `cluster.nodes` contains more than one
  node, or `infra.nico.controlPlane.count` is more than one. With `nico` it is
  needed for one node too: the bootstrap runs without it, but the controller
  stack install is refused, and the VIP cannot be added once the site cluster
  exists, because kube-vip is set up only when the cluster is created with one.
  `bootstrap site` and `validate sitespec` warn about it.
- With `nico`: `infra.nico` (its `orgID`, `remote`, `controlPlane`, `tenant` and
  `releases`) is required, and the files it names, and the file
  `NVKECTL_NICO_CREDENTIALS` names, must exist when the bootstrap runs, and
  the tenant password file must hold a password (`bootstrap site` and
  `validate sitespec --bootstrap-only` check; a full `validate sitespec`
  reports them; `render deploy-path` does not); with `provisioned` `infra.nico` is rejected. `spire.externalAddress` is required with
  `nico` unless `--disable-mtls`, which `bootstrap site` and
  `validate sitespec --bootstrap-only` both check. Pass `--disable-mtls` to
  `validate sitespec` as you will to `bootstrap site`, and the file is checked
  as that run would: `spire.externalAddress` is not required.

CLI acceptance does not prove bootstrap or network reachability. The controller
chooses the first control-plane node ready to join, which need not be the first
node in the site spec or the node whose IP matches the VIP. It configures
kube-vip before `kubeadm init` and uses the VIP as the control-plane endpoint.
If the VIP belongs to another node, routing must reach the initializing API
server rather than that node's not-yet-running API server.

The agent and the legacy controller runbook try to remove the VIP as a `/32`
from non-loopback interfaces before configuring kube-vip on loopback. When the
VIP equals a node IP, that cleanup can remove the node address if it has a
`/32` prefix. BGP source selection and API health checks still use the node IP.
Verify address ownership, bootstrap reachability, and BGP routing on the target
network.

## Environment variable overrides

Any field can be supplied — or overridden — through an environment variable, so
existing tier-0 / DS9 scripts can drive the site spec without editing the file.
A set variable takes precedence over the file value.

| Environment variable | Field |
|---|---|
| `CLUSTER_NAME` | `clusterName` |
| `CLUSTER_K8S_VERSION` | `cluster.k8sVersion` |
| `CLUSTER_CNI_NAME` | `cluster.cniName` |
| `CLUSTER_CNI_VERSION` | `cluster.cniVersion` |
| `CLUSTER_CONTROL_PLANE_VIP` | `cluster.controlPlaneVIP` |
| `CLUSTER_POD_CIDR` | `cluster.podCidr` |
| `CLUSTER_SERVICE_CIDR` | `cluster.serviceCidr` |
| `CLUSTER_SYSTEM_RESERVED_MEMORY` | `cluster.kubeletConfig.systemReservedMemory` |
| `CLUSTER_NODE_AGENT_PORT` | `cluster.nodeAgentPort` |
| `CLUSTER_ETCD_ENCRYPTION` | `cluster.etcdEncryption` |
| `REGISTRY_BOOTSTRAP_REPO` | `registry.bootstrapRepo` |
| `REGISTRY_BOOTSTRAP_CHART` | `registry.bootstrapChart` |
| `REGISTRY_API_KEY` | `registry.apiKey` |
| `NVKECTL_SPIFFE_TRUST_DOMAIN` | `spire.trustDomain` |
| `SPIRE_CA_CERT_FILE` | `spire.caCertFile` |
| `SPIRE_CA_KEY_FILE` | `spire.caKeyFile` |
| `NVKECTL_SPIRE_EXTERNAL_ADDRESS` | `spire.externalAddress` |
| `NVKECTL_INFRA_PROVIDER` | `infra.provider` — `provisioned` or `nico` |
| `NVKECTL_CLUSTER_NODES` | `cluster.nodes` — format `name:ip,name:ip`, applied only when the file lists no nodes |

A malformed `NVKECTL_CLUSTER_NODES` entry fails the load rather than being
silently discarded.

`INIT_NODE_IP` and `INIT_NODE_NAME` are not supported. The site spec does not
select a node for `kubeadm init`; the bootstrap process selects it downstream.

## TPM endorsement-key thumbprints

This section is for `provisioned` sites, where the nodes are listed.
`tpmEkThumbprint` is the SHA-256 of a node's TPM endorsement key, as printed by
[`spire-tpm-plugin`](https://github.com/spiffe/spire-tpm-plugin)'s
`get_tpm_pubhash`. It identifies the *machine* rather than anything about its
software: an EK is burned into the TPM and cannot be moved or changed.

Reading it is an out-of-band step before the bootstrap. For DSX, the ansible
runbook collects it from each host over SSH and writes it into the site spec.

Every thumbprint in the file is seeded into the allowlist of the SPIRE server
`bootstrap site` deploys, and it is the whole of that allowlist:

- a node with a thumbprint can attest and be issued an identity
- a node without one cannot
- a site spec where no node has one deploys SPIRE with no TPM attestor at all
  (`provisioned` only: with `nico` the attestor is always on, and its allowlist
  is filled as CAPNICo reports each machine's key; see
  [SPIRE with NICo nodes](#spire-with-nico-nodes))

It is a public key hash, not a secret, so it belongs in the site spec rather
than beside the credentials, and a file carrying them stays safe to commit.

A malformed thumbprint fails the load. Left to reach the allowlist it would
match nothing and surface much later as that one node failing attestation for no
visible reason.

## The SPIRE CA

`spire.trustDomain` sets the SPIFFE trust domain for the bootstrap server and
for the controller stack install. It must be a lowercase DNS name. If it is
absent, both use `<bootstrap-cluster>.nke.local`, which is
`nvkectl-bootstrap.nke.local` unless `NVKECTL_BOOTSTRAP_CLUSTER_NAME` renames
the bootstrap cluster; then set the field so the install matches. The site
SPIRE server that replaces the bootstrap server uses the same value.

`spire.caCertFile` and `spire.caKeyFile` name a CA the operator brings to the
bootstrap. `nvkectl` configures SPIRE to chain its own CA to it, so a node that
was given that certificate ahead of time — for DSX, delivered by the ansible
runbook over SSH — verifies the server the first time it speaks to it, rather
than trusting whatever answers.

They are paths, not inline PEM. A private key does not belong in a file meant to
be committed, and the same certificate has to be delivered to each node by
whatever provisions it, so it already exists as a file.

Both or neither. Omitting them leaves SPIRE self-signed, which is fine when
nothing outside the cluster has to verify it — and means nodes cannot be given a
trust anchor in advance. Both files are checked for existence at load, while
nothing has been touched yet: otherwise a wrong path surfaces minutes later as a
helm failure naming a temporary file rather than the field that was wrong.

## etcd encryption at rest

`cluster.etcdEncryption` defaults to `true`. The first control-plane node's
kube-apiserver starts with an `EncryptionConfiguration` that encrypts these
resource kinds in etcd with a secretbox key generated for the cluster:
`secrets`, `configmaps`, `persistentvolumeclaims`, `persistentvolumes`,
`serviceaccounts`, `events`, `services`. An `identity` provider stays in the
configuration as a read fallback; it does not make writes plaintext.

The decision is final at bootstrap. Encryption is configured during
`kubeadm init`, and there is no controller on the cluster afterwards to turn it
on, so a cluster built with `etcdEncryption: false` stays unencrypted. Set it
to `false` only when that is the intent.

`nvkectl bootstrap site` writes the key into the working directory, as a Secret
manifest named `<clusterName>-etcd-encryption-key.json` (`--encryption-key-out`
to choose the path; it is resolved independently of `--kubeconfig-out`). Every
control-plane node holds the key in its kube-apiserver configuration; the file
is what a controller installed later is given, and the copy a restore onto new
hosts needs. It is never overwritten, and the run refuses to start if it
already exists.

## Schedulable control plane

Every site spec node is a control-plane node, and kubeadm taints each one
`node-role.kubernetes.io/control-plane:NoSchedule`. With the taint in place
nothing schedules: a Helm `--wait` or `clusterctl init` on the new cluster hangs
until it times out, with no message saying why.

The site spec declares no worker nodes, so the control-plane nodes are the
only place workloads can run. Once every node has joined, `nvkectl bootstrap
site` removes that taint from each node. Other taints are left alone, and a
node that already lacks the taint is not touched. There is no field for this:
the taint follows the topology, and the rule will become "only when the site
spec declares no worker nodes" once it can declare them.

If the removal fails, the cluster is still up and the kubeconfig is on disk.
The error names the command that finishes the step by hand:

```bash
kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule- \
  --kubeconfig <clusterName>.kubeconfig
```

## The control-plane VIP

`cluster.controlPlaneVIP` is an IPv4 address that fronts the Kubernetes API for
every control-plane node. It is required when `cluster.nodes` contains more than
one node, or `infra.nico.controlPlane.count` is more than one with `nico`. When set, the controller runs kube-vip on each node,
kubeadm uses the VIP as the `controlPlaneEndpoint`, and the kubeconfig `bootstrap
site` writes points at it. A single-node cluster with `provisioned` may leave it
empty and use that node's IP as its fixed endpoint; with `nico` it needs the VIP
even for one node, because the node has no address until NICo allocates it and
the controller stack install refuses a site with nothing to reach the controller
at. The controller selects a control-plane node that reports
ready to join, so the endpoint does not necessarily use the first node listed in
the site spec.

The VIP must be routable to the control-plane nodes over BGP. kube-vip
advertises it with local AS 65100 to each node's default gateway, peer AS
4294967000 by default, so the network has to peer with the nodes and carry the
route. Without BGP routing to the initializing node, the other control-plane
nodes cannot reach the VIP to join.

The address must be a routable unicast IPv4 address: the unspecified, loopback,
multicast, link-local and broadcast forms are refused, because the other
control-plane nodes have to dial it to join. It must also sit outside `podCidr`
and `serviceCidr` and must not be a node IP, or it collides with the cluster's
own networks or a node. Loading the site spec rejects a VIP that breaks any of
these rules.

## Relationship to the tier-0 bootstrap template

The field names mirror the tier-0 bootstrap template
so operators do not need to relearn them. The subset of settings our main user
(DSX) configures today is rendered by their Ansible role using the DSX tier-0 bootstrap template.

The differences from tier-0 are deliberate and make the file open-sourceable:

- **Generic registry fields.** tier-0's NVIDIA-specific `registry.ngcApiKey` /
  `registry.sweImageTag` become the registry-agnostic `registry.apiKey`,
  `registry.bootstrapRepo`, and `registry.bootstrapChart`. The image/chart tag
  is baked into the `nvkectl` build rather than set in the file.
- **No deploy-time fields.** `argocd`, `repos`, `skipDeploy`, Forge/baremetal,
  and node-injection blocks are out of scope for the M0 bootstrap and are not
  part of this schema.

## Credential handling

`registry.apiKey` is resolved from, in order of precedence: the
`--registry-api-key` flag, the `REGISTRY_API_KEY` environment variable, then the
file. Keeping the secret out of the file lets the site spec be committed and
shared safely. The credential is never logged or printed.
