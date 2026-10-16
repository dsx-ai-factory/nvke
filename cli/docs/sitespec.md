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

`cluster.nodes` is deliberately not settable; it comes from `--nodes`.
Credentials and the SPIRE CA come from the environment: see "Credential handling".

Unlike `bootstrap site`, `generate sitespec` ignores the environment variables
listed below, so a generated file never carries a value nobody typed. Every
field is validated before anything is written, so a file this command produces
is a file `bootstrap site` accepts.

## Schema

| Field | Type | Required | Notes |
|---|---|---|---|
| `clusterName` | string | yes | Lowercase RFC 1123 label. |
| `cluster.nodes` | list | yes | Must contain an odd number of control-plane nodes and at least one node. |
| `cluster.nodes[].name` | string | yes, per entry | Lowercase RFC 1123 label. Unique across all nodes. |
| `cluster.nodes[].ip` | string | yes, per entry | Valid IPv4 or IPv6 address. Unique across all nodes. |
| `cluster.nodes[].tpmEkThumbprint` | string | no | SHA-256 of the node's TPM endorsement key: 64 lowercase hex digits. See below. |
| `cluster.k8sVersion` | string | yes | Plain semantic version, e.g. `1.32.0` (no leading `v`). |
| `cluster.cniName` | string | no | Defaults to and must be `ovn-kubernetes`. |
| `cluster.cniVersion` | string | no | CNI plugin version. Empty uses the node-agent default. |
| `cluster.controlPlaneVIP` | string | for multiple nodes | IPv4 control-plane VIP. Required when `cluster.nodes` has more than one entry. A single-node cluster may omit it and use that node's IP as its fixed endpoint. See below. |
| `cluster.podCidr` | string | yes | Valid CIDR. A comma-separated list of CIDRs is allowed. Must not overlap `serviceCidr`. |
| `cluster.serviceCidr` | string | yes | Valid CIDR. |
| `cluster.nodeAgentPort` | string | no | Port number 1–65535. Defaults to `15005`. |
| `cluster.etcdEncryption` | bool | no | Encrypt the Kubernetes API's etcd data at rest. Defaults to `true`. Decided at bootstrap only: a cluster built with it `false` cannot be encrypted afterwards. See below. |
| `cluster.kubeletConfig.systemReservedMemory` | string | no | e.g. `16Gi`. |
| `cluster.oidcProviders` | list | no | OIDC providers for site cluster. Cannot be modified afterwards. Ignored when absent. Rejected when `cluster.k8sVersion` is older than `1.34.0`. Each entry needs `name`, `issuerURL`, `audiences`, `usernameClaim`, `usernamePrefix`, and `caBundle` (inline PEM). Optional: `groupsClaim`, `groupsPrefix`, `requiredClaims`.|
| `spire.upstreamCA` | bool | no | `true` when the site's SPIRE chains to a CA you bring. Every `bootstrap site`, `validate sitespec` and install run then requires `NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE`, and the variables are refused without it. See "The SPIRE CA". |
| `spire.trustDomain` | string | no | SPIFFE trust domain, shared by the bootstrap and the controller stack install. Set it once, before the bootstrap, or leave it unset: both then use `nvkectl-bootstrap.nke.local`, and the install says so. A bootstrap run with `NVKECTL_BOOTSTRAP_CLUSTER_NAME` set derives another, so set it then. |
The version (image/chart tag) is baked into the `nvkectl` build, so it is not
part of the site spec.

Unknown fields are rejected. A typo in a field name fails the load rather than
being silently ignored. Removed fields,
`registry`, `spire.caCertFile` and `spire.caKeyFile`,
are refused with the name of what replaced them. So are the
`SPIRE_CA_CERT_FILE` and `SPIRE_CA_KEY_FILE` variables: a run with either set
fails, naming `NVKECTL_SPIRE_CA_CERT_FILE`, `NVKECTL_SPIRE_CA_KEY_FILE` and
`spire.upstreamCA`.

## The controller stack install

The controller stack install reads the site spec too, and changes nothing
in it: the site name, the control-plane VIP and the SPIRE trust domain are
the bootstrap's facts the stack must agree with. Everything only the stack
needs lives in a separate file, the stack config, which starts from
[`deploy/examples/stack-config.json`](../../deploy/examples/stack-config.json);
[deploy/README.md](../../deploy/README.md) lists its keys. The credentials
come from the environment (see "Credential handling"), and the nodes'
architecture is in neither file: the install reads it from the cluster.

## Validation rules

- All problems are reported together in one error, each naming the offending
  field, so a bad file can be fixed in one pass.
- The site spec must contain an odd number of nodes and at least one node.
- Node names and IPs must be unique across all nodes.
- `podCidr` / `serviceCidr` are parsed as CIDRs; `podCidr` also accepts a
  comma-separated list and validates each entry. No `podCidr` entry may
  overlap `serviceCidr`.
- `controlPlaneVIP`, when set, must parse as a routable unicast IPv4 address,
  and must not fall inside `podCidr` or `serviceCidr`. It may equal a declared
  node IP.
- `controlPlaneVIP` is required when `cluster.nodes` contains more than one
  node.

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

The bootstrap fields below can be supplied — or overridden — through an
environment variable, so existing tier-0 / DS9 scripts can drive the site spec
without editing the file.
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
| `NVKECTL_SPIFFE_TRUST_DOMAIN` | `spire.trustDomain` |
| `NVKECTL_CLUSTER_NODES` | `cluster.nodes` — format `name:ip,name:ip`, applied only when the file lists no nodes |

A malformed `NVKECTL_CLUSTER_NODES` entry fails the load rather than being
silently discarded.

`INIT_NODE_IP` and `INIT_NODE_NAME` are not supported. The site spec does not
select a node for `kubeadm init`; the bootstrap process selects it downstream.

## TPM endorsement-key thumbprints

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

`NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE` name a CA the
operator brings to the bootstrap, on the machine that runs `bootstrap site` or
the stack install. `nvkectl` configures SPIRE to
chain its own CA to it, so a node that was given that certificate ahead of
time — for DSX, delivered by the ansible runbook over SSH — verifies the server
the first time it speaks to it, rather than trusting whatever answers.

`spire.upstreamCA: true` records in the site spec that the site uses such a
CA. With it, every run requires both variables, so an install run from a shell
without them fails instead of moving SPIRE to a self-signed root that nodes
given the CA cannot trust. Without it, the variables are refused. Omitting both
leaves SPIRE self-signed, which is fine when nothing outside the cluster has to
verify it — and means nodes cannot be given a trust anchor in advance. Both
files are checked at load, while nothing has been touched yet.

## OIDC providers

`cluster.oidcProviders` is the OpenID Connect (OIDC) configuration of the management cluster's API server. `bootstrap site` applies the list when it creates the management cluster. You cannot change the list after creation. Omit the field to configure no OIDC providers.

A non-empty list requires `cluster.k8sVersion` `1.34.0` or newer and contains at most five providers. Each provider has these fields:

| Field | Required | Description |
|---|---|---|
| `name` | yes | Name of this provider. Unique in the list. |
| `issuerURL` | yes | Issuer URL. An `https` URL, unique in the list. The API server compares it with the token's `iss` claim. |
| `audiences` | yes | `aud` values the API server accepts. At least one, and none empty. |
| `usernameClaim` | yes | Claim the API server uses as the username. |
| `usernamePrefix` | yes | Text the API server puts in front of the username. It must not start with `system:`, `kubeadm:`, `nvke:`, or `nvidia:`. |
| `groupsClaim` | no | Claim the API server uses as the user's groups. |
| `groupsPrefix` | no | Text the API server puts in front of each group. The reserved prefixes are the same as for `usernamePrefix`. |
| `requiredClaims` | no | Map of claim name to the value that claim must hold. |
| `caBundle` | yes | PEM-encoded certificates the API server trusts for this provider. |

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
one node. When set, the controller runs kube-vip on each node,
kubeadm uses the VIP as the `controlPlaneEndpoint`, and the kubeconfig `bootstrap
site` writes points at it. A single-node cluster may leave it empty and use that
node's IP as its fixed endpoint. The controller selects a control-plane node that reports
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

- **Images from the command line.** tier-0's `registry.ngcApiKey` and
  `registry.sweImageTag` give way to `--controller-image` and
  `--provisioned-provider-image`, with registry access from `nerdctl login`.
  The chart versions are baked into the `nvkectl` build.
- **No deploy-time fields.** `argocd`, `repos`, `skipDeploy`, Forge/baremetal,
  and node-injection blocks are out of scope for the M0 bootstrap and are not
  part of this schema.

## Credential handling

Credentials come from the environment of the command that needs them, so the
site spec can be committed and used unchanged from any machine. The secret
values are never logged or printed.

| Environment variable | Read by | What |
|---|---|---|
| `NVKECTL_SPIRE_CA_CERT_FILE`, `NVKECTL_SPIRE_CA_KEY_FILE` | `bootstrap site`, `validate sitespec`, the stack install | Paths to the SPIRE CA certificate and its private key. Required with `spire.upstreamCA: true`, refused without it. See "The SPIRE CA". |
| `NVKECTL_NICO_TOKEN` | the stack install, which `deploy/check-site.sh` checks | NICo API token. |
| `NVKECTL_NODE_USER` | the stack install | Login account rendered into every tenant node's cloud-init. |
| `NVKECTL_NODE_PASSWORD` | the stack install | Password for `NVKECTL_NODE_USER`. |
| `NVKECTL_DATABASE_PASSWORD` | the stack install | Postgres password the controller and Temporal share. Keep it: every install of the same site needs the same value. |
