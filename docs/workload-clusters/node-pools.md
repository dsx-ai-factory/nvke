# Manage node pools

This page describes how to add, scale and delete the worker node pools of an NVIDIA Kubernetes Engine (NVKE) workload cluster, replace individual nodes, and control automatic node replacement (auto mode). You do these tasks after [Create a workload cluster](create.md), whenever the cluster's capacity or node configuration needs to change.

## Before you begin

- You have completed [Configure an infrastructure provider: NICo](../operate/provider-nico.md) and [Create a workload cluster](create.md), and the cluster's status in `nvkectl get clusters` is `Provisioned`.
- `nvkectl` points at your management plane, set with `nvkectl config set-site`. If API authentication is on, `nvkectl` uses an OpenID Connect (OIDC) kubeconfig, as described in [Secure the NVKE API](../operate/api-authentication.md). With that kubeconfig, `nvkectl get nodepoolreleases` and `nvkectl get kubeconfig` fail, because they read the management cluster's Kubernetes API, which does not accept your identity provider's tokens; the steps that use them give a `kubectl` alternative with the management cluster's admin kubeconfig, `<CLUSTER_NAME>.kubeconfig`.
- You have `curl` and `jq`. Listing nodes, deleting a node and changing auto-mode settings use the NVKE API directly, because `nvkectl` has no commands for them.
- You know `<API_URL>`, the base address of the NVKE API without `/api`: `http://<MANAGEMENT_ADDRESS>:30080`, where `<MANAGEMENT_ADDRESS>` is the management cluster's API virtual IP address (VIP), or the node IP of a single-node management cluster. If API authentication is on, add `-H "Authorization: Bearer <TOKEN>"` to each `curl` command that calls `/api/`, where `<TOKEN>` is a JSON Web Token (JWT) from your identity provider, as described in [Secure the NVKE API](../operate/api-authentication.md).

## List node pools and nodes

1. List the node pools of the cluster.

   ```bash
   nvkectl get nodepools --cluster <WORKLOAD_CLUSTER>
   ```

   `<WORKLOAD_CLUSTER>` is the workload cluster's name. The table shows each pool's `NAME`, `DESIRED` node count, `K8S VERSION`, `STATUS` (`Provisioning`, `Provisioned`, `Deleting` or `Unknown`) and `ID`. Add `-o yaml` and a pool name to see one pool's full settings, including its `nico` block.

2. List the nodes of one pool.

   ```bash
   curl -s "<API_URL>/api/nodes?nodePoolID=<NODE_POOL_ID>" \
     | jq -r '.nodes[] | [.id, .name, .ip, .conditions.provisioned.status, .conditions.kubernetesReady.status, .conditions.markedForRecycling.status] | @tsv'
   ```

   `<NODE_POOL_ID>` is the pool's `ID` from step 1. Each line shows a node's ID, name and IP address, then whether it is provisioned, whether its kubelet reports `Ready`, and whether it is marked for replacement. Use `clusterName=<WORKLOAD_CLUSTER>&controlPlane=false` instead of `nodePoolID` to list every worker in the cluster.

## Add a node pool

A node pool is a group of worker nodes that share an NVIDIA Infra Controller (NICo) instance type, a node pool release and a Kubernetes version. A new pool takes the cluster's Kubernetes version.

1. Find the values the pool needs.

   ```bash
   nvkectl get clusters <WORKLOAD_CLUSTER> -o json | jq -r '.[0].id'
   ```

   The output is the cluster ID. You also need:

   - The NICo instance type ID for the GPU machines, from NICo.
   - The name of a node pool release you have published, which `nvkectl get nodepoolreleases` lists. With API authentication on, list them with `kubectl --kubeconfig <CLUSTER_NAME>.kubeconfig get nodepoolreleases` instead.
   - The `vpcPrefixIDs` or `subnetIDs` for the pool's nodes. If the cluster already has a worker pool, copy them from it with `nvkectl get nodepools <POOL_NAME> --cluster <WORKLOAD_CLUSTER> -o yaml`. For a cluster created without `nodePools`, use the VPC prefix or subnet IDs you recorded in [Collect the NICo identifiers](../operate/provider-nico.md#collect-the-nico-identifiers).

2. Write the node pool spec and save it as `pool.yaml`.

   ```yaml
   clusterID: <CLUSTER_ID>
   name: gpu-workers-b
   nodeNamePrefix: gb
   desiredNodeCount: 4
   recycleBudget: 1
   nico:
     instanceTypeID: <WORKER_INSTANCE_TYPE_ID>
     release: <WORKER_RELEASE>
     vpcPrefixIDs:
       - <VPC_PREFIX_ID>
   ```

   - `<CLUSTER_ID>`: the cluster ID from step 1.
   - `name`: unique within the cluster, lowercase, and fixed once created.
   - `nodeNamePrefix`: nodes are named `<WORKLOAD_CLUSTER>-<nodeNamePrefix>-<NNNN>`, at most 63 characters in total.
   - `<WORKER_INSTANCE_TYPE_ID>`: the NICo instance type. It is fixed at creation; to change machine type, add a new pool.
   - `<WORKER_RELEASE>`: the name of a node pool release.
   - `<VPC_PREFIX_ID>`: a NICo virtual private cloud (VPC) prefix. Give exactly one of `vpcPrefixIDs` (sites with native networking) or `subnetIDs`.
   - `recycleBudget`: how many nodes auto mode may replace at once. See [Control automatic node replacement](#control-automatic-node-replacement).

   Optional fields include `configProfile`, `sshKeyGroupIDs`, `systemReservedMemory` (default `16Gi`), `kubeReservedMemory` (default `2Gi`), `nodeLabels` and `nodeTaints`. Labels and taints are fixed at creation. At preview, NVKE stores `nodeLabels` and `nodeTaints` on the pool but does not apply them to the Kubernetes nodes; see [Preview status and known limitations](../overview/preview-status.md).

3. Create the pool.

   ```bash
   nvkectl create nodepool -f pool.yaml
   ```

   The command prints `Node pool "gpu-workers-b" created` with the pool's ID and returns while provisioning continues. The API rejects the request if the name is already used in the cluster, if `instanceTypeID` or `release` is missing, or if the cluster's pod CIDR has no room for the requested nodes.

4. Watch the nodes join.

   ```bash
   nvkectl get nodepools gpu-workers-b --cluster <WORKLOAD_CLUSTER>
   ```

   On a provisioned cluster the pool's status reads `Provisioned` within moments, before its nodes join. Follow the nodes by listing them as in [List node pools and nodes](#list-node-pools-and-nodes). A node waits as pending until NICo allocates it a machine of the instance type.

## Machine annotations and labels

Set `machineAnnotations` and `machineLabels` on a node pool at creation, either
inside a cluster's `nodePools` list or in a standalone node pool request:

```yaml
machineAnnotations:
  example.com/setup: "gpu,network"
machineLabels:
  example.com/workload: gpu
```

Both fields are optional maps of strings and are fixed when the pool is created.
They are applied only to new CAPI Machines, including scale-out and replacement
Machines. They do not label or annotate Kubernetes Nodes or infrastructure
Machines. They are not reconciled after provisioning. Annotation values can
be free-form strings, while labels must follow Kubernetes label syntax.

Keys under `nke.nvidia.com`, `nke.io`, `nvke.nvidia.com`, `kubernetes.io`, `k8s.io`,
`cluster.x-k8s.io` and their subdomains are reserved. Operators can allow exact
keys using `machineMetadata.allowlistedLabelKeys` and
`machineMetadata.allowlistedAnnotationKeys` in the controller Helm values
(`machineMetadataAllowlistedLabelKeys` and
`machineMetadataAllowlistedAnnotationKeys` in `nke-controller-config`). Both
lists default to empty. Label exceptions do not permit annotation keys, or vice
versa. Add the configuration-profile selection annotation to the annotation
allowlist if it uses a reserved domain. These settings do not change `nodeLabels`
validation.

For control-plane Machines, set the same fields under `controlPlaneConfig` in
the cluster creation request. They are fixed at cluster creation, use the same
allowlists, and apply to newly provisioned control-plane Machines, including
scale-out and replacement Machines. They cannot be changed through the cluster
update API.

## Scale a node pool

`desiredNodeCount` is the number of nodes NVKE keeps in the pool. It is not workload-driven autoscaling: NVKE changes the count only when you do.

1. Set the new count.

   ```bash
   nvkectl patch nodepool <POOL_NAME> --cluster <WORKLOAD_CLUSTER> --patch 'desiredNodeCount: 8'
   ```

   `desiredNodeCount` is the only field `nvkectl patch nodepool` accepts. The command prints `Node pool "<POOL_NAME>" patched` and returns. The API rejects a higher count if the cluster's pod CIDR has no room for the extra nodes.

2. If you raised the count, watch the new nodes join as in [Add a node pool](#add-a-node-pool), step 4.

3. If you lowered the count, delete the nodes you want to remove.

   Lowering `desiredNodeCount` removes only pending nodes that NVKE has not yet started to provision. NVKE does not remove running nodes, or nodes already waiting for a NICo machine, to meet a lower count. Choose the nodes to remove, then delete each one as in [Replace a node](#replace-a-node). NVKE does not replace a deleted node while the pool has at least `desiredNodeCount` nodes.

## Replace a node

Deleting a worker node removes it from the cluster and returns the machine to NICo. If the pool then has fewer nodes than `desiredNodeCount`, NVKE provisions a replacement on any free machine of the pool's instance type.

1. Find the node's ID by listing the pool's nodes, as in [List node pools and nodes](#list-node-pools-and-nodes).

2. Delete the node.

   ```bash
   curl -X DELETE "<API_URL>/api/nodes/<NODE_ID>"
   ```

   `<NODE_ID>` is the node's ID. The API returns `202` and the deletion continues in the background.

   To also flag the machine in NICo for repair, send a `machineHealthIssue`. `category` is one of `Hardware`, `Network`, `Performance` or `Other`.

   ```bash
   curl -X DELETE "<API_URL>/api/nodes/<NODE_ID>" \
     -H 'Content-Type: application/json' \
     -d '{"machineHealthIssue": {"category": "Hardware", "summary": "Node reports 7 of 8 GPUs"}}'
   ```

   If the node no longer responds, add `"forceUnreachable": true` to the body so that NVKE tolerates failures while it shuts the node down.

Before it removes a worker, NVKE cordons the node and evicts its pods through the Kubernetes eviction API, which honors PodDisruptionBudgets. Pods still on the node after 100 seconds are deleted without further waiting, so schedule the delete when the customer's disruption policy allows it.

## Delete a node pool

1. Delete the pool.

   ```bash
   nvkectl delete nodepool <POOL_NAME> --cluster <WORKLOAD_CLUSTER>
   ```

   The command prints that deletion started and returns. NVKE removes every node in the pool, each drained as described in [Replace a node](#replace-a-node).

2. Check progress.

   ```bash
   nvkectl get nodepools --cluster <WORKLOAD_CLUSTER>
   ```

   The pool shows `Deleting`, then disappears from the list.

## Control automatic node replacement

Auto mode moves a pool onto a new node configuration by marking outdated nodes for replacement, a few at a time. A node becomes outdated when you change the pool's node configuration, such as its release or Kubernetes version, after the node was built. Upgrading a pool's Kubernetes version is not covered on this page.

Auto mode works as follows:

- **NVKE marks, something else deletes.** NVKE sets the condition `nvke.nvidia.com/NeedsRecycle` on the Kubernetes node of each outdated node it selects, oldest first, and reports it as `markedForRecycling` in the node list. NVKE does not delete marked nodes itself. NVSentinel, or your own process, drains and deletes them when the customer's maintenance policy allows, and NVKE then provisions replacements on the current configuration. Without NVSentinel, delete marked nodes yourself as in [Replace a node](#replace-a-node).
- **`recycleBudget` limits how many nodes are out at once.** NVKE marks at most `recycleBudget` minus the pool's shortfall, where the shortfall is `desiredNodeCount` minus the number of available nodes. A node is unavailable when it is not `Ready`, is cordoned, is being deleted, or is marked for replacement or repair, and a node the pool is still waiting for adds to the shortfall. With `desiredNodeCount: 4`, `recycleBudget: 2` and one node `NotReady`, NVKE marks at most one node.
- **Auto mode is inactive until you set a budget.** `recycleBudget` defaults to `0`, and with `0` NVKE marks nothing.

NVSentinel can also request a reboot or a replacement of a faulty node, and NVKE carries it out.

To change auto-mode settings, send an update request to the pool. `nvkectl patch nodepool` does not accept these fields.

1. Set the budget.

   ```bash
   curl -X PATCH "<API_URL>/api/nodePools/<NODE_POOL_ID>" \
     -H 'Content-Type: application/json' \
     -d '{"recycleBudget": 2}'
   ```

   The response is the updated pool in JSON, with `"recycleBudget": 2`.

2. To turn auto mode off for the pool, set `autoModeDisabled`.

   ```bash
   curl -X PATCH "<API_URL>/api/nodePools/<NODE_POOL_ID>" \
     -H 'Content-Type: application/json' \
     -d '{"autoModeDisabled": true}'
   ```

   NVKE stops marking nodes in the pool. Nodes it already marked keep the condition.

Two related settings take the same request:

| Field | Default | When `true` |
|---|---|---|
| `autoProvisionDisabled` | `false` | NVKE stops creating and provisioning nodes for the pool, including replacements and changes to `desiredNodeCount`. |
| `provisioningMonitorDisabled` | `false` | NVKE stops replacing worker nodes that have been provisioning for longer than the management plane's timeout, 2 hours by default. |

## Verify

1. Check the pools.

   ```bash
   nvkectl get nodepools --cluster <WORKLOAD_CLUSTER>
   ```

   Every pool you kept shows `Provisioned` with the `DESIRED` count you set.

2. Check the nodes from inside the cluster.

   ```bash
   nvkectl get kubeconfig <WORKLOAD_CLUSTER> --output-file <WORKLOAD_CLUSTER>.kubeconfig
   ```

   With API authentication on, this command fails; get the file with `kubectl` as described in [Get the admin kubeconfig](access.md#get-the-admin-kubeconfig) instead.

   ```bash
   kubectl --kubeconfig <WORKLOAD_CLUSTER>.kubeconfig get nodes
   ```

   Each pool's nodes are listed with names that start with `<WORKLOAD_CLUSTER>-<nodeNamePrefix>-` and status `Ready`.

## Next steps

- [Observe NVKE and its clusters](../operate/observe.md) to monitor node and pool health.
- [Troubleshoot](../operate/troubleshoot.md) if nodes stay pending or do not join.
