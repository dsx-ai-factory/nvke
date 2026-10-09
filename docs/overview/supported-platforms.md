# Supported platforms and versions

This page lists the Kubernetes versions, networking, infrastructure providers, node operating system and tools that this release of NVIDIA Kubernetes Engine (NVKE) supports. Values change between releases; the values below are checked against this release's source and the NVIDIA Kubernetes Recipes (NVKR) Definitions that build its artifacts.

In the Notes column, **Pinned** means NVKE builds, installs or stages exactly that version, and other versions are not supported. **Range** means NVKE accepts any version within the stated bounds. For what preview does and does not support, see [Preview status and known limitations](preview-status.md).

## Kubernetes

| Item | Supported | Notes |
|---|---|---|
| Kubernetes versions | 1.34.2, 1.35.3, 1.36.3 | Pinned. The node image stages one `nke-kubeadm-distro` package for each version. A version not listed, including another patch release of the same minor version, is not on the node. |
| Version format | `X.Y.Z`, with no leading `v` | The API and the site spec reject any other format. |
| Management cluster | One of the versions above, 1.34.2 by default | Set once, in the site spec's `k8sVersion` field, at bootstrap. The management cluster does not upgrade itself. |
| Workload cluster | One of the versions above | `controlPlaneKubernetesVersion` is required and has no default. The API accepts any `X.Y.Z`, but nodes run only a version their node image carries. |
| Control-plane upgrade | One minor version up, or a later patch release | Range. Downgrades and requests for the current version are rejected. The cluster must have a `controlPlaneVIP`, and only one upgrade runs at a time. |
| Worker node pool skew | Up to 3 minor versions behind the control plane | Range. Checked when you upgrade the control plane. A new node pool starts at the cluster's version. |
| Upgrade target on NICo | A registered OS profile for the target version | The upgrade selects an OS profile for the target Kubernetes version with the same architecture, variant and Ubuntu version as the current one. It fails if none is registered, unless the current OS profile sets the Kubernetes version at provisioning time. |
| OpenID Connect (OIDC) on a workload cluster | 1.30.0 or later | Range. Applies to `enableOidc` and `nkeOIDCAuth`. |

## Container networking

| Plugin or setting | Version | Notes |
|---|---|---|
| `ovn-kubernetes` | 1.4.0 | Pinned. The default Container Network Interface (CNI) plugin for workload clusters and the management cluster. The node image stages its charts and images packages. |
| `calico` | 3.30.1 | Pinned. Set `cniPlugin: calico` on a workload cluster; the API then accepts only the default pod and service CIDRs. On the management cluster, the site spec's `cniName` accepts it for development. The node image installs its manifests package and stages its images package. |
| Other `cniVersion` values | Not in the node image | The node agent installs the CNI from packages on the node, so another version needs a node image built with it. |
| IP family | IPv4 | Pod and service CIDRs are private IPv4 ranges, fixed when the cluster is created: `/12` to `/23` for pods, `/12` to `/24` for services. The defaults are `192.168.0.0/16` and `10.96.0.0/12`. The API virtual IP address (VIP) is IPv4. |

## Infrastructure providers

| Provider | Version | Notes |
|---|---|---|
| NVIDIA Infra Controller (NICo), through CAPNICo | CAPNICo v0.0.45 | Pinned. The only infrastructure provider for workload clusters. The install deploys this CAPNICo release with the reboot and repair settings NVKE uses. The local quickstart builds a development CAPNICo version from source instead. |
| Cluster API core, kubeadm bootstrap and kubeadm control plane | v1.12.3 | Pinned. One version covers `clusterctl`, the providers that `clusterctl init` installs on the management cluster, and the providers `nkectl` installs in the seed cluster. |
| cert-manager | v1.19.3 | Pinned by Cluster API v1.12.3, which installs it during `clusterctl init`. |
| Provisioned infrastructure provider | Built from the same NVKE source as `nkectl` | Runs only in the seed cluster during bootstrap, to form the management cluster. |

## Node operating system

| Item | Supported | Notes |
|---|---|---|
| Operating system | Ubuntu 26.04.1 LTS | Pinned. NVKR builds the node image from the Ubuntu 26.04.1 server installer, pinned by digest. |
| CPU architectures | amd64, arm64 | NVKR has a node image Definition for each architecture. |
| Kernel | `linux-nvidia` on amd64, `linux-nvidia-64k` on arm64 | Ubuntu's NVIDIA-tuned kernel flavors. |
| Full-disk encryption | Not supported | The NVKR node image has no full-disk encryption. |
| SPIFFE Runtime Environment (SPIRE) agent | 1.14.1, with TPM plugin 1.9.0 | Pinned. Installed on the node image for TPM node attestation. |
| Node agent (`nke-agent`) | Built from the NVKE source NVKR pins | Installed and enabled on the node image. Listens on port 15005. |

See [Build node images with NVKR](../operate/build-node-images.md) for the build host requirements.

## Management and bootstrap tooling

| Tool or host | Version | Notes |
|---|---|---|
| Bootstrap host | Linux | Any Linux host that reaches the nodes. It can be one of the nodes. |
| Container runtime on the bootstrap host | Rootful `nerdctl` with containerd | No version is checked. Docker is not supported. |
| `kind` | v0.32.0 | Pinned for continuous integration (CI) and the local quickstart. The bootstrap does not check the version. |
| `kubectl` | v1.36.0 | Pinned for CI. The bootstrap does not check the version. |
| `clusterctl` | v1.12.3 | Pinned. Match the Cluster API version above. |
| Helm | v4.2.4 | Pinned for CI. The local quickstart needs Helm 4. The bootstrap and the install do not check the version. |
| Go, to build `nkectl` and the NVKE images | 1.27.2 or later | Range. An older Go downloads 1.27.2 unless `GOTOOLCHAIN=local` is set. Also needs GNU Make. |
| Go, to build `nkdctl` (NVKR) | 1.27.1 | Pinned. NVKR builds with `GOTOOLCHAIN=local`. |
| `nkectl` | Linux and macOS, amd64 and arm64 | `nkectl bootstrap site` runs only on a Linux bootstrap host. |
| Management cluster nodes | amd64 | The install uses the controller image that NVKR's `nke-v1` Definition builds, and `nke-v1` builds the controller image, its Helm chart and `nkectl` for linux/amd64 only. The NVKE source also builds the controller image for arm64, but the install does not use that image. |
| Local quickstart host | Linux, amd64 or arm64 | See [Quickstart: evaluate locally with mock-NICo](../get-started/quickstart-local.md). |

## nkectl compatibility

`nkectl` is versioned on its own `cli/vX.Y.Z` tags, separately from the management plane. The [nkectl compatibility policy](../../cli/docs/compatibility.md) defines which commands, flags, exit codes and output formats are stable across releases. Build `nkectl` from the same release as the management plane.
