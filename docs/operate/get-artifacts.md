# Get the NVKE artifacts

This page describes how to produce the stack artifacts file, `stack-artifacts.json`, that the NVIDIA Kubernetes Engine (NVKE) install reads: the images and Helm charts of the management plane, each named by OCI reference and digest. Do it after you plan the installation and before you install the management-plane services.

The NVKE release is source: you build the artifacts from the checked-out release tag, as [RELEASE.md](https://github.com/dsx-ai-factory/nvke/blob/main/RELEASE.md) describes, or you publish them to your own registry and name them. Either way the install reads one file. `make local-stack-artifacts` writes it for a site that installs from the checkout; the example file is the starting point for a site that installs from its registry. [stack-artifacts.md](https://github.com/dsx-ai-factory/nvke/blob/main/cli/docs/stack-artifacts.md) is the file's reference.

## Before you begin

- You have read [Plan your installation](plan-installation.md).
- You have a checkout of `dsx-ai-factory/nvke` in `nvke/`, at the release you install.
- You have `nvkectl` on this host, from [Install nvkectl](bootstrap-management-cluster.md#install-nvkectl).
- To build the artifacts: a Linux host with `docker` and its `buildx` plugin or `nerdctl`, `helm` `v4.2.4`, `oras`, `python3` and GNU Make 4.
- To name artifacts in your registry: a registry the management cluster's nodes pull from, with the images and charts pushed to it.

## What the install needs

The `deploy/nico` deploy path binds each component to an artifact by name. The file must carry these:

| Artifact | Kind | What it is |
|---|---|---|
| `cwe` | `oci` | The NVKE controller image. |
| `controller-profiles` | `oci` | The profile content image: config profiles and hook scripts. See [Write release manifests](write-release-manifests.md#config-profiles-and-the-profile-content-image). |
| `capnico-controller` | `oci` | The Cluster API provider for NVIDIA Infra Controller (NICo) image. |
| `controller-chart` | `helm-oci` | The NVKE controller Helm chart. |
| `nktp` | `helm-oci` | The NVIDIA Kubernetes Telemetry Pipeline (NKTP) Helm chart. |

A chart the file does not carry, such as `postgresql`, `spire-crds`, `temporal` or `spire`, comes from the public repository the deploy path pins. An image the file does not carry stops the install, which names every missing one. The install selects each artifact for the management nodes' CPU architecture, which it reads from the cluster; an artifact name may carry an `-amd64` or `-arm64` suffix, or a multi-architecture image lists its platforms.

## Build the artifacts from the checkout

1. Build the five artifacts into OCI layouts and write the file that names them.

   ```bash
   make -C nvke local-stack-artifacts
   ```

   The target builds the controller image and the profile content image from the repository's test fixtures, copies the CAPNICo image the deploy path pins, packages the two charts with their dependencies, and writes `nvke/dist/local-site/stack-artifacts.json` with one layout per artifact under `nvke/dist/local-site/images/`. `ARCH=amd64` or `ARCH=arm64` builds for the management nodes' architecture when it is not this host's; `OUT=<DIR>` writes elsewhere; `CONTAINER_CLI=nerdctl` builds without Docker. The output directory is replaced as a whole, so a failed build leaves the previous one as it was.

2. Keep the output directory on the install host. The install imports each layout into the node's containerd under the name the file gives it, `nvke.local/<NAME>@<DIGEST>`, so the nodes pull nothing from a registry for these artifacts. A layout reaches one node: the one whose containerd the install's `CTR` command talks to. On a management cluster with more than one node, name the artifacts in a registry instead, as the next section describes.

## Name artifacts in your registry

1. Push the controller image, the profile content image and the two charts to your registry, and note each digest. `oras resolve <REFERENCE>` prints the digest of a pushed artifact; `docker push` prints it too.

2. Copy the example file.

   ```bash
   cp nvke/deploy/examples/stack-artifacts.json stack-artifacts.json
   ```

3. Replace every `REPLACE_*` value with the reference of the artifact in your registry, as `<REGISTRY_HOST>/<NAMESPACE>/<REPOSITORY>@sha256:<DIGEST>`, and set each chart's `chartVersion` to the version you pushed. The `capnico-controller` entry names the image on `ghcr.io` that the deploy path pins; replace its digest with the one `oras resolve` prints, or point it at a copy in your registry.

   The install pulls a chart from its reference with the credentials in your Docker configuration: run `docker login <REGISTRY_HOST>` or `oras login <REGISTRY_HOST>` on the install host first. The management cluster's nodes pull the images; if the registry needs credentials, pass a pull Secret to the controller chart with `--values-for controller=<FILE>` in [Install management-plane services](install-management-plane-services.md).

## Verify

1. Confirm that the file loads and names what the deploy path binds.

   ```bash
   nvke/deploy/check-site.sh --sitespec sitespec.yaml --stack-config stack-config.json --stack-artifacts stack-artifacts.json --kubeconfig <CLUSTER_NAME>.kubeconfig
   ```

   The `stack artifacts` findings pass: the file parses, every layout it names exists, and every image `deploy/nico` binds is in the file for the nodes' architecture. The check runs before the install, so it needs the bootstrapped cluster's kubeconfig; without the cluster, the file alone loads when `nvkectl` accepts it, which the install's check reports as the first `stack artifacts` finding.

## Next steps

- [Write release manifests](write-release-manifests.md).
- [Prepare nodes](prepare-nodes.md).
- [Install management-plane services](install-management-plane-services.md) with `stack-artifacts.json`.
