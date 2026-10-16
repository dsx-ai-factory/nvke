# Site Spec File

`nvkectl bootstrap site` reads a single YAML site spec file that describes your pre-provisioned nodes and the Kubernetes cluster to build on them. The file is loaded and fully validated before any node is touched — a typo or missing field fails fast with a clear error.

`nvkectl generate sitespec <name> --nodes hosts.csv` writes one from a node CSV, so
the file rarely needs assembling by hand — see
[Pre-Provisioned Nodes](pre-provisioned-nodes.md#step-2-run-bootstrap-on-init-node).
This page summarises the schema; the CLI's
[site spec reference](../../cli/docs/sitespec.md) is the full one.

## Example

```yaml
clusterName: "nvke-site"

cluster:
  # Pre-provisioned control-plane nodes.
  nodes:
    - name: "ctrl-1"
      ip: "10.0.0.1"
    - name: "ctrl-2"
      ip: "10.0.0.2"
    - name: "ctrl-3"
      ip: "10.0.0.3"

  k8sVersion: "1.34.2"     # plain semver, no leading "v"
  cniName: "ovn-kubernetes"
  podCidr: "192.168.0.0/16"
  serviceCidr: "10.96.0.0/12"

  kubeletConfig:
    systemReservedMemory: "16Gi"
```

## Field Reference

| Field | Type | Required | Notes |
|---|---|---|---|
| `clusterName` | string | yes | Lowercase RFC 1123 label. |
| `infra.provider` | string | no | `provisioned` (default) or `nico`. With `nico`, CAPNICo allocates the control-plane machines, described by `infra.nico`; see the CLI's [site spec reference](../../cli/docs/sitespec.md). |
| `cluster.nodes` | list | yes, with `provisioned` | Must contain an odd number of control-plane nodes and at least one node. Not used with `infra.provider: nico`, which takes `infra.nico.controlPlane.count` instead and rejects this list. |
| `cluster.nodes[].name` | string | yes, per entry | Lowercase RFC 1123 label. Must be unique across all nodes. |
| `cluster.nodes[].ip` | string | yes, per entry | Valid IPv4 or IPv6 address. Must be unique across all nodes. |
| `cluster.nodes[].tpmEkThumbprint` | string | no | SHA-256 of the node's TPM endorsement key: 64 lowercase hex digits. Every thumbprint listed is allowlisted on the SPIRE server the bootstrap deploys; a node without one cannot attest, and a file with none deploys no TPM attestor. That holds for `provisioned` only: with `infra.provider: nico` there is no node list, and the attestor is always on, with its allowlist filled in from the nodes CAPNICo creates. Ignored with `--disable-mtls`. |
| `cluster.k8sVersion` | string | yes | Plain semver, e.g. `1.34.2` — no leading `v`. |
| `cluster.cniName` | string | no | Defaults to and must be `ovn-kubernetes`. |
| `cluster.cniVersion` | string | no | CNI plugin version. Empty uses the agent's built-in default. |
| `cluster.controlPlaneVIP` | string | for multiple nodes | kube-vip control-plane VIP. Required when `cluster.nodes` (or `infra.nico.controlPlane.count` with nico) has more than one entry. It must be a routable unicast IPv4 address, reachable by the nodes over BGP, and outside `podCidr` and `serviceCidr`. It may equal a declared node IP, but CLI acceptance does not prove bootstrap or networking works. See [CLI validation rules](../../cli/docs/sitespec.md#validation-rules) for runtime constraints. A single-node `provisioned` cluster may omit it and use that node's IP as its fixed endpoint; with `nico` it is needed even for one node, because the node has no address until NICo allocates it, and it cannot be added once the site cluster exists. |
| `cluster.podCidr` | string | yes | Valid CIDR. Comma-separated list allowed for dual-stack. |
| `cluster.serviceCidr` | string | yes | Valid CIDR. |
| `cluster.nodeAgentPort` | string | no | gRPC port nvke-agent listens on. Defaults to `15005`. Set it only if you passed `--secure-server-port` to the agent. |
| `cluster.etcdEncryption` | bool | no | Encrypt the Kubernetes API's etcd data at rest. Defaults to `true`. Decided at bootstrap only: a cluster built with it `false` cannot be encrypted afterwards. |
| `cluster.kubeletConfig.systemReservedMemory` | string | no | e.g. `16Gi`, which is also the template default. kubelet subtracts it from capacity, so on a node with 16Gi or less nothing is allocatable and the node never reaches `Ready`; set it to fit the hardware. |
| `cluster.oidcProviders` | list | no | OIDC providers for site cluster. Cannot be modified afterwards. Ignored when absent. Rejected when `cluster.k8sVersion` is older than `1.34.0`. Each entry needs `name`, `issuerURL`, `audiences`, `usernameClaim`, `usernamePrefix`, and `caBundle` (inline PEM). Optional: `groupsClaim`, `groupsPrefix`, `requiredClaims`.|
| `spire.upstreamCA` | bool | no | `true` when the site's SPIRE chains to a CA you bring. Every bootstrap, validation and install run then requires `NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE`, and the variables are refused without it. |
| `spire.trustDomain` | string | no | SPIFFE trust domain, shared by the bootstrap and the controller stack install. Set it once, before the bootstrap, or leave it unset: both then use `nvkectl-bootstrap.nke.local`, and the install says so. A bootstrap run with `NVKECTL_BOOTSTRAP_CLUSTER_NAME` set derives another, so set it then. |
| `spire.externalAddress` | string | with `nico`, unless `--disable-mtls` | IP address or DNS name nodes created by CAPNICo use to reach the bootstrap SPIRE server, on NodePort 30081. Rejected with `provisioned`. |

Unknown fields are rejected. A typo in a field name fails the load rather than being silently ignored.

## Environment Variable Overrides

Any field can be supplied or overridden via an environment variable. A set variable takes precedence over the file value.

| Variable | Field |
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
| `NVKECTL_SPIRE_EXTERNAL_ADDRESS` | `spire.externalAddress` |
| `NVKECTL_INFRA_PROVIDER` | `infra.provider` — `provisioned` or `nico` |
| `NVKECTL_NICO_CREDENTIALS` | Not a site spec field: the path of the file holding the NICo API token, read on the bootstrap host. Required with `infra.provider: nico` by `bootstrap site` and `validate sitespec --bootstrap-only` |
| `NVKECTL_NICO_ALLOW_INSECURE_ENDPOINT` | Not a site spec field: set to `1` to let `infra.nico.remote.endpoint` be an http URL. The bootstrap sends the NICo token there, so https is required without it; for a local or simulated NICo |
| `NVKECTL_CLUSTER_NODES` | `cluster.nodes` — format `name:ip,name:ip`, applied only when the file lists no nodes |

`INIT_NODE_IP` and `INIT_NODE_NAME` are not supported. The inventory does not
select a node for `kubeadm init`; the bootstrap process selects it downstream.

## Credential Handling

The bootstrap reads the SPIRE CA from the environment, so the site spec can be committed and used unchanged from any machine. `NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE` name the files of a CA for SPIRE to chain its own to, so a node given that certificate when it was provisioned verifies the server on first contact. Set `spire.upstreamCA: true` in the site spec with them, so every later run requires them; both files are checked at load. Without them SPIRE is self-signed.

```bash
NVKECTL_SPIRE_CA_CERT_FILE=/etc/nke/spire-ca.crt NVKECTL_SPIRE_CA_KEY_FILE=/etc/nke/spire-ca.key \
  nvkectl bootstrap site -f sitespec.yaml
```

With `infra.provider: nico` the bootstrap also reads the NICo token from the file `NVKECTL_NICO_CREDENTIALS` names, and the login it renders into every node from `NVKECTL_NODE_USER` and `NVKECTL_NODE_PASSWORD`; see [the CLI reference](../../cli/docs/sitespec.md#credential-handling).

Images from a private registry are pulled with nerdctl, so log in to the
registry with `nerdctl login <registry>` before the bootstrap.

The controller stack install's credentials are listed in
[the site spec reference](../../cli/docs/sitespec.md#credential-handling).

## Validating the Site Spec

`nvkectl validate sitespec` checks the file the way `bootstrap site` reads it, including the SPIRE CA variables `spire.upstreamCA` requires, and prints `valid`. All problems are reported together in one pass. With `infra.provider: nico`, what only the bootstrap host holds (the NICo token file, the node login, the release files, `spire.externalAddress`, and the https check on `infra.nico.remote.endpoint`) is reported but does not change the exit status, because the controller stack install runs this command on a host that lacks those inputs; add `--bootstrap-only` to fail on them instead.

```bash
nvkectl validate sitespec -f sitespec.yaml
nvkectl validate sitespec -f sitespec.yaml --bootstrap-only   # on the bootstrap host, with nico
nvkectl validate sitespec -f sitespec.yaml --bootstrap-only --disable-mtls   # as "bootstrap site --disable-mtls" would: no spire.externalAddress needed with nico
```

`bootstrap site --dry-run` loads and validates the bootstrap fields and prints the resolved inventory without dispatching any workflow or touching any node. All validation errors are reported together in one pass.

```bash
nvkectl bootstrap site -f sitespec.yaml --dry-run
```

The controller stack install reads the site spec too, for the site name, the control-plane VIP and the SPIRE trust domain, and takes everything only the stack needs from a file of its own, the stack config; [deploy/README.md](../../deploy/README.md) describes it. The install reads the NICo token, the node login and the PostgreSQL password from `NVKECTL_NICO_TOKEN`, `NVKECTL_NODE_USER`, `NVKECTL_NODE_PASSWORD` and `NVKECTL_DATABASE_PASSWORD`, so neither file holds a credential.

## What's Next After Bootstrap

After the site controller cluster is up, install Cluster API and an infrastructure provider on it before deploying the NVKE services (CWE, Temporal, CNPG) — see [Infrastructure Providers](infrastructure-providers.md). The NVKE services deployment uses a GitOps-based deploy repository and ArgoCD; see [Deploy Repository](deploy-repository.md) for the structure and application manifests.
