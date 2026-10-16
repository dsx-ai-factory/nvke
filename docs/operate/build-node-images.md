# Get NVKE artifacts from NVKR and write release manifests

This page describes where the artifacts that NVIDIA Kubernetes Engine (NVKE) installs come from, how to hand them to the install, and how to write the releases that the management plane provisions nodes from. Do it after you plan the installation and before you prepare the nodes.

NVIDIA Kubernetes Recipes (NVKR) builds the artifacts NVKE runs: the controller image and chart, the charts NVKE installs alongside it, `nvkectl`, and the node OS image. NVKR's own documentation covers building, publishing and verifying them. NVKE owns the releases, which are Kubernetes resources you write and apply to the management cluster.

## Before you begin

- You have read [Plan your installation](plan-installation.md).
- You have a published NVKR Release reference, as `<REGISTRY>/<REPOSITORY>@sha256:<DIGEST>`, or a completed local NVKR Run. To produce either, follow the NVKR documentation linked in [Get the artifacts from NVKR](#get-the-artifacts-from-nvkr).
- You have a checkout of `dsx-ai-factory/nvke` in `nvke/`, at the release you install.
- You have `nvkectl` on this host, from [Install nvkectl](bootstrap-management-cluster.md#install-nvkectl), to check the release resources before you publish them.
- To build your own profile content image, you have Docker and a container registry that the management cluster's nodes can pull from.

## Get the artifacts from NVKR

NVKR builds every artifact from a Definition, a YAML file that names each output and the pinned sources it is built from. A Run executes a Definition, and a published Run is a Release: one digest-pinned reference for the whole set. Use these NVKR pages for the procedures:

| Task | NVKR documentation |
|---|---|
| Install `nvkr` and run a first build | [Quickstart](https://github.com/dsx-ai-factory/nvkr/blob/main/docs/quickstart.md) |
| Plan, run, publish and inspect a Run | [Definition, Run, and Release workflow](https://github.com/dsx-ai-factory/nvkr/blob/main/docs/lifecycle.md) |
| Browse, verify and export a Release | [Browse, export and verify a release](https://github.com/dsx-ai-factory/nvkr/blob/main/docs/release-quickstart.md) |
| The maintained NVKE Definition | [Combined NVKE M2 candidate](https://github.com/dsx-ai-factory/nvkr/blob/main/definitions/nvke-m2/README.md) |
| Build the node OS image | [NVKE node image](https://github.com/dsx-ai-factory/nvkr/blob/main/definitions/nvke-os/README.md) and [Build a node image from component materials](https://github.com/dsx-ai-factory/nvkr/blob/main/docs/node-images.md) |

### What the install takes from an NVKR Release

The install reads a Release by output name. Each item of the `deploy/nico` deploy path names the outputs it uses:

| Output | What it is | Used by |
|---|---|---|
| `cwe` | The NVKE controller image. | The `controller` item. |
| `controller-chart` | The NVKE controller Helm chart. | The `controller` item. |
| `controller-profiles` | The profile content image, which holds config profiles and hook scripts. | The `controller` item. |
| `capnico-controller` | The Cluster API provider for NVIDIA Infra Controller (NICo) image. | The `capnico` item. |
| `postgresql`, `spire-crds`, `spire`, `temporal`, `nktp` | Helm charts. | The item of the same name. |

The install selects each output for the management nodes' CPU architecture, which it reads from the cluster. An output name may carry an `-amd64` or `-arm64` suffix. A chart the Release does not carry comes from the checkout or from its public repository. An image the Release does not carry stops the render, which names every missing image.

### Hand the NVKR Release to the install

1. Verify the Release under your policy, as described in [Verify against an accepted policy](https://github.com/dsx-ai-factory/nvkr/blob/main/docs/release-quickstart.md#verify-against-an-accepted-policy).

2. Export the Release description.

   ```bash
   nvkr release export --reference <RELEASE_REF> > release.json
   ```

   `<RELEASE_REF>` is the Release reference you verified in step 1. The file is `nvkr.dev/release-description/v1` JSON that lists every output with its digest-pinned reference. `export` verifies nothing, so run it only on a reference that `verify` accepted. See [Export the complete description](https://github.com/dsx-ai-factory/nvkr/blob/main/docs/release-quickstart.md#export-the-complete-description).

3. Keep `release.json`. You pass it to the install as `--release` in [Install management-plane services](install-management-plane-services.md). For a local Run that was never published, also pass `--nvkr-run-dir .nvkr/runs/<ATTEMPT>`: the install then reads charts and images from the Run's OCI layouts instead of a registry, and imports the images into the node's containerd.

> **Note:** NVKR's maintained M2 Definition (`definitions/nvke-m2`) does not build the `controller-profiles` or `capnico-controller` outputs, so the install refuses a Release built from it. Install without `--release` instead. The controller chart then comes from your checkout, and the stack config names the profile content image in `images.profiles`, as `repository:tag` or `repository@sha256:<DIGEST>`; `images.controller` is optional in the same form and overrides the deploy path's pinned controller image. To use the Release's controller image, take the `cwe` output's `Derived artifact reference:` from `nvkr release browse`. To build the profiles image, see [Build the profile content image](#build-the-profile-content-image).

### What an NVKR Release proves

NVKR records an evidence lane for every plan, Run and Release. No lane qualifies a Release.

| Lane | Comes from | Signed |
|---|---|---|
| `static-unqualified` | A plan or a dry run. Nothing is built. | No |
| `unqualified-local-execution` | A Run on your own host. | No |
| `pipeline-attested` | A Run of the hosted pipeline that NVKR generates. It publishes signatures and build provenance that `nvkr release verify` checks. | Yes |

See [Preview status and known limitations](../overview/preview-status.md).

### The node OS image

NVKR builds the node OS image, `nvke-node-image-<ARCH>.qcow2`: Ubuntu 26.04.1 in QEMU copy-on-write (QCOW2) format, for `amd64` or `arm64`. A local build runs NVKR's `scripts/nvke-os-local.sh` with `NVKE_OS_ARCH` set to the architecture, on native Linux with Kernel-based Virtual Machine (KVM) access, at least 8 CPUs, 10 GiB of memory and 64 GiB of free disk space. It writes `.nvkr/runs/<ATTEMPT>/outputs/nvke-node-image-<ARCH>.qcow2`, which is unsigned and unqualified. The maintained M2 Definition also builds node images for both architectures in the hosted pipeline. [NVKE node image](https://github.com/dsx-ai-factory/nvkr/blob/main/definitions/nvke-os/README.md) has the procedure. Copy the image to where your provisioning system serves images, then [Prepare nodes](prepare-nodes.md) from it.

## Write the release manifests

The `release` field of a workload cluster or node pool names the release that the management plane provisions its nodes from. NVKE reads releases only from three cluster-scoped custom resources in the API group `internal.nvke.nvidia.com/v1`:

| Resource | What it holds |
|---|---|
| `MachineImage` | The image a node boots: its operating system, architecture, build date, and the iPXE script that tells NICo how to boot the machine. |
| `ClusterRelease` | For control-plane nodes: the `MachineImage` to boot and the cloud-init user data the node runs on first boot. |
| `NodePoolRelease` | The same, for worker nodes. |

A control-plane node pool resolves its `release` to a `ClusterRelease`, and a worker node pool to a `NodePoolRelease`. Give all three resources one name, and use that name as the `release`. The specs are immutable once applied: to change an image or its user data, apply new resources with a new name.

1. Copy the examples into a `releases` directory.

   ```bash
   mkdir releases
   ```

   ```bash
   cp nvke/cli/examples/machineimage.yaml nvke/cli/examples/clusterrelease.yaml nvke/cli/examples/nodepoolrelease.yaml releases/
   ```

2. Set the name. Replace `example-ubuntu-24.04-amd64` in `metadata.name` of all three files and in `spec.machineImageRef.name` of both releases with one name of your own, such as `example-ubuntu-26.04-amd64`.

3. Describe the image in `releases/machineimage.yaml`.

   | Field | Value |
   |---|---|
   | `spec.operatingSystem.family`, `spec.operatingSystem.version` | The distribution, such as `ubuntu` and `"26.04"` for the NVKR node image. |
   | `spec.architecture` | `amd64` or `arm64`. |
   | `spec.buildDate` | The date you built the image, as `YYYY-MM-DD`. |
   | `spec.variant` | Optional. Distinguishes images built from the same operating system for another purpose, such as `fde`. |
   | `spec.nico.ipxeScript` | The script NICo runs to boot the machine. It starts with a `#!ipxe` line of its own. |

   The example's `ipxeScript` installs Ubuntu 24.04 from the installer ISO; replace its `boot.example.com` and `releases.example.com` URLs with hosts that your NICo machines reach. To boot the NVKR node image instead, `ipxeScript` must write the QCOW2 file to the machine's disk with a loader your NICo site provides. This release has no reference script.

4. Write the first-boot user data in `spec.machineInit.userDataTemplate` of both releases. It starts with a cloud-init header such as `#cloud-config`, and it can reference the values in [What NVKE puts in the user data](#what-nvke-puts-in-the-user-data). In the example, replace the `apt.example.com` repository line with a command that adds an apt repository serving the NVKE node packages; NVKE publishes no such repository. A profile for the NVKR node image installs the packages the image stages, as [Prepare nodes](prepare-nodes.md) does, and writes the SPIRE files shown below.

5. Set the Kubernetes version annotations on both releases. Both are required.

   | `machine-init-kubernetes-version-injected` | `machine-init-kubernetes-version` | Meaning |
   |---|---|---|
   | `"true"` | `""` | The template installs `nvke-kubeadm-distro-{{ trimPrefix .KubernetesVersion "v" }}`, so the release serves any Kubernetes version. |
   | `"false"` | The version, such as `"1.34.8"` | The template installs one version, such as `nvke-kubeadm-distro-1.34.8`. |

   Both annotations take the prefix `internal.nvke.nvidia.com/`. When you upgrade a node pool whose release has `"false"`, NVKE moves it to the newest release, by `buildDate`, whose version annotation matches the target version and whose image matches the current image's architecture, variant and operating system. It skips releases with `spec.lifecycle.deprecated` set.

6. Confirm that no example host remains.

   ```bash
   grep -n 'example.com' releases/*.yaml
   ```

   The command prints nothing when every placeholder host is replaced.

The files hold `metadata` and `spec` only; `nvkectl` supplies the API version and kind when it publishes them. You publish the three resources with `nvkectl create` in [Install management-plane services](install-management-plane-services.md), after `nvkectl config set-site` records the management cluster's kubeconfig, with the machine image first because both releases reference it.

### What NVKE puts in the user data

NVKE renders `userDataTemplate` as a Go template for each node it creates, with the `indent`, `quote` and `trimPrefix` functions. You reference the values below; NVKE fills them in.

| Template value | What NVKE fills in |
|---|---|
| `{{ .Hostname }}` | The node's name. |
| `{{ .Username }}`, `{{ .HashedPassword }}` | The node login account from `NVKECTL_NODE_USER` and `NVKECTL_NODE_PASSWORD`, with the password as a SHA-512 crypt hash. |
| `{{ .Files }}` | The `nvke-agent` files to write with `write_files`, including `/etc/default/nke-agent`, which sets the agent's arguments and port. `{{ .NodeAgentPort }}` is the port alone. |
| `{{ .KubernetesVersion }}` | The Kubernetes version of the cluster or node pool. |
| `{{ .CNIImagePackage }}`, `{{ .CNIChartsPackage }}` | The CNI packages for the cluster's `cniPlugin` and `cniVersion`, such as `nvke-cni-ovn-images-1.4.0` and `nvke-cni-ovn-charts-1.4.0`. `{{ .OvnEnabled }}` is true for OVN-Kubernetes. |
| `{{ .SpireServerAddress }}`, `{{ .SpireServerPort }}` | Where the node reaches the management cluster's SPIRE server: the management cluster's virtual IP address (VIP) from the site spec's `cluster.controlPlaneVIP`, or its only node's IP address, and port `30440`. |
| `{{ .TrustDomain }}`, `{{ .TrustBundle }}` | The management plane's SPIFFE trust domain, and the PEM trust bundle of the management cluster's SPIRE server. Empty when the node does not use SPIFFE. |

`ipxeScript` is rendered the same way with one value, `{{ .DiskSelectionStrategy }}`: the node pool's `diskSelectionStrategy`, `smallest` or `largest`.

NVKE reads the SPIRE server's current trust bundle each time it creates a node and passes it in `{{ .TrustBundle }}`, so the profile carries no CA. A profile for the NVKR node image writes the SPIRE values to the files the image reads on first boot. Indent the block to match the rest of your `write_files` list:

```yaml
write_files:
  - path: /etc/nke/spire.env
    permissions: "0600"
    content: |
      SPIRE_SERVER_ADDRESS={{ .SpireServerAddress }}
      SPIRE_SERVER_PORT={{ .SpireServerPort }}
      SPIRE_TRUST_DOMAIN={{ .TrustDomain }}
  - path: /etc/nke/trust-bundle.pem
    permissions: "0644"
    content: |
{{ indent 6 .TrustBundle }}
```

`indent 6` adds six spaces to each line of the bundle so that it sits under `content`. The image renders the SPIRE agent configuration from these files, and the agent attests with the node's TPM. The install turns SPIFFE on, so the management plane reaches `nvke-agent` over mutual TLS with a SPIFFE identity: a node whose SPIRE agent cannot attest never becomes reachable.

## Config profiles and the profile content image

The optional `configProfile` field of a node pool names a config profile, which sets node labels and the hook scripts that run before a node joins. The controller reads config profiles from a profile content image that an init container, `materialize-profiles`, copies into the controller pod at startup. NVKE looks up `configProfiles/<NAME>.yaml` in that image, and the hook scripts under `runbooks/`.

With a Release, the profile content image is the Release's `controller-profiles` output. Without one, or to use your own config profiles, build the image and name it in the stack config's `images.profiles`, which overrides the Release's.

### Build the profile content image

1. Copy the example bundle.

   ```bash
   cp -R nvke/controller/profiles/example example-profiles
   ```

   The bundle holds one config profile, `profiles/configProfiles/example-worker.yaml`, its hook script `runbooks/example-sysctl.sh`, and a `Dockerfile` that copies both into `/profiles`.

2. Replace `REPLACE_SYSCTL_VALUE` in `example-profiles/profiles/configProfiles/example-worker.yaml`, the value the hook script writes for `net.ipv4.ip_forward`, and add your own profiles. Then confirm that no placeholder remains.

   ```bash
   test -z "$(grep -R 'REPLACE_' example-profiles)"
   ```

3. Build and push the image.

   ```bash
   docker build -t <REGISTRY_HOST>/<NAMESPACE>/nvke-profiles:<TAG> example-profiles
   ```

   ```bash
   docker push <REGISTRY_HOST>/<NAMESPACE>/nvke-profiles:<TAG>
   ```

   `<REGISTRY_HOST>/<NAMESPACE>` is your registry path, such as `registry.example.com/nvke`, and `<TAG>` is a version you choose, such as `v1`. Note the digest that `docker push` prints.

4. Set the image in the stack config, the file you write in [Install management-plane services](install-management-plane-services.md#write-the-stack-config).

   ```json
   "images": {
     "controller": "",
     "profiles": "<REGISTRY_HOST>/<NAMESPACE>/nvke-profiles@<PROFILES_DIGEST>"
   }
   ```

   `<PROFILES_DIGEST>` is the `sha256:` digest from step 3. A digest pin restarts the controller when the profiles change; `repository:tag` also works.

## Verify

1. Confirm that the Release description lists the outputs the install needs.

   ```bash
   jq -r '.outputs[].name' release.json
   ```

   The list includes `cwe`, `controller-chart`, `controller-profiles` and `capnico-controller`, each possibly with an architecture suffix. Skip this step if you install without `--release`.

2. Confirm that each release resource is a document `nvkectl` accepts.

   ```bash
   nvkectl create machineimage -f releases/machineimage.yaml --dry-run=client
   ```

   The command prints the complete `MachineImage` manifest, with its API version and kind, and contacts no cluster. Repeat it with `clusterrelease` and `nodepoolrelease` for the other two files.

## Next steps

- [Prepare nodes](prepare-nodes.md) from the node image.
- [Bootstrap the management cluster](bootstrap-management-cluster.md).
- [Install management-plane services](install-management-plane-services.md) with `release.json` and the `releases` directory.
