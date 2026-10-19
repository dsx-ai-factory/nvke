# Plan your installation

> **Preview:** NVKE is available as a preview. APIs, configuration and behavior can change between releases, and we do not recommend NVKE for production workloads. See [Preview status and known limitations](../overview/preview-status.md).

This page describes what you need before you install NVIDIA Kubernetes Engine (NVKE) on your own hardware, which settings cannot change later, and which values to collect. Read it before every other installation page.

## Before you begin

- You have read [What is NVKE](../overview/what-is-nvke.md).
- You have checked your hardware and versions against [Supported platforms and versions](../overview/supported-platforms.md).
- You have read [Preview status and known limitations](../overview/preview-status.md). Two limitations shape the plan: the management cluster does not upgrade, scale or replace its own nodes, and NVIDIA Infra Controller (NICo) is the default infrastructure provider, with LaunchLayer available only once a deployment enables it.

## What you end up with

A management cluster with an odd number of nodes, each a control-plane node that also runs the NVKE management plane. The management plane requests machines from NICo through CAPNICo, the Cluster API infrastructure provider for NICo, and forms workload clusters on them for your customers. You get there in seven steps:

| Step | What you do | Page |
|---|---|---|
| 1. Build | Build the NVKE controller image and the node OS image with NVIDIA Kubernetes Recipes (NVKR), and package the releases. | [Get NVKE artifacts from NVKR and write release manifests](build-node-images.md) |
| 2. Prepare nodes | Boot the management cluster's nodes from the node image with cloud-init user data, and record each node's TPM thumbprint. | [Prepare nodes](prepare-nodes.md) |
| 3. Bootstrap | Run `nvkectl bootstrap site` to form the management cluster. | [Bootstrap the management cluster](bootstrap-management-cluster.md) |
| 4. Install | Install Cluster API, CAPNICo, SPIRE, telemetry and the NVKE API and controllers. | [Install management-plane services](install-management-plane-services.md) |
| 5. Configure NICo | Confirm that CAPNICo is running, and collect the NICo identifiers each workload cluster needs. | [Configure an infrastructure provider: NICo](provider-nico.md) |
| 6. Secure the API | Make the NVKE API require a token from your identity provider. | [Secure the NVKE API](api-authentication.md) |
| 7. Create clusters | Create a workload cluster. | [Create a workload cluster](../workload-clusters/create.md) |

## What you need before you start

### Machines and storage

- **Management cluster nodes.** An odd number of machines, such as one, three or five, each with a TPM 2.0 device. Your provisioning system boots each from the node image with cloud-init user data. The node image uses DHCP, so reserve each node's address. Each node has more memory than the kubelet reserves for the system, 16 GiB by default.
- **A storage provisioner** for the management cluster's default StorageClass. The bootstrap installs none, and the management plane's database claims a 40 GiB volume.
- **Workload cluster machines** enrolled in NICo, which provisions them from your node image.

### NICo

- A site with machines enrolled.
- A virtual private cloud (VPC) in that site, with VPC prefixes or subnets for the nodes.
- One or more instance types, each with an allocation for your organization. An instance type with no allocation provisions nothing.
- An SSH key group, if you want SSH access to nodes.
- The NICo API endpoint, your organization ID, an API token and the API name your NICo deployment serves.

### Network

- **A Kubernetes API virtual IP address (VIP)** for the management cluster when it has more than one node, and for each workload cluster with more than one control-plane node. Each VIP is an unused, routable IPv4 address outside the cluster's pod and service networks. A single-node management cluster uses its node's IP instead.
- **Border Gateway Protocol (BGP) peering for each VIP.** Every control-plane node advertises the VIP from local autonomous system (AS) 65100 to the node's default gateway, with peer AS 4294967000. The local AS is fixed. The peer AS has two exceptions, both for workload clusters: a control plane that uses VPC prefix IDs reads its peer AS from NICo's metadata service on each node, and setting `KUBE_VIP_FMDS_ENDPOINT` in the management plane's `env` makes every workload cluster read it from that URL. Your network must accept the route and carry it to the cluster's API callers.
- **Pod and service networks** for each cluster that do not overlap each other or your node network.
- **Outbound access** from the bootstrap host and the management cluster to the registries and chart repositories listed in [Install management-plane services](install-management-plane-services.md).

### Ports

All connections are TCP; traffic between the nodes of one cluster is not listed. The management address is the management cluster's API VIP, or the node IP of a single-node management cluster.

| Source | Destination | Port | Purpose |
|---|---|---|---|
| Bootstrap host | Each management cluster node | 15005 | `nvke-agent`, which the bootstrap drives. |
| Each management cluster node | Bootstrap host | 30081 | SPIRE server, for TPM node attestation during the bootstrap. |
| Bootstrap host and install host | Management cluster API address | 6443 | Kubernetes API. |
| Each control-plane node with a VIP | The node's default gateway | 179 | BGP, to advertise the VIP. |
| API callers | Management address | 30080 | NVKE API, plain HTTP. |
| Workload cluster nodes | Management address | 30440 | SPIRE server, for node attestation. |
| Management cluster | Each workload cluster node | 15005 | `nvke-agent`, which the management plane drives. |
| Management cluster | Each workload cluster API address | 6443 | Cluster lifecycle operations. |
| Management cluster | NICo API endpoint | The port in the endpoint URL | Requesting machines. |
| Management cluster | Your identity provider's signing-key URL | The port in that URL | Fetching token signing keys, if you secure the API. |
| Customers | Their workload cluster's API address | 6443 | Kubernetes API. |

### Identity and trust

- **A SPIFFE trust domain**, such as `mgmt.example.com`.
- **Your own SPIRE certificate authority (CA)**: a certificate and key as files. The certificate goes into every management node's user data; both files go on the bootstrap host. Management nodes built from the node image cannot attest without it. The install also makes it the upstream authority of the management cluster's SPIRE server, which workload cluster nodes trust through the bundle NVKE puts in their user data.

### Registries

- **A container registry you control**, with an account that can push to it, for the controller image and the profile content image. If pulling needs credentials, the management cluster needs them too.
- CAPNICo's image is public on `ghcr.io`, or comes from your NVKR release, so it needs no credentials.

### Hosts and tools

| Host | Needs |
|---|---|
| NVKR build host | Linux AMD64 with Docker, Git, GNU Make, `jq` and the Go version NVKR pins. For the node OS image, also Kernel-based Virtual Machine (KVM) access, 8 CPUs, 10 GiB of memory and 64 GiB of free disk. |
| Bootstrap host | Linux, run as root, with rootful `nerdctl` and containerd, `kind`, `helm`, `kubectl`, `clusterctl`, Go and GNU Make. Docker is not supported. It can be a management cluster node. |
| Install host | `nvkectl`, `helm` v4.2.4, `kubectl`, `clusterctl` v1.13.6, `curl`, `python3` and `git`. It can be the bootstrap host. |
| API workstation | `nvkectl`, `curl` and `jq`; with API authentication, also the `kubectl oidc-login` plugin and `python3`. |

### Identity provider

API authentication is off by default: any caller that reaches port 30080 can call every endpoint. To secure the API, you need an OpenID Connect (OIDC) identity provider that:

- Publishes an OIDC discovery document over HTTPS, with a signing-key endpoint the management cluster can reach.
- Signs tokens with an asymmetric algorithm, such as `RS256` or `ES256`.
- Puts group membership in a token claim named `groups`.
- Has an OIDC client for `nvkectl` that needs no client secret.

NVKE admits callers by group but does no authorization: every admitted caller can call every endpoint.

## Decide before you install

| Setting | Fixed at | What to decide |
|---|---|---|
| Management cluster node count | Bootstrap | An odd number. The management cluster cannot add or replace nodes, so size it for the life of the installation. |
| Management cluster etcd encryption | Bootstrap | On by default. A management cluster bootstrapped without it stays unencrypted. |
| Management cluster API VIP | Bootstrap | Required for more than one node. |
| SPIFFE trust domain | Node preparation | One value in the node user data, the site spec and the install values. If the site spec leaves it unset, `nvkectl` uses `nvkectl-bootstrap.nke.local`, which does not match your nodes. |
| Node CPU architecture | Image build | The node image and the SPIRE TPM attestor checksum are per architecture. |
| Workload cluster Container Network Interface (CNI) plugin and networks | Cluster creation | `ovn-kubernetes`. Pod and service ranges default to `192.168.0.0/16` and `10.96.0.0/12`. |
| Workload cluster etcd encryption | Cluster creation | `etcdEncrypted` has no default. You can turn encryption on later, never off. |
| Workload cluster API VIP | Cluster creation | Without one, the control plane has one node that you cannot scale, upgrade or replace. Adding a VIP means recreating the cluster. |
| NICo identifiers | Cluster or node pool creation | A cluster's `orgID`, `siteID` and `vpcID`, and a node pool's `instanceTypeID`, InfiniBand partition and NVLink logical partition. |
| Releases | When applied | `MachineImage`, `ClusterRelease` and `NodePoolRelease` resources are immutable. To change an image or its user data, publish a release with a new name. |

## Fill in your values

Each placeholder has the same name on the page that uses it. Keep credentials in your secret store, not in this list.

### Management cluster

| Value | Placeholder or field | Used on | Your value |
|---|---|---|---|
| Cluster name, a lowercase RFC 1123 label | `<CLUSTER_NAME>` | Bootstrap, Install, Secure the API, Observe | |
| Each node's name, IP address and TPM endorsement-key (EK) thumbprint | `hosts.csv` columns `name`, `ip`, `tpm_ek_thumbprint`; `<CP_01_EK_THUMBPRINT>` and so on | Prepare nodes, Bootstrap | |
| Kubernetes version, without a leading `v` | `<K8S_VERSION>` | Prepare nodes, Bootstrap | |
| Kubernetes API VIP | `<API_VIP>` | Bootstrap | |
| Management address | `<MANAGEMENT_ADDRESS>` | Install, Secure the API, Create, Manage node pools, Observe | |
| Pod and service networks | `<POD_CIDR>`, `<SERVICE_CIDR>` | Bootstrap | |
| Memory reserved for the system | `<RESERVED_MEMORY>` | Bootstrap | |
| Address the nodes use to reach the bootstrap host | `<BOOTSTRAP_HOST_IP>` | Prepare nodes | |
| SPIFFE trust domain | `<TRUST_DOMAIN>` | Prepare nodes, Bootstrap, Install | |
| SPIRE CA certificate and key files, passed as `NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE` with `spire.upstreamCA: true` in the site spec; the certificate in PEM | `<CA_CERT_PATH>`, `<CA_KEY_PATH>`; `<SPIRE_CA_CERT_PEM>` | Bootstrap, Install; Prepare nodes | |
| CPU architecture of the management nodes, `amd64` or `arm64` | `<ARCH>` | Install | |

### Registries and images

| Value | Placeholder | Used on | Your value |
|---|---|---|---|
| Registry host and namespace | `<REGISTRY_HOST>`, `<NAMESPACE>` | Build, Bootstrap, Install | |
| Registry account that can push | `<REGISTRY_USER>`, `<REGISTRY_TOKEN>` | Build | |
| NVKR release reference, and its exported description `release.json` | `<RELEASE_REF>` | Build, Install | |
| Controller image, the release's `cwe` artifact | `<CONTROLLER_IMAGE>` | Bootstrap; Install without a release | |
| Provisioned infrastructure provider image | `<PROVISIONED_PROVIDER_IMAGE>` | Bootstrap | |
| Profile content image, the release's `controller-profiles` artifact | `images.profiles` in the stack config | Install without a release | |
| OS release resources, `MachineImage`, `ClusterRelease` and `NodePoolRelease` | `releases/` | Build, Install | |
| For `nvkr release verify`: the accepted signer identity and OIDC issuer, and a `cosign` path and digest | `<IDENTITY_REGEXP>`, `<OIDC_ISSUER>`, `<COSIGN_PATH>`, `<COSIGN_DIGEST>` | Build | |

### NICo

| Value | Placeholder | Used on | Your value |
|---|---|---|---|
| API endpoint | `<NICO_ENDPOINT>` | Install, Configure NICo | |
| Organization ID | `<NICO_ORG_ID>` | Install, Configure NICo, Create | |
| API token | `NVKECTL_NICO_TOKEN` | Install | |
| API name | `<NICO_API_NAME>` | Install, Configure NICo | |
| Site ID and VPC ID | `<NICO_SITE_ID>`, `<NICO_VPC_ID>` | Create | |
| Instance type IDs for control-plane and worker nodes | `<CP_INSTANCE_TYPE_ID>`, `<WORKER_INSTANCE_TYPE_ID>` | Create, Manage node pools | |
| VPC prefix IDs, or subnet IDs | `<VPC_PREFIX_ID>` | Create, Manage node pools | |
| SSH key group ID, optional | `<SSH_KEY_GROUP_ID>` | Create | |

### Management plane and identity provider

| Value | Placeholder | Used on | Your value |
|---|---|---|---|
| Login account written into every workload cluster node's user data, and its password | `NVKECTL_NODE_USER`, `NVKECTL_NODE_PASSWORD` | Install | |
| Database password, chosen once and kept in your secret manager | `NVKECTL_DATABASE_PASSWORD` | Install | |
| Issuer URL | `<ISSUER_URL>` | Secure the API | |
| Signing-key URL | `<JWKS_URI>` | Secure the API | |
| OIDC client ID for `nvkectl` | `<CLIENT_ID>` | Secure the API | |
| Token audience | `<AUDIENCE>` | Secure the API | |
| Group allowed to call the API | `<ADMITTED_GROUP>` | Secure the API | |
| Extra scopes your IdP needs to release the `groups` claim, if any | `<SCOPE>` | Secure the API | |
| NVKE API base address, `http://<MANAGEMENT_ADDRESS>:30080` | `<API_URL>` | Secure the API, Manage node pools | |
| A JWT from your IdP, when API authentication is on | `<TOKEN>` | Create, Manage node pools, Observe, Troubleshoot | |

### Each workload cluster

The workload cluster pages name each cluster `<WORKLOAD_CLUSTER>`. [Create a workload cluster](../workload-clusters/create.md) reuses `<K8S_VERSION>` and `<API_VIP>`; record them separately from the management cluster's.

| Value | Placeholder or field | Your value |
|---|---|---|
| Cluster name, a lowercase DNS label of at most 55 characters | `<WORKLOAD_CLUSTER>` | |
| Kubernetes version | `<K8S_VERSION>` | |
| Kubernetes API VIP | `<API_VIP>` | |
| Pod and service networks, if not the defaults | `network.podCIDR`, `network.serviceCIDR` | |
| CNI | `cniPlugin` | |
| etcd encryption | `etcdEncrypted` | |
| Releases for the control plane and the workers | `<CP_RELEASE>`, `<WORKER_RELEASE>` | |
| Each node pool's name | `<POOL_NAME>` | |

## Verify

Your plan is complete when every row in [Fill in your values](#fill-in-your-values) that applies to you has a value, every connection in [Ports](#ports) is open, and you have decided each setting in [Decide before you install](#decide-before-you-install).

## Next steps

- [Get NVKE artifacts from NVKR and write release manifests](build-node-images.md).
