# Preview status and known limitations

NVIDIA Kubernetes Engine (NVKE) is available as a preview. APIs, configuration and behavior can change between releases, and NVKE is not recommended for production workloads. This page lists what the preview supports and every known limitation. For versions and platforms, see [Supported platforms and versions](supported-platforms.md).

## Supported at preview

- Build node OS images and NVKE components with NVIDIA Kubernetes Recipes (NVKR): [Get NVKE artifacts and OS profiles built with NVKR](../operate/build-node-images.md).
- Prepare nodes and bootstrap a management cluster from them with `nvkectl`: [Prepare nodes](../operate/prepare-nodes.md) and [Bootstrap the management cluster](../operate/bootstrap-management-cluster.md).
- Install the management plane on the management cluster: [Install management-plane services](../operate/install-management-plane-services.md).
- Provision workload cluster nodes through NVIDIA Infra Controller (NICo): [Configure an infrastructure provider: NICo](../operate/provider-nico.md).
- Authenticate NVKE API callers with JWTs from your identity provider: [Secure the NVKE API](../operate/api-authentication.md).
- Create, change and delete workload clusters with `ovn-kubernetes` networking and optional etcd encryption: [Create a workload cluster](../workload-clusters/create.md).
- Add, scale and delete node pools, replace nodes, and mark outdated nodes for recycling with auto mode: [Manage node pools](../workload-clusters/node-pools.md).
- Hand a workload cluster to a customer through its admin kubeconfig: [Access a workload cluster](../workload-clusters/access.md).
- Check health and collect telemetry with NVIDIA Kubernetes Telemetry Pipeline (NKTP): [Observe NVKE and its clusters](../operate/observe.md).
- Evaluate NVKE on one Linux machine with simulated NICo: [Quickstart: evaluate locally with mock-NICo](../get-started/quickstart-local.md).

## Known limitations

### Management cluster

- **The management cluster does not manage itself.** It does not upgrade, scale or replace its own nodes.
- **No management-plane upgrade procedure.** NVKE provides no procedure to move the management-plane services to a later NVKE release.
- **No backup or restore procedure.** NVKE provides no procedure to back up or restore the management cluster or the management plane's state. Keep the kubeconfig and etcd encryption key file from the bootstrap in secure storage: the key file is the only copy of the management cluster's etcd encryption key that is not on a node.
- **Management cluster nodes built from the node image need your own SPIRE certificate authority (CA).** Without your CA files at bootstrap, SPIRE uses a self-signed CA that no node can trust in advance, so the nodes cannot attest and cannot join. Supply your CA files as described in [Prepare nodes](../operate/prepare-nodes.md). Workload cluster nodes do not use this CA: NVKE puts the management cluster's SPIRE trust bundle in their user data, as described in [Get NVKE artifacts and OS profiles built with NVKR](../operate/build-node-images.md#what-nvke-puts-in-the-user-data).
- **NICo is the only infrastructure provider.** NVKE provisions workload cluster nodes only through NICo.

### API and access

- **The API serves plain HTTP after install, with authentication off.** The NVKE API listens on port 30080 of every management node without TLS, and any caller in a network you admit to it can call every endpoint. Admit only the networks that need the API, as described in [Install management-plane services](../operate/install-management-plane-services.md), and turn on authentication as described in [Secure the NVKE API](../operate/api-authentication.md). Tokens sent to the port cross the network unencrypted, so put a TLS endpoint of your own in front of it or keep its clients on a trusted, isolated network.
- **NVKE does no authorization.** Every caller that API authentication admits can call every endpoint, including the ones that create and delete workload clusters. Issue tokens for the NVKE API only to people and systems that may manage every workload cluster.
- **`nvkectl` needs an OIDC kubeconfig once authentication is configured.** With the kubeconfig from the bootstrap, `nvkectl` requests a token for the service account `nke-clients/nke-client`, which the install does not create, so every API call fails. Generate an OpenID Connect (OIDC) kubeconfig with `nvkectl generate oidc-kubeconfig`, as described in [Secure the NVKE API](../operate/api-authentication.md). With that kubeconfig, the commands that read the management cluster's Kubernetes API directly fail, because its API server does not accept the kubeconfig's tokens: `nvkectl get kubeconfig`, `nvkectl get providers`, and the machine image, cluster release and node pool release commands. Use `kubectl` with the management cluster's admin kubeconfig for them.
- **Customers get only the admin kubeconfig.** NVKE issues no per-user credentials to customers, and `enableOidc` does not connect a customer's identity provider to a workload cluster. The customer's administrators create narrower credentials inside the cluster. See [Access a workload cluster](../workload-clusters/access.md).
- **A handed-over admin kubeconfig cannot be revoked.** NVKE does not rotate a workload cluster's certificate authority, so a copy stays valid until its client certificate expires. Hand it only to the people who administer the cluster for the customer.

### Workload clusters and node pools

- **Node labels and taints are not applied.** NVKE stores a node pool's `nodeLabels` and `nodeTaints`, and the control plane's `controlPlaneConfig.nodeLabels`, but does not apply them to the Kubernetes nodes. Label and taint the nodes yourself with `kubectl` in the workload cluster, including nodes that NVKE adds later.

### Building artifacts

- **No NVKR Release is qualified, and local builds are unsigned.** A Run on your own host is in the `unqualified-local-execution` evidence lane, with no signatures or build provenance. Only NVKR's hosted pipeline produces `pipeline-attested` Releases, which carry signatures and build provenance, and NVKR qualifies none of them. See [Get NVKE artifacts and OS profiles built with NVKR](../operate/build-node-images.md#what-a-release-proves).
- **NVKR's maintained Release lacks two images the install needs.** The `deploy/nico` deploy path names the profile content image `controller-profiles` and the CAPNICo image `capnico-controller`, which NVKR's maintained M2 Definition does not build, so the install refuses a Release built from it. Install without `--release` and name the images in the site spec, as described in [Get NVKE artifacts and OS profiles built with NVKR](../operate/build-node-images.md#hand-the-release-to-the-install).
- **No reference iPXE script boots the NVKR node image through NICo.** To provision workload cluster nodes from the image, an OS profile's `ipxeScript` must write the image, a QEMU copy-on-write (QCOW2) file, to the machine's disk with a loader your NICo site provides. The example OS profile installs Ubuntu 24.04 from the installer ISO instead. See [Get NVKE artifacts and OS profiles built with NVKR](../operate/build-node-images.md).

### Telemetry

- **The default NKTP backend is for evaluation only.** It keeps data in memory and gives anonymous users admin access to Grafana. Point NKTP at your own backend for anything else, as described in [Observe NVKE and its clusters](../operate/observe.md).
