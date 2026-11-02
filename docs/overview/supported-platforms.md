# Supported platforms and versions

This page lists the Kubernetes versions, networking, infrastructure providers, node operating system and tools that this release of NVIDIA Kubernetes Engine (NVKE) supports. Values change between releases; the values below are checked against this release's source. The node-image rows describe the repository's reference image build.

In the Notes column, **Pinned** means NVKE builds, installs or stages exactly that version, and other versions are not supported. **Range** means NVKE accepts any version within the stated bounds. For what preview does and does not support, see [Preview status and known limitations](preview-status.md).

## Kubernetes

| Item | Supported | Notes |
|---|---|---|
| Kubernetes version | 1.36.3 | Pinned. Release qualification targets 1.36 (`release/policy.yaml`). The node image also stages `nvke-kubeadm-distro` packages for 1.34.2, 1.35.3 and 1.37.2; those versions are not supported. Another patch release of 1.36 is not on the node. |
| Version format | `X.Y.Z`, with no leading `v` | The API and the site spec reject any other format. |
| Management cluster | 1.36.3 | Set once, in the site spec's `cluster.k8sVersion` field, at bootstrap. `nvkectl generate sitespec` writes 1.36.3 unless you set it. The management cluster does not upgrade itself. |
| Workload cluster | 1.36.3 | `controlPlaneKubernetesVersion` is required and has no default. The API accepts any `X.Y.Z`, but nodes run only a version their node image carries. |
| Control-plane upgrade | One minor version up, or a later patch release | Range. Downgrades and requests for the current version are rejected. The cluster must have a `controlPlaneVIP`, and only one upgrade runs at a time. |
| Worker node pool skew | Up to 3 minor versions behind the control plane | Range. Checked when you upgrade the control plane. A new node pool starts at the cluster's version. |
| Upgrade target on NICo | A registered release for the target version | The upgrade selects a release for the target Kubernetes version with the same architecture, variant and Ubuntu version as the current one. It fails if none is registered, unless the current release sets the Kubernetes version at provisioning time. |
| OpenID Connect (OIDC) on a workload cluster | 1.30.0 or later | Range. Applies to `enableOidc` and `nvkeOIDCAuth`. |
| Fleet-wide OIDC providers on a workload cluster | 1.34.0 or later | Range. Applies to `oidc.providers`. A cluster created on an earlier version gets none. |

### Kubernetes feature gates

NVKE runs every Kubernetes component with the upstream default feature gates for its Kubernetes version. On the supported version, NVKE sets no `--feature-gates` flag on `kube-apiserver`, `kube-controller-manager` or `kube-scheduler`, and no `featureGates` in the kubelet or kube-proxy configuration. The same applies to the management cluster and to workload clusters.

| Feature stage | State in NVKE | Notes |
|---|---|---|
| General availability (GA) | Enabled | Kubernetes locks most GA gates on, and removes them in a later release. |
| Beta | The upstream default | Most beta gates are on by default upstream, and some are off. NVKE does not change either. |
| Alpha | Disabled | Alpha gates are off by default upstream. NVKE does not enable any. |
| Beta API versions | The upstream default | NVKE does not set `--runtime-config`, so beta API versions that are off by default upstream stay off. |

The NVKE API and the site spec have no field to change a feature gate. The [Kubernetes feature gates reference](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/) lists each gate's stage and default for every Kubernetes version.

## Container networking

| Plugin or setting | Version | Notes |
|---|---|---|
| `ovn-kubernetes` | 2.4.2 | Pinned. The NVIDIA distribution of OVN-Kubernetes, built from `dsx-ai-factory/ovn-kubernetes`. It is the Container Network Interface (CNI) plugin for workload clusters and the management cluster. The node image stages its charts and images packages. Pod `hostPort` is enabled. |
| Other `cniVersion` values | Not in the node image | The node agent installs the CNI from packages on the node, so another version needs a node image built with it. |
| IP family | IPv4 | Pod and service CIDRs are private IPv4 ranges, fixed when the cluster is created: `/12` to `/23` for pods, `/12` to `/24` for services. The defaults are `192.168.0.0/16` and `10.96.0.0/12`. The API virtual IP address (VIP) is IPv4. |

## Infrastructure providers

| Provider | Version | Notes |
|---|---|---|
| NVIDIA Infra Controller (NICo), through CAPNICo | CAPNICo v0.0.45 | Pinned. The default infrastructure provider for workload clusters; LaunchLayer is also supported once enabled, see [Infrastructure Providers](../site-bootstrap/infrastructure-providers.md#launchlayer). The install deploys this CAPNICo release with the reboot and repair settings NVKE uses. The local quickstart builds a development CAPNICo version from source instead. |
| Cluster API core, kubeadm bootstrap and kubeadm control plane | v1.13.6 | Pinned. One version covers `clusterctl`, the providers that `clusterctl init` installs on the management cluster, and the providers `nvkectl` installs in the seed cluster. |
| cert-manager | The version Cluster API v1.13.6 installs | `clusterctl init` installs it. |
| Provisioned infrastructure provider | Built from the same NVKE source as `nvkectl` | Runs only in the seed cluster during bootstrap, to form the management cluster. |

## Node operating system

| Item | Supported | Notes |
|---|---|---|
| Operating system | Ubuntu 26.04 LTS or Ubuntu 24.04 LTS | No other release is supported. The reference Make build defaults to Ubuntu 26.04.1; set `UBUNTU_RELEASE` to select the installer point release. See the image build guide for release selection and verification. |
| CPU architectures | amd64, arm64 | Set `ARCH=amd64` or `ARCH=arm64` on the repository image Make target; `image-native` requires a matching Linux/KVM host. |
| Kernel | `linux-nvidia` on amd64, `linux-nvidia-64k` on arm64 | Ubuntu's NVIDIA-tuned kernel flavors. |
| Full-disk encryption (FDE) | Opt-in reference build, amd64 and arm64, for evaluation only | `VARIANT=fde`; follow the separate [build and verification instructions](../../os-image/README.md#full-disk-encryption-fde-variant). NVKR's hosted pipeline also selects FDE images for both architectures; its local `scripts/nvke-os-local.sh` path builds only non-FDE. Nodes built from one FDE image share its volume encryption key, so the image does not protect data at rest; see [Preview status and known limitations](preview-status.md#building-artifacts). |
| SPIFFE Runtime Environment (SPIRE) agent | 1.14.1, with TPM plugin 1.9.0 | Pinned. Installed on the node image for TPM node attestation. |
| Node agent (`nvke-agent`) | Built from the NVKE checkout used for the image | Installed and enabled on the node image. Listens on port 15005. |

[Build the OS image](../../os-image/README.md#build-the-os-image) lists the build host requirements and Make targets; [Build and host node OS images](../operate/build-node-images.md) covers the build and HTTP(S) hosting. [Register node images for provisioning](../operate/build-node-images.md#register-the-image-for-provisioning) covers the management-cluster configuration.

For NVKR's separate build route, see its [node image Definition](https://github.com/dsx-ai-factory/nvkr/blob/main/inputs/definitions/diagnostics/README.md).

## Management and bootstrap tooling

| Tool or host | Version | Notes |
|---|---|---|
| Bootstrap host | Linux | Any Linux host that reaches the nodes. It can be one of the nodes. |
| Container runtime on the bootstrap host | Rootful `nerdctl` with containerd | No version is checked. Docker is not supported. |
| `kind` | v0.32.0 | Pinned for continuous integration (CI) and the local quickstart. The bootstrap does not check the version. |
| `kubectl` | v1.36.0 | Pinned for CI. The bootstrap does not check the version. |
| `clusterctl` | v1.13.6 | Pinned. Match the Cluster API version above. The install reports a Cluster API of another version. |
| Helm | v4.2.4 | Pinned. The install and the local quickstart need Helm 4; Helm 3 cannot install the charts. The install warns on another version. |
| Go, to build `nvkectl` and the NVKE images | 1.27.3 or later | Range. An older Go downloads 1.27.3 unless `GOTOOLCHAIN=local` is set. Also needs GNU Make. |
| Go, to build `nvkr` (NVKR) | 1.27.2 | Pinned in NVKR's `.go-version`. |
| `nvkectl` | Linux and macOS, amd64 and arm64 | `nvkectl bootstrap site` runs only on a Linux bootstrap host. |
| Management cluster nodes | amd64, arm64 | All nodes of one architecture. The install reads it from the nodes and selects the release's artifacts for it. NVKR's maintained release builds the controller image for both architectures. |
| Local quickstart host | Linux, amd64 or arm64 | See [Quickstart: evaluate locally with mock-NICo](../get-started/quickstart-local.md). |

## nvkectl compatibility

`nvkectl` is versioned on its own `cli/vX.Y.Z` tags, separately from the management plane. The [nvkectl compatibility policy](../../cli/docs/compatibility.md) defines which commands, flags, exit codes and output formats are stable across releases. Build `nvkectl` from the same release as the management plane.
