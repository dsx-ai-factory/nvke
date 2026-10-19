# Configure OVN-Kubernetes networking

This page describes how to configure and deploy OVN-Kubernetes as the Container Network Interface (CNI) for NVIDIA Kubernetes Engine (NVKE) management and workload clusters. You can use artifacts staged by NVIDIA Kubernetes Recipes (NVKR) or host the required packages and container image yourself.

## What is OVN-Kubernetes?

OVN-Kubernetes provides pod networking, Kubernetes Service routing and NetworkPolicy enforcement by combining Open Virtual Network (OVN) with Open vSwitch (OVS).

- OVS forwards packets on each node. NVKE installs OVS on the host and configures the node's physical uplink as a port on an OVS bridge before the node joins the cluster.
- OVN represents the cluster network as logical switches, routers and access controls, then programs the OVS data plane on each node.
- OVN-Kubernetes watches Kubernetes network resources and translates them into OVN configuration. Its control-plane components maintain the logical network, while its per-node components configure networking for pods on each node.

## NVIDIA-curated OVN-Kubernetes

NVKE uses an [NVIDIA-curated OVN-Kubernetes repository](https://github.com/dsx-ai-factory/ovn-kubernetes). NVIDIA manages this build so NVKE can rapidly adopt new features for GPUs and DPUs. NVIDIA plans to contribute all enhancements to the [upstream OVN-Kubernetes project](https://github.com/ovn-kubernetes/ovn-kubernetes).

## Before you begin

- You have a checkout of NVKE at the release you want to deploy and a build environment for NVKR, as described in [Get NVKE artifacts and OS profiles built with NVKR](../operate/build-node-images.md).
- If you host the artifacts yourself, you have a signed apt repository and an OCI registry that every cluster node can reach. Nodes authenticate to a private OCI registry through their container runtime configuration.
- You have completed the NVKE installation steps that precede creating the target cluster.

## Understand the artifacts

Each OVN-Kubernetes stream produces two Debian packages:

- `nvke-cni-ovn-charts-<CNI_VERSION>` contains the unpacked NVIDIA-curated Helm chart and NVKE's values file.
- `nvke-cni-ovn-images-<CNI_VERSION>` contains the NVKE-built `ovnkube` image as a Docker archive. Installing the package imports the image into containerd.

NVKE uses the artifacts as follows:

- **Helm chart:** The NVKE-managed deployment requires the chart Debian package on the initial control-plane node. During cluster initialization, the controller asks the node agent to deploy the local chart with the cluster's API endpoint, pod CIDR and service CIDR. NVKE does not deploy the chart from a remote Helm or OCI registry.
- **Container image:** Install the image Debian package to preload the image into containerd on each node, or configure the nodes to pull the image from a remote OCI registry.

The supported stream is `2.4.2`. The package build reads the source commit and tree hash from `os-image/packages/cni-ovn/lock/<ARCH>/2.4.2.lock`. It verifies those pins, exports the chart and builds the image before producing either package.

The chart package installs the chart under `/etc/dgx/ovn-kubernetes`. Its NVKE values disable the chart's OVS pod because NVKE runs OVS on the host. The node agent supplies the Kubernetes API endpoint, pod CIDR and service CIDR when it deploys the chart.

## Build the default artifacts

1. Build the NVKR node image by following [The node OS image](../operate/build-node-images.md#the-node-os-image).

   During the build, NVKR invokes the NVKE node-image build. That build:

   - downloads the locked OVN-Kubernetes source and builds its image;
   - creates the chart and image Debian packages for the image architecture;
   - writes a flat apt index; and
   - stages the packages and index under `/usr/lib/nvke/debs` in the node image.

2. Use the resulting node image for the cluster's nodes.

3. Install both packages during each node's first boot. In a `ClusterRelease` or `NodePoolRelease`, use the package names that NVKE renders from the cluster's selected CNI and version:

   ```yaml
   runcmd:
     - DEBIAN_FRONTEND=noninteractive apt-get install -y {{ .CNIImagePackage }} {{ .CNIChartsPackage }}
   ```

   With OVN-Kubernetes `2.4.2`, the package names resolve to `nvke-cni-ovn-images-2.4.2` and `nvke-cni-ovn-charts-2.4.2`. For management-cluster nodes, [Prepare nodes](../operate/prepare-nodes.md#write-the-user-data) shows the same packages by their literal names.

This default path needs no CNI registry access at runtime. The image package imports the image under the repository and tag recorded in the packaged values, so kubelet finds it in the local containerd image store.

> **Why does NVKE bake CNI images into the node image?** Installing CNI artifacts from the node image is the CNI deployment path for NVKE. The planned direction is API-driven deployment through a cluster add-on operator; after that path is available, the OS-baked path can be deprecated.

## Host the artifacts yourself

Use this path when nodes install packages from your apt repository or must pull the OVN image from your registry.

### Host the chart package

NVKE deploys OVN-Kubernetes from the `nvke-cni-ovn-charts-<CNI_VERSION>` Debian package, so that package must be installed on the initial control-plane node regardless of how container images are delivered. You can produce and distribute the package through your existing build and apt-publishing process.

The package source and build target live under `os-image/packages/cni-ovn/`. Whichever process you use, make the package available through an apt repository configured on the nodes before first-boot package installation. The OS profile examples use `{{ .CNIChartsPackage }}` and `{{ .CNIImagePackage }}`, so they work with any repository that provides those package names.

> **Note:** Hosting the chart means hosting its Debian package. NVKE does not accept a Helm repository URL or OCI chart reference for OVN-Kubernetes.

### Host the container image

You may publish `nvke-cni-ovn-images-<CNI_VERSION>` in your apt repository when nodes should import the image locally.

When you host the container image, publish the architecture-specific `ovnkube` image produced by the same build as the chart package. The image tag is the first 12 characters of the source commit recorded in `os-image/packages/cni-ovn/lock/<ARCH>/<CNI_VERSION>.lock`. A cluster with both `amd64` and `arm64` nodes needs a multi-platform index that contains both built images under that tag.

The `cni.imageRegistry` setting replaces the registry host and preserves the packaged `nvke/ovnkube` repository path and commit-based tag. For example, the `2.4.2` lock records commit `8af51273d52dfa07454435d18d37230e582d797a`. A registry setting of `registry.example.com` makes NVKE use:

```text
registry.example.com/nvke/ovnkube:8af51273d52d
```

Set the prefix in the NVKE controller chart:

```yaml
cni:
  imageRegistry: registry.example.com
  installAfterInit: true
```

`nvkectl` enables `installAfterInit` during the standard controller installation. A controller chart installed through another process needs the same setting.

The `nvke-cni-ovn-images-<CNI_VERSION>` Debian package normally preloads the OVN-Kubernetes image into containerd on each node. When you use your own registry instead, you do not need to install this package, provided every node can pull the hosted image before OVN-Kubernetes pods start. The OVN-Kubernetes install does not create an image pull secret. If the registry is private, configure registry authentication on every node.

## Deploy OVN-Kubernetes

1. Select OVN-Kubernetes and a version in the cluster configuration.

   A management-cluster site spec uses:

   ```yaml
   cluster:
     cniName: ovn-kubernetes
     cniVersion: 2.4.2
   ```

   A workload-cluster spec uses:

   ```yaml
   cniPlugin: ovn-kubernetes
   cniVersion: 2.4.2
   ```

   If omitted, the CNI defaults to `ovn-kubernetes` and its version defaults to `2.4.2`.

2. Create the cluster through the normal NVKE workflow.

   Before a node joins, NVKE configures its physical uplink as a port on an OVS bridge. On the initial control-plane node, the node agent reads the installed chart package and runs `helm upgrade --install` for a release named `ovn-kubernetes`. Other nodes use the CNI resources already installed in the cluster.

## Verify

After the cluster is created, point `kubectl` and Helm at its kubeconfig.

1. Confirm that the Helm release exists:

   ```bash
   helm list --all-namespaces --filter '^ovn-kubernetes$'
   ```

2. Check the OVN-Kubernetes workloads:

   ```bash
   kubectl -n ovn-kubernetes get deployments,daemonsets,pods
   ```

   The OVN-Kubernetes pods are `Running`, and the node DaemonSet has one ready pod for every cluster node.

3. Confirm that every node is ready:

   ```bash
   kubectl get nodes
   ```

## Change the OVN-Kubernetes version

The CNI version, chart package, image package and mirrored image must come from the same locked source. A different stream requires new locked artifacts and a node image or apt repository containing packages with that stream suffix.

NVKE does not automate in-place OVN-Kubernetes upgrades, and a Kubernetes upgrade does not change the CNI version. An operator can upgrade the existing `ovn-kubernetes` Helm release when OVN-Kubernetes provides a verified upgrade path between the installed and target versions. Before running `helm upgrade`, make the matching chart and images available, preserve the cluster-specific values from the existing release and follow the validation and sequencing required by that upgrade path.

This manual operation is outside the NVKE-managed lifecycle. NVKE does not validate the upgrade path, update its stored CNI version or roll back the release. Keep the CNI version and artifacts used to provision future nodes aligned with the manually installed version.
