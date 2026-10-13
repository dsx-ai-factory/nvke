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

registry:
  bootstrapRepo: "nvcr.io/org"
  bootstrapChart: "controller-chart"
  # Leave apiKey empty and supply it at runtime via --registry-api-key
  # or the REGISTRY_API_KEY env var so the file can be committed safely.
```

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
| `registry.bootstrapRepo` | string | yes | OCI repository holding the bootstrap engine chart. The controller image is published separately, to `<registry>/<org>/controller`. |
| `registry.bootstrapChart` | string | yes | Bootstrap engine chart name within `bootstrapRepo`. |
| `registry.apiKey` | string | no | Registry credential. Never logged or printed. See below. |
| `spire.trustDomain` | string | no | SPIFFE trust domain, shared by the bootstrap and the controller stack install. Set it once, before the bootstrap, or leave it unset: both then use `nvkectl-bootstrap.nke.local`, and the install says so. A bootstrap run with `NVKECTL_BOOTSTRAP_CLUSTER_NAME` set derives another, so set it then. |
| `spire.caCertFile` | string | no | Path to a CA certificate for SPIRE to chain its own CA to, so a node given that certificate when it was provisioned verifies the server on first contact. Without it SPIRE is self-signed. |
| `spire.caKeyFile` | string | no | Path to that CA's private key. Both or neither; both files are checked for existence at load. |

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

`nvkectl validate sitespec` checks the whole file and reports by section: the fields `bootstrap site` reads, then the `controllerStack:` section the controller stack install reads, with the `REPLACE_*` values still to fill in listed apart from errors. It exits 0 only when the file is ready for both. The two halves are used in sequence: once the bootstrap section is valid, `nvkectl bootstrap site` can run with the file, and the `controllerStack:` section is filled in before the stack install. The output says so.

```bash
nvkectl validate sitespec -f sitespec.yaml
nvkectl validate sitespec -f sitespec.yaml --bootstrap-only   # before the controllerStack section is filled in
```

`bootstrap site --dry-run` loads and validates the bootstrap fields and prints the resolved inventory without dispatching any workflow or touching any node. All validation errors are reported together in one pass.

```bash
nvkectl bootstrap site -f sitespec.yaml --dry-run
```

The site spec's `controllerStack:` section is the input for the controller stack install. `nvkectl generate sitespec` writes it with `REPLACE_*` placeholders, `nvkectl bootstrap site` ends by listing the ones still in the file, and the install refuses a placeholder. Fill it in before installing the stack. Tokens and passwords are paths to files that hold them, so the site spec holds no credential.

## What's Next After Bootstrap

After the site controller cluster is up, install Cluster API and an infrastructure provider on it before deploying the NVKE services (CWE, Temporal, CNPG) — see [Infrastructure Providers](infrastructure-providers.md). The NVKE services deployment uses a GitOps-based deploy repository and ArgoCD; see [Deploy Repository](deploy-repository.md) for the structure and application manifests.
