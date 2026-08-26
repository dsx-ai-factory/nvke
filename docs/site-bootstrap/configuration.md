# Inventory File

`nkectl bootstrap site` reads a single YAML inventory file that describes your pre-provisioned nodes and the Kubernetes cluster to build on them. The file is loaded and fully validated before any node is touched — a typo or missing field fails fast with a clear error.

## Example

```yaml
clusterName: "nke-site"

cluster:
  # First control-plane node — kubeadm init runs here.
  initNode:
    name: "ctrl-1"
    ip: "10.0.0.1"

  # Remaining nodes. Omit or leave empty for a single-node cluster.
  joinNodes:
    - name: "ctrl-2"
      ip: "10.0.0.2"
    - name: "ctrl-3"
      ip: "10.0.0.3"

  k8sVersion: "1.32.0"     # plain semver, no leading "v"
  cniName: "calico"         # "calico" or "ovn-kubernetes"
  podCidr: "10.100.0.0/16"
  serviceCidr: "10.200.0.0/16"

  kubeletConfig:
    systemReservedMemory: "16Gi"

registry:
  bootstrapRepo: "nvcr.io/org/cloud-workflow-engine"
  bootstrapChart: "cloud-workflow-engine"
  # Leave apiKey empty and supply it at runtime via --registry-api-key
  # or the REGISTRY_API_KEY env var so the file can be committed safely.
```

## Field Reference

| Field | Type | Required | Notes |
|---|---|---|---|
| `clusterName` | string | yes | Lowercase RFC 1123 label. |
| `cluster.initNode.name` | string | yes | Lowercase RFC 1123 label. |
| `cluster.initNode.ip` | string | yes | Valid IPv4 or IPv6 address. |
| `cluster.joinNodes[].name` | string | yes, per entry | Must be unique across all nodes. |
| `cluster.joinNodes[].ip` | string | yes, per entry | Must be unique across all nodes. |
| `cluster.k8sVersion` | string | yes | Plain semver, e.g. `1.32.0` — no leading `v`. |
| `cluster.cniName` | string | yes | `calico` or `ovn-kubernetes`. |
| `cluster.cniVersion` | string | no | CNI plugin version. Empty uses the agent's built-in default. |
| `cluster.podCidr` | string | yes | Valid CIDR. Comma-separated list allowed for dual-stack. |
| `cluster.serviceCidr` | string | yes | Valid CIDR. |
| `cluster.nodeAgentPort` | string | no | gRPC port nke-agent listens on. nkectl defaults to `50051`; the nke-agent binary defaults to `15005`. Set this to match the agent's configured `--secure-server-port`. |
| `cluster.kubeletConfig.systemReservedMemory` | string | no | e.g. `16Gi`. |
| `registry.bootstrapRepo` | string | yes | OCI repository holding the bootstrap engine image and chart. |
| `registry.bootstrapChart` | string | yes | Bootstrap engine chart name within `bootstrapRepo`. |
| `registry.apiKey` | string | no | Registry credential. Never logged or printed. See below. |

Unknown fields are rejected. A typo in a field name fails the load rather than being silently ignored.

## Environment Variable Overrides

Any field can be supplied or overridden via an environment variable. A set variable takes precedence over the file value.

| Variable | Field |
|---|---|
| `CLUSTER_NAME` | `clusterName` |
| `INIT_NODE_IP` | `cluster.initNode.ip` |
| `INIT_NODE_NAME` | `cluster.initNode.name` |
| `CLUSTER_K8S_VERSION` | `cluster.k8sVersion` |
| `CLUSTER_CNI_NAME` | `cluster.cniName` |
| `CLUSTER_CNI_VERSION` | `cluster.cniVersion` |
| `CLUSTER_POD_CIDR` | `cluster.podCidr` |
| `CLUSTER_SERVICE_CIDR` | `cluster.serviceCidr` |
| `CLUSTER_SYSTEM_RESERVED_MEMORY` | `cluster.kubeletConfig.systemReservedMemory` |
| `CLUSTER_NODE_AGENT_PORT` | `cluster.nodeAgentPort` |
| `REGISTRY_BOOTSTRAP_REPO` | `registry.bootstrapRepo` |
| `REGISTRY_BOOTSTRAP_CHART` | `registry.bootstrapChart` |
| `REGISTRY_API_KEY` | `registry.apiKey` |
| `JOIN_NODES` | `cluster.joinNodes` — format `name:ip,name:ip`, applied only when the file lists no join nodes |

## Credential Handling

`registry.apiKey` is resolved in this order:

1. `--registry-api-key` flag
2. `REGISTRY_API_KEY` environment variable
3. `registry.apiKey` field in the file

Keep the secret out of the file so the inventory can be committed and shared safely:

```bash
REGISTRY_API_KEY="nvapi-xxxx" nkectl bootstrap site -i inventory.yaml
```

## Validating the Inventory

Use `--dry-run` to load, validate, and print the resolved inventory without dispatching any workflow or touching any node:

```bash
nkectl bootstrap site -i inventory.yaml --dry-run
```

All validation errors are reported together in one pass so you can fix them without iterating.

## What's Next After Bootstrap

After the site controller cluster is up, you deploy the NKE services (CWE, Temporal, CNPG) onto it. That step uses a GitOps-based deploy repository and ArgoCD. See [Deploy Repository](deploy-repository.md) for the structure and application manifests.

> **TODO:** Confirm `j7sbcjl3qgta` is the final public NGC org for NKE Helm charts and container images. Update all `helm.ngc.nvidia.com/j7sbcjl3qgta` and `nvcr.io/j7sbcjl3qgta` references if the org changes.
