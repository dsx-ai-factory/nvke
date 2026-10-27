# Preview status and known limitations

> **Preview:** NVKE is available as a preview. APIs, configuration and behavior can change between releases, and we do not recommend NVKE for production workloads.

This page lists what the NVIDIA Kubernetes Engine (NVKE) preview supports and every known limitation. For versions and platforms, see [Supported platforms and versions](supported-platforms.md).

## Supported at preview

- Build reference node OS images with the repository Make targets and serve the boot artifacts over HTTP(S): [Build and host node OS images](../operate/build-node-images.md).
- Prepare nodes and bootstrap a management cluster from them with `nvkectl`: [Prepare nodes](../operate/prepare-nodes.md) and [Bootstrap the management cluster](../operate/bootstrap-management-cluster.md).
- Install the management plane on the management cluster: [Install the management plane](../operate/install-management-plane.md).
- Provision workload cluster nodes through NVIDIA Infra Controller (NICo): [Configure an infrastructure provider: NICo](../operate/provider-nico.md).
- Authenticate NVKE API callers with JWTs from your identity provider: [Secure the NVKE API](../operate/api-authentication.md).
- Create, change and delete workload clusters with `ovn-kubernetes` networking and optional etcd encryption: [Create a workload cluster](../workload-clusters/create.md).
- Add, scale and delete node pools, and replace nodes: [Manage node pools](../workload-clusters/node-pools.md).
- Hand a workload cluster to a customer through its admin kubeconfig: [Access a workload cluster](../workload-clusters/access.md).
- Check health and collect telemetry with NVKE telemetry pipeline (NKTP): [Observe NVKE and its clusters](../operate/observe.md).
- Evaluate NVKE on one Linux machine with simulated NICo: [Quickstart: evaluate locally with mock-NICo](../get-started/quickstart-local.md).

## Known limitations

### Management cluster

- **The management cluster does not manage itself.** It does not upgrade, scale or replace its own nodes.
- **No management-plane upgrade procedure.** NVKE provides no procedure to move the management-plane services to a later NVKE release.
- **No backup or restore procedure.** NVKE provides no procedure to back up or restore the management cluster or the management plane's state. Keep the kubeconfig and etcd encryption key file from the bootstrap in secure storage: the key file is the only copy of the management cluster's etcd encryption key that is not on a node.
- **Management cluster nodes built from the node image need your own SPIRE certificate authority (CA).** Without your CA files at bootstrap, SPIRE uses a self-signed CA that no node can trust in advance, so the nodes cannot attest and cannot join. Supply your CA files as described in [Prepare nodes](../operate/prepare-nodes.md). Workload cluster nodes do not use this CA: NVKE puts the management cluster's SPIRE trust bundle in their user data, as described in [Register node images for provisioning](../operate/build-node-images.md#what-nvke-puts-in-the-user-data).
- **NICo is the default infrastructure provider.** LaunchLayer is also supported once a deployment enables it; see [Infrastructure Providers](../site-bootstrap/infrastructure-providers.md#launchlayer).

### API and access

- **The API serves plain HTTP after install, with authentication off.** The NVKE API listens on port 30080 of every management node without TLS, and any caller in a network you admit to it can call every endpoint. Admit only the networks that need the API, as described in [Install the management plane](../operate/install-management-plane.md#admit-your-networks-to-the-nvke-api), and turn on authentication as described in [Secure the NVKE API](../operate/api-authentication.md). Tokens sent to the port cross the network unencrypted, so put a TLS endpoint of your own in front of it or keep its clients on a trusted, isolated network.
- **NVKE does no authorization.** Every caller that API authentication admits can call every endpoint, including the ones that create and delete workload clusters. Issue tokens for the NVKE API only to people and systems that may manage every workload cluster.
- **`nvkectl` needs an OIDC kubeconfig once authentication is configured.** With the kubeconfig from the bootstrap, `nvkectl` requests a token for the service account `nke-clients/nke-client`, which the install does not create, so every API call fails. Generate an OpenID Connect (OIDC) kubeconfig with `nvkectl generate oidc-kubeconfig`, as described in [Secure the NVKE API](../operate/api-authentication.md). With that kubeconfig, the commands that read the management cluster's Kubernetes API directly fail, because its API server does not accept the kubeconfig's tokens: `nvkectl get kubeconfig`, `nvkectl get providers`, and the machine image, cluster release and node pool release commands. Use `kubectl` with the management cluster's admin kubeconfig for them.
- **Customers get only the admin kubeconfig.** NVKE issues no per-user credentials to customers, and `enableOidc` does not connect a customer's identity provider to a workload cluster. The customer's administrators create narrower credentials inside the cluster. See [Access a workload cluster](../workload-clusters/access.md).
- **A handed-over admin kubeconfig cannot be revoked.** NVKE does not rotate a workload cluster's certificate authority, so a copy stays valid until its client certificate expires. Hand it only to the people who administer the cluster for the customer.

### Workload clusters and node pools

- **Changing a node pool's configuration does not replace its running nodes.** Only nodes created after the change use the new configuration. With `recycleBudget` above `0`, auto mode marks the outdated nodes with the condition `nvke.nvidia.com/NeedsRecycle`, but NVKE does not drain or delete them, and `nvkectl` cannot set the auto-mode fields or show which nodes are marked. Delete outdated nodes yourself, or with a break-fix system you configure to act on the condition, as described in [Manage node pools](../workload-clusters/node-pools.md#control-automatic-node-replacement).
- **Node labels and taints are fixed at creation and applied only when a node joins.** You cannot update a node pool's `nodeLabels` or `nodeTaints`, or the control plane's `controlPlaneConfig.nodeLabels`, after you create them. NVKE applies them once, after each node joins the cluster. If a label or taint is later removed from a node, it is not added back. To change them on existing nodes, use `kubectl label` and `kubectl taint`.
- **`nvkectl` upgrades only the control plane.** `nvkectl patch cluster` changes `controlPlaneKubernetesVersion`, but `nvkectl patch nodepool` accepts only `desiredNodeCount`. To change a node pool's Kubernetes version, send `kubernetesVersion` in an update request to `/api/nodePools/<NODE_POOL_ID>`. Nodes built before the change become outdated: auto mode marks them for replacement within the pool's `recycleBudget`, and NVSentinel or you delete them, as described in [Manage node pools](../workload-clusters/node-pools.md#control-automatic-node-replacement).

### Building artifacts

- **A local image build is not a qualified release.** The Make build and smoke test do not validate a real NICo boot, disk selection or the complete first-boot configuration. Validate the image and imager together on your target hardware before using them. See [Build and host node OS images](../operate/build-node-images.md#build-the-node-os-image).
- **The install needs controller and profile image references.** For an install without a release description, stage them on the management nodes and name them in the stack config as described in [Install the management plane](../operate/install-management-plane.md#stage-the-controller-images-on-the-management-nodes).
- **QCOW2 provisioning needs a site-specific imager and script.** To provision workload cluster nodes from the image, the `MachineImage.spec.nico.ipxeScript` must chainload an EFI imager that downloads the QEMU copy-on-write (QCOW2) file and writes it to the machine's disk. The repository includes an end-to-end iPXE test template, but it does not provide a complete, validated customer provisioning procedure for this image. Use an imager and script validated together for your NICo site. The example MachineImage installs Ubuntu 24.04 from the installer ISO instead. See [Build and host node OS images](../operate/build-node-images.md).
- **FDE node images do not protect data at rest.** NVKR's hosted pipeline is configured to build a full-disk encryption (FDE) variant of the node image for amd64 and arm64, with the root volume on LUKS2. Each image build generates a new volume encryption key, and every node built from that image inherits the same key. On first boot the node sets up TPM-based unlocking and removes the bootstrap passphrase, a public, documented value, but it does not generate a per-node key or re-encrypt the volume. Anyone with the original image and the bootstrap passphrase can recover the key that decrypts every node built from it, even after TPM enrollment. The passphrase also stays in the node's GRUB configuration as a `luks.key=` kernel parameter after the bind. Use FDE images for evaluation only, and do not rely on them for data-at-rest confidentiality.

### Telemetry

- **The default NKTP backend is for evaluation only.** It keeps data in memory and gives anonymous users admin access to Grafana. Point NKTP at your own backend for anything else, as described in [Observe NVKE and its clusters](../operate/observe.md).
