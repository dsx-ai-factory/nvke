# Site Spec File

The site spec is one YAML file with two parts, used in sequence.

1. **The bootstrap part**: `clusterName`, `cluster`, `registry` and `spire`. It describes your pre-provisioned nodes and the Kubernetes cluster to build on them. `nvkectl bootstrap site` reads it, and nothing else. It is all the bootstrap requires.
2. **The `controllerStack:` section**: the node architecture, the NICo API, the tenant login, and paths to the files that hold the tokens and passwords. The controller stack install reads it after the bootstrap; see [Install the Site Controller](install-site-controller.md). The bootstrap does not read it, so it can stay unfilled until then.

The file is loaded and validated before any node is touched — a typo or missing field fails fast with a clear error.

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

registry:
  bootstrapRepo: "nvcr.io/org"
  bootstrapChart: "controller-chart"
  # Leave apiKey empty and supply it at runtime via --registry-api-key
  # or the REGISTRY_API_KEY env var so the file can be committed safely.
```

The example shows the bootstrap part. `nvkectl generate sitespec` writes the `controllerStack:` section below it with `REPLACE_*` placeholders; the [site spec reference](../../cli/docs/sitespec.md) describes its fields.

## Field Reference

| Field | Type | Required | Notes |
|---|---|---|---|
| `clusterName` | string | yes | Lowercase RFC 1123 label. |
| `cluster.nodes` | list | yes | Must contain an odd number of control-plane nodes and at least one node. |
| `cluster.nodes[].name` | string | yes, per entry | Lowercase RFC 1123 label. Must be unique across all nodes. |
| `cluster.nodes[].ip` | string | yes, per entry | Valid IPv4 or IPv6 address. Must be unique across all nodes. |
| `cluster.nodes[].tpmEkThumbprint` | string | no | SHA-256 of the node's TPM endorsement key: 64 lowercase hex digits. Every thumbprint listed is allowlisted on the SPIRE server the bootstrap deploys; a node without one cannot attest, and a file with none deploys no TPM attestor. Ignored with `--disable-mtls`. |
| `cluster.k8sVersion` | string | yes | Plain semver, e.g. `1.34.2` — no leading `v`. |
| `cluster.cniName` | string | no | Defaults to and must be `ovn-kubernetes`. New site-controller clusters cannot use Calico. |
| `cluster.cniVersion` | string | no | CNI plugin version. Empty uses the agent's built-in default. |
| `cluster.controlPlaneVIP` | string | for multiple nodes | kube-vip control-plane VIP. Required when `cluster.nodes` has more than one entry. It must be a routable unicast IPv4 address, reachable by the nodes over BGP, and outside `podCidr` and `serviceCidr`. It may equal a declared node IP, but CLI acceptance does not prove bootstrap or networking works. See [CLI validation rules](../../cli/docs/sitespec.md#validation-rules) for runtime constraints. A single-node cluster may omit it and use that node's IP as its fixed endpoint. |
| `cluster.podCidr` | string | yes | Valid CIDR. Comma-separated list allowed for dual-stack. |
| `cluster.serviceCidr` | string | yes | Valid CIDR. |
| `cluster.nodeAgentPort` | string | no | gRPC port nvke-agent listens on. Defaults to `15005`. Set it only if you passed `--secure-server-port` to the agent. |
| `cluster.etcdEncryption` | bool | no | Encrypt the Kubernetes API's etcd data at rest. Defaults to `true`. Decided at bootstrap only: a cluster built with it `false` cannot be encrypted afterwards. |
| `cluster.kubeletConfig.systemReservedMemory` | string | no | e.g. `16Gi`, which is also the template default. kubelet subtracts it from capacity, so on a node with 16Gi or less nothing is allocatable and the node never reaches `Ready`; set it to fit the hardware. |
| `cluster.oidcProviders` | list | no | OIDC providers for site cluster. Cannot be modified afterwards. Ignored when absent. Rejected when `cluster.k8sVersion` is older than `1.34.0`. Each entry needs `name`, `issuerURL`, `audiences`, `usernameClaim`, `usernamePrefix`, and `caBundle` (inline PEM). Optional: `groupsClaim`, `groupsPrefix`, `requiredClaims`.|
| `registry.bootstrapRepo` | string | yes | OCI repository holding the bootstrap engine chart. The controller image is published separately, to `<registry>/<org>/controller`. |
| `registry.bootstrapChart` | string | yes | Bootstrap engine chart name within `bootstrapRepo`. |
| `registry.apiKey` | string | no | Registry credential. Never logged or printed. See below. |
| `spire.trustDomain` | string | no | SPIFFE trust domain, shared by the bootstrap and the controller stack install. Set it once, before the bootstrap, or leave it unset: both then use `nvkectl-bootstrap.nke.local`, and the install says so. A bootstrap run with `NVKECTL_BOOTSTRAP_CLUSTER_NAME` set derives another, so set it then. |
| `spire.caCertFile` | string | no | Path to a CA certificate for SPIRE to chain its own CA to, so a node given that certificate when it was provisioned verifies the server on first contact. Without it SPIRE is self-signed. |
| `spire.caKeyFile` | string | no | Path to that CA's private key. Both or neither; both files are checked for existence at load. |
| `controllerStack` | object | not for the bootstrap; yes for the install | The second part of the file. `nvkectl bootstrap site` ignores it; the controller stack install refuses a `REPLACE_*` placeholder in it. See the [site spec reference](../../cli/docs/sitespec.md). |

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
| `REGISTRY_BOOTSTRAP_REPO` | `registry.bootstrapRepo` |
| `REGISTRY_BOOTSTRAP_CHART` | `registry.bootstrapChart` |
| `REGISTRY_API_KEY` | `registry.apiKey` |
| `NVKECTL_SPIFFE_TRUST_DOMAIN` | `spire.trustDomain` |
| `SPIRE_CA_CERT_FILE` | `spire.caCertFile` |
| `SPIRE_CA_KEY_FILE` | `spire.caKeyFile` |
| `NVKECTL_CLUSTER_NODES` | `cluster.nodes` — format `name:ip,name:ip`, applied only when the file lists no nodes |

`INIT_NODE_IP` and `INIT_NODE_NAME` are not supported. The inventory does not
select a node for `kubeadm init`; the bootstrap process selects it downstream.

## Credential Handling

`registry.apiKey` is resolved in this order:

1. `--registry-api-key` flag
2. `REGISTRY_API_KEY` environment variable
3. `registry.apiKey` field in the file

Keep the secret out of the file so the site spec can be committed and shared safely:

```bash
REGISTRY_API_KEY="nvapi-xxxx" nvkectl bootstrap site -f sitespec.yaml
```

## Validating the Site Spec

`nvkectl validate sitespec` checks both parts and reports them one at a time. It exits 0 only when the file is ready for the bootstrap and the install. In between, when the bootstrap part is valid and the `controllerStack:` section is still to fill in, it says exactly that. On a spec `generate sitespec` just wrote:

```console
$ nvkectl validate sitespec -f sitespec.yaml
bootstrap: valid; nvkectl bootstrap site can run with this file
controllerStack: 8 values still to fill in: controllerStack.arch, controllerStack.nico.endpoint, ...
```

and it exits non-zero with `the site spec is not ready for the controller stack install; the bootstrap can run now`. Run the bootstrap on the strength of the first line; fill in the section, run the command again and the second line becomes `controllerStack: valid`. `--bootstrap-only` checks the first part alone and exits 0 on it.

```bash
nvkectl validate sitespec -f sitespec.yaml
nvkectl validate sitespec -f sitespec.yaml --bootstrap-only
```

`bootstrap site --dry-run` loads and validates the bootstrap part and prints the resolved inventory without dispatching any workflow or touching any node. All validation errors are reported together in one pass.

```bash
nvkectl bootstrap site -f sitespec.yaml --dry-run
```

`nvkectl bootstrap site` ends by listing the `REPLACE_*` values still in the file, and the install refuses one. Tokens and passwords are paths to files that hold them, so the site spec holds no credential.

## What's Next After Bootstrap

After the site controller cluster is up, a short list of commands installs Cluster API, the infrastructure provider and the NVKE services on it; see [Install the Site Controller](install-site-controller.md). [Infrastructure Providers](infrastructure-providers.md) describes the provider it installs, and [Deploy Path](deploy-repository.md) the format of the directory the services are installed from.
