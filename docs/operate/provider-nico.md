# Configure an infrastructure provider: NICo

This page describes how to confirm that the NVIDIA Kubernetes Engine (NVKE) management plane can request bare-metal machines from NVIDIA Infra Controller (NICo), how to manage the credentials it uses, and which NICo identifiers each workload cluster request needs. NVKE requests machines through CAPNICo, the Cluster API (CAPI) infrastructure provider for NICo, which runs on the management cluster and calls the NICo API with your credentials. You do this after you install the management-plane services and before you create the first workload cluster.

NICo is NVKE's default infrastructure provider; LaunchLayer is also supported once a deployment enables it, as described in [Infrastructure Providers](../site-bootstrap/infrastructure-providers.md#launchlayer). See also [Preview status and known limitations](../overview/preview-status.md).

## Before you begin

- You have completed [Install the management plane](install-management-plane.md). That procedure installs CAPNICo in the `capnico-system` namespace and writes your NICo API details from the `nico` section of the stack config to the `nico-credentials` Secret, before it installs the management plane.
- You have the management cluster kubeconfig, `<CLUSTER_NAME>.kubeconfig`, and `kubectl` on `PATH`. The kubeconfig must be able to read Secrets in the `capnico-system` namespace.
- `nvkectl` points at your management plane, as set in [Install the management plane](install-management-plane.md) with `nvkectl config set-site`. If API authentication is on, `nvkectl` uses an OpenID Connect (OIDC) kubeconfig, as described in [Secure the NVKE API](api-authentication.md), and `nvkectl get providers` fails with it, because it reads the management cluster's Kubernetes API, which does not accept your identity provider's tokens. The steps that use it give a `kubectl` alternative.
- In NICo, you have:
  - A site, with machines enrolled.
  - A virtual private cloud (VPC) in that site, and either VPC prefixes or subnets in it for the nodes.
  - One or more instance types, each with an allocation for your organization. An instance type with no allocation looks healthy and provisions nothing: machines wait with the reason `InstanceTypeUnavailable`.
  - An SSH key group for node access, if you want SSH access to nodes.

Run the commands on this page with `KUBECONFIG` set:

```bash
export KUBECONFIG=$PWD/<CLUSTER_NAME>.kubeconfig
```

## Confirm that CAPNICo is running

1. Check the provider state.

   ```bash
   nvkectl get providers
   ```

   The `nico` row reads `CONFIGURED`: the CAPNICo custom resource definitions (CRDs) are established, the CAPNICo controller is available and the `nico-credentials` Secret exists. `CREDENTIALS` reads `UNSUPPORTED`; see the note after these steps. `NOT-INSTALLED` or `NOT-CONFIGURED` means the install did not complete; run it again as described in [Run NVKE end to end on one machine](../get-started/local-site.md). `DEGRADED` means the CAPNICo controller is not available; check the `controller-manager` Deployment in `capnico-system`. `UNKNOWN` usually means the kubeconfig that `nvkectl` uses cannot read Secrets in `capnico-system`.

   With API authentication on, this command fails. Check CAPNICo with `kubectl` instead:

   ```bash
   kubectl -n capnico-system get deploy,secret
   ```

   The Deployment `controller-manager` shows all replicas ready, and the Secret `nico-credentials` is listed.

2. Check that the management plane started its NICo controllers.

   ```bash
   kubectl -n nvke-controller get deploy nvke-controller
   ```

   The Deployment is ready. This install enables the NICo controllers, and the process exits at startup when the `NicoMachine` CRD is missing, so a ready Deployment means those controllers started.

   If it is not ready, the pod state and log name the failure. When the log is the `NicoMachine` exit, install CAPNICo or set `controllers.nico.enabled` to `false`. The pod retries on its own.

`CONFIGURED` does not mean the credentials work. The `CREDENTIALS` column of `nvkectl get providers` reports whether they do, from the check CAPNICo runs on them every few minutes, but only with a CAPNICo release that serves `NicoIdentity` and a `NicoIdentity` that names `nico-credentials`. The CAPNICo release this procedure installs does not serve `NicoIdentity`, so `CREDENTIALS` reads `UNSUPPORTED`. CAPNICo still checks the token against the NICo API, but only when it reconciles a `NicoCluster` that belongs to a Cluster API `Cluster`, which normally first happens when you create a workload cluster. [Infrastructure providers](https://github.com/dsx-ai-factory/nvke/blob/main/cli/README.md#infrastructure-providers) lists every `CREDENTIALS` value.

## Manage the NICo credentials

CAPNICo authenticates to the NICo API with a bearer token from the Secret `nico-credentials` in the `capnico-system` namespace. The name, the namespace and the four keys are fixed.

| Key | Value | Where it comes from |
|---|---|---|
| `endpoint` | The NICo API base URL. | `nico.endpoint` in the stack config |
| `orgID` | Your NICo organization ID. | `nico.orgID` in the stack config |
| `token` | Your NICo API token. | `NVKECTL_NICO_TOKEN` in the install's environment. |
| `apiName` | The API name in the NICo API path that your NICo deployment serves, such as `forge`. | `nico.apiName` in the stack config |

To rotate the token or change any other value:

1. Write the Secret to a file named `nico-credentials.yaml`.

   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: nico-credentials
     namespace: capnico-system
   type: Opaque
   stringData:
     endpoint: "<NICO_ENDPOINT>"
     orgID: "<NICO_ORG_ID>"
     token: "REDACTED"
     apiName: "<NICO_API_NAME>"
   ```

   Set all four keys. Replace `<NICO_ENDPOINT>`, `<NICO_ORG_ID>` and `<NICO_API_NAME>` with the values described in the table above, and `REDACTED` with the new token.

2. Apply the Secret, then delete the file.

   ```bash
   kubectl apply --server-side -f nico-credentials.yaml
   ```

   ```bash
   rm nico-credentials.yaml
   ```

   The Secret is read on each reconcile, so the change takes effect without a restart.

3. Keep the install's inputs in step: set `NVKECTL_NICO_TOKEN` to the new token wherever you run the install, and update `endpoint`, `orgID` and `apiName` under `nico` in the stack config if they changed. If you run the install again, it writes the Secret from those inputs and overwrites your change.

## Collect the NICo identifiers

The management plane holds no site-wide NICo settings. Each workload cluster request carries the NICo identifiers it uses, in a `nico` object on the cluster, the control plane and each node pool. Record these values from NICo before you create a cluster:

| Field | Where it goes | Value |
|---|---|---|
| `orgID` | Cluster `nico` | Your NICo organization ID. |
| `siteID` | Cluster `nico` | The NICo site ID, a UUID. |
| `vpcID` | Cluster `nico` | The NICo VPC ID, a UUID. |
| `instanceTypePreference` | Control plane `nico` | An ordered list of instance type IDs. The first one that has capacity is used. |
| `instanceTypeID` | Node pool `nico` | One instance type ID. Fixed when the node pool is created. |
| `vpcPrefixIDs` or `subnetIDs` | Control plane and node pool `nico` | Exactly one of the two: VPC prefixes for a site with native networking, subnets otherwise. |
| `sshKeyGroupIDs` | Control plane and node pool `nico` | Optional. SSH key groups for node access. |
| `infinibandPartitionID`, `nvLinkLogicalPartitionID` | Node pool `nico` | Optional. An InfiniBand partition and an NVLink logical partition. Fixed when the node pool is created. |

The `orgID`, `siteID` and `vpcID` of a cluster are fixed when the cluster is created. The control plane and each node pool select one of the releases you applied in [Run NVKE end to end on one machine](../get-started/local-site.md) with `nico.release` and, optionally, a config profile with `nico.configProfile`. [Create a workload cluster](../workload-clusters/create.md) shows a full request.

## Verify

1. Check the provider state.

   ```bash
   nvkectl get providers
   ```

   The `nico` row reads `CONFIGURED`. With API authentication on, run `kubectl -n capnico-system get deploy,secret` instead, as in [Confirm that CAPNICo is running](#confirm-that-capnico-is-running).

2. Check that the Secret has all four keys.

   ```bash
   kubectl describe secret nico-credentials -n capnico-system
   ```

   The `Data` section lists `apiName`, `endpoint`, `orgID` and `token`, each with a non-zero size. The command does not print the values.

If `nvkectl get providers` reports another state, see [Troubleshoot](troubleshoot.md).

## Next steps

- [Secure the NVKE API](api-authentication.md).
- [Create a workload cluster](../workload-clusters/create.md) with the NICo identifiers you collected.
