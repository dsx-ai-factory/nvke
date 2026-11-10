# Upgrade a workload cluster's Kubernetes version

Upgrade an NVIDIA Kubernetes Engine (NVKE) workload cluster's control plane, then each worker node pool, within the [Kubernetes version skew policy](https://kubernetes.io/releases/version-skew-policy/). The API does not stop a pool from moving to a newer minor version than the control plane.

## Before you begin

- The cluster meets the upgrade rows of [Supported platforms and versions](../overview/supported-platforms.md).

## Upgrade the control plane

1. Let NVKE replace control-plane nodes, adding each new node before it removes an old one.

   ```bash
   nvkectl patch cluster <WORKLOAD_CLUSTER> --patch '{enableCPNodeRecycling: true, controlPlaneConfig: {maxSurge: 1}}'
   ```

   A control plane with one node needs `maxSurge: 1`: NVKE removes a control-plane node only while at least two are healthy.

2. Set the target version.

   ```bash
   nvkectl patch cluster <WORKLOAD_CLUSTER> --patch 'controlPlaneKubernetesVersion: "<K8S_VERSION>"'
   ```

   Send one version change. Until `STATUS` shows `Upgrading`, a second one is checked against the new target, not the running version, and replaces it.

3. Wait until `STATUS` in `nvkectl get clusters <WORKLOAD_CLUSTER>` is `Provisioned` again and every control-plane node shows `v<K8S_VERSION>` in `kubectl --kubeconfig <WORKLOAD_CLUSTER>.kubeconfig get nodes`, with the kubeconfig from [Get the admin kubeconfig](access.md#get-the-admin-kubeconfig).

> **Warning:** On a control plane with one node, the Kubernetes API can be unavailable briefly at the end of the upgrade, while kubeadm upgrades the add-ons.

## Upgrade a worker node pool

`<API_URL>` and the API token are described in [Manage node pools](node-pools.md).

1. Set the pool's version, and let NVKE add one new node. With `maxSurge: 0`, the default, the change adds no node.

   ```bash
   curl -X PATCH "<API_URL>/api/nodePools/<NODE_POOL_ID>" \
     -H 'Content-Type: application/json' \
     -d '{"kubernetesVersion": "<K8S_VERSION>", "maxSurge": 1}'
   ```

2. Replace the old nodes one at a time, as described in [Control automatic node replacement](node-pools.md#control-automatic-node-replacement). Delete an old node only after the new one is `Ready` on the new version.
