# Configure an infrastructure provider: LaunchLayer

This page describes how to confirm that the NVIDIA Kubernetes Engine (NVKE) management plane can request machines from LaunchLayer, how to manage the credentials it uses, and which LaunchLayer identifiers each workload cluster request needs. NVKE requests machines through CAPLL, the Cluster API (CAPI) infrastructure provider for LaunchLayer, which runs on the management cluster and calls the LaunchLayer API with an OAuth2 client. You do this after you install the management plane and before you create the first workload cluster.

[Infrastructure providers](infrastructure-providers.md) describes how NVKE uses a provider. See also [Preview status and known limitations](../overview/preview-status.md).

## Before you begin

- You have installed the management plane with the `deploy/launchlayer` deploy path, as its [README](https://github.com/dsx-ai-factory/nvke/blob/main/deploy/launchlayer/README.md) describes. It is the install in [Install the management plane](install-management-plane.md), run with `--deploy-path deploy/launchlayer` and the inputs the README lists. It installs CAPLL in the `capll-system` namespace and writes the `launchlayer-credentials` Secret from the `launchLayer` section of the stack config.
- Your SPIRE server and node images support DSX IMDS node attestation, as described in [Infrastructure Providers](../site-bootstrap/infrastructure-providers.md#launchlayer).
- You have the management cluster kubeconfig, `<CLUSTER_NAME>.kubeconfig`, and `kubectl` on `PATH`. The kubeconfig must be able to read Secrets in the `capll-system` namespace.
- `nvkectl` points at your management plane. With API authentication on, `nvkectl get providers` fails, as described in [Configure an infrastructure provider: NICo](provider-nico.md#before-you-begin); the steps give a `kubectl` alternative.
- In LaunchLayer, you have an OAuth2 client for CAPLL, a site, an organization or an existing virtual private cloud (VPC), and the instance types and host OS profiles for the nodes.

Run the commands on this page with `KUBECONFIG` set:

```bash
export KUBECONFIG=$PWD/<CLUSTER_NAME>.kubeconfig
```

## Confirm that CAPLL is running

1. Check the provider state.

   ```bash
   nvkectl get providers
   ```

   The `launchlayer` row reads `CONFIGURED`. `NOT-INSTALLED` or `NOT-CONFIGURED` means the install did not complete; run it again as described in [Run the install again](install-management-plane.md#run-the-install-again). `DEGRADED` means the CAPLL controller is not available or has no `--base-url`. `UNKNOWN` usually means the kubeconfig cannot read Secrets in `capll-system`. For any state other than `CONFIGURED`, the `DETAIL` column names the cause.

   With API authentication on, check CAPLL with `kubectl` instead:

   ```bash
   kubectl -n capll-system get deploy,secret
   ```

   The CAPLL controller Deployment shows all replicas ready, and the Secret `launchlayer-credentials` is listed.

2. Check that the management plane is running.

   ```bash
   kubectl -n nvke-controller get deploy nvke-controller
   ```

   The Deployment is ready. A node attestation method other than `dsxImds` with SPIFFE on never gets this far: the chart refuses to render, so the install fails and its output names `nodeAgent.spireAttestationMethod`.

`CONFIGURED` does not mean the credentials work. CAPLL first uses them when it reconciles a workload cluster's `LaunchLayerCluster`.

## Manage the LaunchLayer credentials

CAPLL gets an OAuth2 token for the LaunchLayer API with the client in the Secret `launchlayer-credentials` in the `capll-system` namespace.

| Key | Value | Where it comes from |
|---|---|---|
| `issuerURL` | The OAuth2 issuer CAPLL gets its token from. | `launchLayer.issuerURL` in the stack config |
| `clientID` | The OAuth2 client ID. | `launchLayer.clientID` in the stack config |
| `clientSecret` | The OAuth2 client secret. | `NVKECTL_LAUNCHLAYER_CLIENT_SECRET` in the install's environment |
| `scopes` | Space-separated scopes the token must carry. Optional. | `launchLayer.scopes` in the stack config |

The LaunchLayer API address is not in the Secret. It is the CAPLL `--base-url` flag, from `launchLayer.baseURL` in the stack config.

To rotate the client secret or change any other value, update the stack config or `NVKECTL_LAUNCHLAYER_CLIENT_SECRET`, then [run the install again](install-management-plane.md#run-the-install-again). The install writes the Secret from those inputs.

## Collect the LaunchLayer identifiers

Each workload cluster request carries its LaunchLayer identifiers in a `launchLayer` object on the cluster, the control plane and each node pool, in place of `nico`. Every field is fixed at creation.

| Field | Where it goes | Value |
|---|---|---|
| `siteID` | Cluster `launchLayer` | Required. The LaunchLayer datahall the machines come from. |
| `vpcID` or `orgID` | Cluster `launchLayer` | Exactly one. `vpcID` names an existing VPC, which CAPLL never deletes. `orgID` names the organization CAPLL creates a VPC in. |
| `vpcType` | Cluster `launchLayer` | Optional. `BARE_METAL` or `VIRTUALIZED`, the default. |
| `securityGroups` | Cluster `launchLayer` | Optional. Security groups CAPLL creates with the cluster and deletes with it. |
| `instanceType` | Control plane and node pool `launchLayer` | Required. The machine shape, such as `vm200`. |
| `hostOSProfile` | Control plane and node pool `launchLayer` | Optional. The bare-metal profile, as `<name>` or `<name>:<version>`. |
| `securityGroupRefs` | Control plane and node pool `launchLayer` | Optional. Names of the cluster's security groups to attach, per network interface: `primaryNetworkInterface` and `gpuNetworkInterface`. |

`vmProfile` takes the place of `hostOSProfile` for virtual machines. With SPIFFE on and `nodeAgent.externalMTLS.enabled` off, the API rejects it, because DSX IMDS attests bare-metal machines only.

In the cluster spec from [Create a workload cluster](../workload-clusters/create.md), replace each `nico` object with a `launchLayer` object:

```yaml
launchLayer:
  siteID: <LL_SITE_ID>
  orgID: <LL_ORG_ID>
controlPlaneConfig:
  desiredCount: 1
  launchLayer:
    instanceType: <CP_INSTANCE_TYPE>
    hostOSProfile: <HOST_OS_PROFILE>
nodePools:
  - name: gpu-workers
    nodeNamePrefix: w
    desiredNodeCount: 2
    launchLayer:
      instanceType: <WORKER_INSTANCE_TYPE>
      hostOSProfile: <HOST_OS_PROFILE>
```

Replace the placeholders with the values in the table above. The other fields of the spec are the same as for NICo.

## Verify

1. Check the provider state.

   ```bash
   nvkectl get providers
   ```

   The `launchlayer` row reads `CONFIGURED`. With API authentication on, run `kubectl -n capll-system get deploy,secret` instead.

2. Check that the Secret has its keys.

   ```bash
   kubectl describe secret launchlayer-credentials -n capll-system
   ```

   The `Data` section lists `clientID`, `clientSecret` and `issuerURL` with a non-zero size, and `scopes`. The command does not print the values.

## Check a workload cluster's LaunchLayer resources

After you create a cluster, NVKE creates a `LaunchLayerCluster` named after the workload cluster and a `LaunchLayerMachine` for each node, named after the node, in the `nvke-system` namespace.

1. Check the cluster.

   ```bash
   kubectl -n nvke-system describe launchlayercluster <WORKLOAD_CLUSTER>
   ```

   The `VPCReady` condition is `True`, and so is `SecurityGroupsReady` when the cluster declares security groups. The reason `WaitingForEndpointConfig` means CAPLL cannot read the `launchlayer-credentials` Secret or has no `--base-url`.

2. Check the machines.

   ```bash
   kubectl -n nvke-system get launchlayermachines -l cluster.x-k8s.io/cluster-name=<WORKLOAD_CLUSTER>
   ```

   For a machine that does not come up, `kubectl -n nvke-system describe launchlayermachine <NODE_NAME>` shows why in its `MachineProvisioned` condition.

## Next steps

- [Secure the NVKE API](api-authentication.md).
- [Create a workload cluster](../workload-clusters/create.md) with the LaunchLayer identifiers you collected.
