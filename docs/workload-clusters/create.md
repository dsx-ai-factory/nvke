# Create a workload cluster

This page describes how to create an NVIDIA Kubernetes Engine (NVKE) workload cluster for a customer with `nvkectl`, watch it until its nodes are ready, change its day-2 settings, and delete it. You create workload clusters after you configure the infrastructure provider, and before you give the customer access.

## Before you begin

- You have completed [Install management-plane services](../operate/install-management-plane-services.md) and [Configure an infrastructure provider: NICo](../operate/provider-nico.md).
- `nvkectl` points at your management plane, set with `nvkectl config set-site`, and `nvkectl get providers` reports the `nico` row as `CONFIGURED`. If you turned on API authentication, `nvkectl` uses an OpenID Connect (OIDC) kubeconfig, as described in [Secure the NVKE API](../operate/api-authentication.md). `nvkectl get providers` reads the management cluster's Kubernetes API with that kubeconfig, which the bootstrap does not configure to accept tokens from your identity provider, so the command fails. Check the provider with the management cluster's admin kubeconfig instead:

  ```bash
  kubectl --kubeconfig <CLUSTER_NAME>.kubeconfig -n capnico-system get deploy,secret
  ```

  The Deployment `controller-manager` shows all replicas ready, and the Secret `nico-credentials` is listed. `<CLUSTER_NAME>` is the management cluster's name.
- You have the NVIDIA Infra Controller (NICo) identifiers listed in [Collect the NICo identifiers](../operate/provider-nico.md#collect-the-nico-identifiers): the organization, site and virtual private cloud (VPC) IDs, the instance type IDs, the VPC prefix or subnet IDs, and optionally SSH key group IDs.
- The releases you plan to use exist on the management cluster. The control plane names a cluster release and each node pool names a node pool release. List them with `nvkectl get clusterreleases` and `nvkectl get nodepoolreleases`.
- If the cluster has more than one control-plane node, you have an unused, routable IPv4 address on the control-plane network for the Kubernetes API virtual IP address (VIP).
- `curl` and `jq` are installed, for the REST calls on this page.

## Write the cluster spec

The cluster spec is a YAML file that describes the cluster, its control plane and its initial worker node pools. Its fields are those of the `CreateClusterRequest` in the NVKE REST API reference, and `nvkectl` rejects any field the schema does not define.

1. Write the spec and save it as `cluster.yaml`.

   ```yaml
   name: <WORKLOAD_CLUSTER>
   controlPlaneKubernetesVersion: <K8S_VERSION>
   cniPlugin: ovn-kubernetes
   etcdEncrypted: true
   controlPlaneVIP: <API_VIP>
   network:
     podCIDR: 192.168.0.0/16
     serviceCIDR: 10.96.0.0/12
   nico:
     orgID: <NICO_ORG_ID>
     siteID: <NICO_SITE_ID>
     vpcID: <NICO_VPC_ID>
   controlPlaneConfig:
     desiredCount: 3
     maxSurge: 1
     systemReservedMemory: 16Gi
     nico:
       instanceTypePreference:
         - <CP_INSTANCE_TYPE_ID>
       vpcPrefixIDs:
         - <VPC_PREFIX_ID>
       sshKeyGroupIDs:
         - <SSH_KEY_GROUP_ID>
       release: <CP_RELEASE>
   nodePools:
     - name: gpu-workers
       nodeNamePrefix: w
       desiredNodeCount: 2
       nico:
         instanceTypeID: <WORKER_INSTANCE_TYPE_ID>
         vpcPrefixIDs:
           - <VPC_PREFIX_ID>
         sshKeyGroupIDs:
           - <SSH_KEY_GROUP_ID>
         release: <WORKER_RELEASE>
   ```

   - `<WORKLOAD_CLUSTER>`: a lowercase DNS label of at most 55 characters, unique on the management plane, such as `example-customer-a`. Node names are built from it.
   - `<K8S_VERSION>`: the Kubernetes version as `X.Y.Z` with no leading `v`, such as `1.34.2`. Use a version your releases' node images carry; see [Supported platforms and versions](../overview/supported-platforms.md).
   - `<API_VIP>`: the Kubernetes API VIP. It must be outside the pod and service CIDRs.
   - `<NICO_ORG_ID>`, `<NICO_SITE_ID>` and `<NICO_VPC_ID>`: your NICo organization ID, and the site and VPC IDs. Every NICo ID in the spec except the organization ID is a UUID.
   - `<CP_INSTANCE_TYPE_ID>` and `<WORKER_INSTANCE_TYPE_ID>`: NICo instance type IDs. `instanceTypePreference` is a required, ordered list: add more IDs after the first as fallbacks.
   - `<VPC_PREFIX_ID>`: a NICo VPC prefix ID. On a site without native networking, use `subnetIDs` with subnet IDs instead. Each `nico` object takes exactly one of the two.
   - `<SSH_KEY_GROUP_ID>`: a NICo SSH key group for node access. Optional; omit `sshKeyGroupIDs` to add none.
   - `<CP_RELEASE>`: the name of a cluster release. `<WORKER_RELEASE>`: the name of a node pool release.

2. Review the settings that are fixed at creation.

   | Field | Default | Notes |
   |---|---|---|
   | `cniPlugin` | `ovn-kubernetes` | `cniVersion` defaults to `1.6.0`, the version the node image carries. |
   | `etcdEncrypted` | None, required | Encrypts etcd at rest. You can turn encryption on later, but not off. |
   | `controlPlaneVIP` | None | Without a VIP, the API endpoint is the first control-plane node's IP, `desiredCount` must be `1`, and you cannot later scale the control plane above one node, upgrade it or replace its nodes. Recreate the cluster to add a VIP. |
   | `network.podCIDR` | `192.168.0.0/16` | Private IPv4 CIDRs, comma-separated, each `/12` to `/23`. Needs one `/24` for every node in the request, plus one. |
   | `network.serviceCIDR` | `10.96.0.0/12` | Private IPv4, `/12` to `/24`. |
   | `nico.orgID`, `siteID`, `vpcID` | None, required | Fixed for the life of the cluster. |

   The network cannot be changed after creation.

3. Review the control-plane and node pool settings.

   | Field | Default | Notes |
   |---|---|---|
   | `controlPlaneConfig.desiredCount` | None, required | Number of control-plane nodes. Use an odd number so that etcd keeps a quorum. More than one requires `controlPlaneVIP`. |
   | `controlPlaneConfig.maxSurge` | `0` | Extra control-plane nodes allowed during a rollout: `0` or `1`. |
   | `controlPlaneConfig.systemReservedMemory` | `16Gi` | Memory the kubelet reserves for system daemons. |
   | `controlPlaneConfig.nodeLabels` | None | Stored with the cluster but not applied to the Kubernetes nodes; see [Preview status and known limitations](../overview/preview-status.md). |
   | `nico.configProfile` (control plane or node pool) | None | Optional. Without one, the nodes run no config profile hooks or labels. |
   | `nodePools[].nodeNamePrefix` | None, required | Node names are `<WORKLOAD_CLUSTER>-<nodeNamePrefix>-<NNNN>` and must fit in 63 characters. |
   | `nodePools[].systemReservedMemory` | `16Gi` | `kubeReservedMemory` defaults to `2Gi`. |
   | `nodePools[].nico.diskSelectionStrategy` | `smallest` | The disk the OS is installed on: `smallest` or `largest`. |

   Node pools also take `recycleBudget` and `autoModeDisabled`, and accept `nodeLabels` and `nodeTaints`, which are stored but not applied to the Kubernetes nodes. [Manage node pools](node-pools.md) describes them. You can create the cluster with no `nodePools` and add pools later.

## Create the cluster

1. Create the cluster from the spec.

   ```bash
   nvkectl create cluster -f cluster.yaml
   ```

   The command prints `Cluster "<WORKLOAD_CLUSTER>" created (id: <CLUSTER_ID>)` and returns before provisioning finishes. `-f -` reads the spec from standard input.

   If the request is invalid, the command prints the API's error and creates nothing. Common causes are a duplicate name, a missing `nico.orgID`, `siteID` or `vpcID`, a missing control-plane `release`, `maxSurge` above `1`, a pod CIDR too small for the nodes, and a VIP inside the pod or service CIDR.

   The API does not check release names at creation. A name with no matching release fails when the first node provisions.

The equivalent REST call sends the same fields as JSON to `POST /api/clusters`. Here `cluster.json` is `cluster.yaml` converted to JSON:

```bash
curl -X POST http://<MANAGEMENT_ADDRESS>:30080/api/clusters \
  -H "Content-Type: application/json" \
  --data @cluster.json
```

`<MANAGEMENT_ADDRESS>` is the management cluster's API VIP, or the node IP of a single-node management cluster. If API authentication is on, add `-H "Authorization: Bearer <TOKEN>"` to each `curl` command that calls `/api/`, where `<TOKEN>` is a JSON Web Token (JWT) from your identity provider, as described in [Secure the NVKE API](../operate/api-authentication.md). The API returns `201` with the new cluster, or `400` with a `message`.

## Watch it come up

1. Check the cluster's status.

   ```bash
   nvkectl get clusters <WORKLOAD_CLUSTER>
   ```

   `STATUS` reads `Provisioning`, then `Provisioned`. The `ID` column holds `<CLUSTER_ID>`, which REST calls use.

   | `STATUS` | Meaning |
   |---|---|
   | `Provisioning` | NVKE is requesting machines from NICo and forming the cluster. |
   | `Provisioned` | The first control-plane node ran `kubeadm init` and the Container Network Interface (CNI) plugin is installed. The other nodes join after this. |
   | `ProvisioningFailed` | Provisioning stopped. The `provisioning` condition's message gives the cause. |
   | `Updating`, `Upgrading`, `Recycling` | A day-2 operation is running. |
   | `UpdateFailed` | The last day-2 operation failed. |
   | `Deleting` | The cluster is being deleted. |

2. If the status stays `Provisioning`, read the condition messages.

   ```bash
   nvkectl get clusters <WORKLOAD_CLUSTER> -o yaml
   ```

   Each condition under `conditions` has a `status`, a `lastTransitionTime` and a `message`. NVKE keeps waiting for a control-plane machine that does not become ready, so a NICo instance type without capacity shows as a cluster that stays in `Provisioning`. See [Troubleshoot](../operate/troubleshoot.md).

3. Check the node pools.

   ```bash
   nvkectl get nodepools --cluster <WORKLOAD_CLUSTER>
   ```

   Each pool's `STATUS` reads `Provisioned` once the cluster is provisioned. Its nodes can still be joining.

## Verify

1. List the cluster's nodes and their Kubernetes readiness.

   ```bash
   curl -s "http://<MANAGEMENT_ADDRESS>:30080/api/nodes?clusterName=<WORKLOAD_CLUSTER>" \
     | jq -r '.nodes[] | [.name, .controlPlane, .conditions.kubernetesReady.status] | @tsv'
   ```

   The output has one line for each control-plane node and each worker, and every line ends in `True`. The count of control-plane lines (`true` in the second column) equals `controlPlaneConfig.desiredCount`.

2. Check the cluster's status.

   ```bash
   nvkectl get clusters <WORKLOAD_CLUSTER>
   ```

   `STATUS` reads `Provisioned`.

## Change cluster settings after creation

You change three cluster settings after creation with `nvkectl patch cluster`, which sends a patch document in YAML or JSON to `PATCH /api/clusters/<CLUSTER_ID>`. Fields you leave out keep their values.

| Field | Effect | Reversible |
|---|---|---|
| `enableEtcdEncryption: true` | Encrypts etcd at rest on a cluster created with `etcdEncrypted: false`. | No. `false` is rejected. |
| `enableOidc: true` | Configures the cluster's Kubernetes API server to accept OIDC tokens for the NVKE management plane's identities. | No. `false` is rejected. |
| `enableCPNodeRecycling` | When `true`, control-plane upgrades and configuration changes replace control-plane nodes automatically. When `false`, the default, nodes that need replacement are reported as pending and wait for you. | Yes. |

`enableEtcdEncryption` and `enableOidc` start a rollout across the control-plane nodes, one node at a time. Both require a cluster in `Provisioned` state. Only one operation runs at a time: while one runs, `STATUS` reads `Updating` and every other `PATCH` returns `409` with a message that names the running operation. You cannot combine the two in one request, or combine either with a Kubernetes version change.

1. Apply the change. For example, to turn on etcd encryption:

   ```bash
   nvkectl patch cluster <WORKLOAD_CLUSTER> --patch 'enableEtcdEncryption: true'
   ```

   The command prints `Cluster "<WORKLOAD_CLUSTER>" patched (id: <CLUSTER_ID>)` and returns before the rollout finishes. Use `--patch 'enableOidc: true'` or `--patch 'enableCPNodeRecycling: true'` for the other settings, or `--patch-file` to read the document from a file.

2. Wait for `STATUS` to return to `Provisioned`.

   ```bash
   nvkectl get clusters <WORKLOAD_CLUSTER>
   ```

   After etcd encryption, `nvkectl get clusters <WORKLOAD_CLUSTER> -o yaml` shows `etcdEncrypted: true`. If `STATUS` reads `UpdateFailed`, send the same patch again to retry.

`enableOidc` requires Kubernetes 1.30.0 or later and turns on the shared OIDC provider that the `nvkeOIDCAuth` chart values configure. The install in [Install management-plane services](../operate/install-management-plane-services.md) configures it for the management cluster's SPIRE server and sets `nvkeOIDCAuth.enabled` to `false`, so new clusters start without it and the `enableOidc` patch is accepted. Try `enableOidc` on one cluster before you rely on it. [Configure OIDC for workload clusters](oidc.md) describes the shared provider, and the fleet-wide providers that a cluster gets only at creation.

The same `PATCH` also takes `controlPlaneKubernetesVersion`, to upgrade the control plane, and `controlPlaneConfig`, to change the control-plane node count or release. An upgrade, a node count above one and a release change each require `controlPlaneVIP`.

## Delete a workload cluster

1. Delete the cluster.

   ```bash
   nvkectl delete cluster <WORKLOAD_CLUSTER>
   ```

   The command asks for no confirmation. It prints `Cluster "<WORKLOAD_CLUSTER>" deletion started (id: <CLUSTER_ID>)` and returns. The REST equivalent is `DELETE /api/clusters/<CLUSTER_ID>`, which returns `202`. You can delete a cluster in any state, including while it provisions or upgrades.

2. Wait for the cluster to disappear.

   ```bash
   nvkectl get clusters
   ```

   The cluster shows `Deleting` until teardown finishes, then is no longer listed.

Deletion removes the node pools and nodes, the cluster's etcd encryption key, its Cluster API objects and its records on the management plane. For each node, NVKE makes a best-effort `kubeadm reset`, then deletes the node's Cluster API machine, which releases the NICo instance back to NICo. A failed reset does not stop the deletion. The cluster stays listed until every machine is released. What happens to the hardware after release is decided by NICo, not NVKE.

If teardown fails, the cluster stays in `Deleting` and its `deleting` condition message starts with `Workflow failed:`. See [Troubleshoot](../operate/troubleshoot.md).

## Next steps

- [Access a workload cluster](access.md) to get the kubeconfig and hand the cluster to the customer.
- [Manage node pools](node-pools.md) to add, scale or remove node pools.
