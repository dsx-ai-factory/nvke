# Register node images for provisioning

After you build and host a node OS image, register its boot instructions and first-boot settings with NVKE. This lets the management plane use that image when it provisions workload-cluster nodes. You do this after installing the management-plane services.

## Before you begin

- You have [built and hosted the node OS image](build-node-images.md), and verified the boot files' HTTP(S) URLs.
- You have completed [Install management-plane services](install-management-plane-services.md), including `nvkectl config set-site` with a kubeconfig that can create configuration objects on the management cluster.
- You have `nvkectl` on this host and a checkout of `nvke/` matching the software you installed.

## Prepare the configuration files

Create three YAML files that tell NVKE which hosted image to boot and what each node must do on first boot:

| Resource | What it holds |
|---|---|
| [`MachineImage`](../../cli/examples/machineimage.yaml) | Image metadata and boot instructions: operating system, architecture, build date, and an iPXE script pointing to the hosted boot files. The object stores no image bytes. |
| [`ClusterRelease`](../../cli/examples/clusterrelease.yaml) | Control-plane node settings: a reference to the `MachineImage` and a cloud-init template to initialize each node. |
| [`NodePoolRelease`](../../cli/examples/nodepoolrelease.yaml) | Worker node settings: a reference to the `MachineImage` and a cloud-init template to initialize each worker. |

A control-plane node pool resolves its `release` to a `ClusterRelease`, and a worker node pool to a `NodePoolRelease`. This example gives all three resources one name and uses that name as the `release`. The `MachineImage` spec and each release's `machineImageRef` and `machineInit` are immutable once applied: to change an image or its user data, create resources with new names. To deprecate a release, use the CLI commands in [Retire a node configuration](#retire-a-node-configuration).

Run the file-preparation commands below from the directory containing your `nvke/` checkout. The resulting `node-config/` directory holds the files you create on the management cluster below.

1. Copy the examples into a `node-config` directory.

   ```bash
   mkdir node-config
   ```

   ```bash
   cp nvke/cli/examples/machineimage.yaml nvke/cli/examples/clusterrelease.yaml nvke/cli/examples/nodepoolrelease.yaml node-config/
   ```

2. Set the name. Replace `example-ubuntu-24.04-amd64` in `metadata.name` of all three files and in `spec.machineImageRef.name` of both releases with one name of your own, such as `example-ubuntu-26.04-amd64`.

3. Describe the image in `node-config/machineimage.yaml`.

   | Field | Value |
   |---|---|
   | `spec.operatingSystem.family`, `spec.operatingSystem.version` | The distribution, such as `ubuntu` and `"26.04"` for the reference node image. |
   | `spec.architecture` | `amd64` or `arm64`. |
   | `spec.buildDate` | The date you built the image, as `YYYY-MM-DD`. |
   | `spec.variant` | Optional. Distinguishes images built from the same operating system for another purpose, such as `fde`. |
   | `spec.nico.ipxeScript` | The script NICo runs to boot the machine. It starts with a `#!ipxe` line of its own. |

   The example's `ipxeScript` installs Ubuntu 24.04 from the installer ISO; replace its `boot.example.com` and `releases.example.com` URLs with hosts that your NICo machines reach. To boot the reference node image instead, `ipxeScript` must write the QCOW2 file to the machine's disk by chainloading an EFI imager that downloads and writes the QCOW2. Use an imager and script validated together for your NICo site. Publish the files and verify their URLs as described in [Serve the boot artifacts over HTTP(S)](build-node-images.md#serve-the-boot-artifacts-over-https).

   The repository includes an end-to-end iPXE test template, but it does not provide a complete, validated customer provisioning procedure for this image. The hosting guide explains the template and the imager compatibility requirements.

4. Write the first-boot user data in `spec.machineInit.userDataTemplate` of both releases. It starts with a cloud-init header such as `#cloud-config`, and it can reference the values in [What NVKE puts in the user data](#what-nvke-puts-in-the-user-data). In the example, replace the `apt.example.com` repository line with a command that adds an apt repository serving the NVKE node packages; NVKE publishes no such repository. A release for the reference node image installs the packages the image stages, as [Prepare nodes](prepare-nodes.md) does, and writes the SPIRE files shown below.

5. Set the Kubernetes version annotations on both releases. Both are required.

   | `machine-init-kubernetes-version-injected` | `machine-init-kubernetes-version` | Meaning |
   |---|---|---|
   | `"true"` | `""` | The template installs `nvke-kubeadm-distro-{{ trimPrefix .KubernetesVersion "v" }}`, so the release serves any Kubernetes version. |
   | `"false"` | The version, such as `"1.36.3"` | The template installs one version, such as `nvke-kubeadm-distro-1.36.3`. |

   Both annotations take the prefix `internal.nvke.nvidia.com/`. When you upgrade a node pool whose release has `"false"`, NVKE selects the newest eligible configuration by its image's `buildDate`. A candidate must either name the target Kubernetes version or enable version injection so its template receives the target version at node creation. Its image must match the current image's architecture, variant and operating system. NVKE skips configurations deprecated with `nvkectl deprecate`.

6. Confirm that no example host remains.

   ```bash
   grep -n 'example.com' node-config/*.yaml
   ```

   The command prints nothing when every placeholder host is replaced.

The files hold `metadata` and `spec` only; `nvkectl` supplies the API version and kind when it publishes them. After checking the files, [create the objects on the management cluster](#create-the-objects-on-the-management-cluster), starting with `MachineImage` because both other objects reference it.

### What NVKE puts in the user data

NVKE renders `userDataTemplate` as a Go template for each node it creates, with the `indent`, `quote` and `trimPrefix` functions. You reference the values below; NVKE fills them in.

| Template value | What NVKE fills in |
|---|---|
| `{{ .Hostname }}` | The node's name. |
| `{{ .Username }}`, `{{ .HashedPassword }}` | The node login account from `NVKECTL_NODE_USER` and `NVKECTL_NODE_PASSWORD`, with the password as a SHA-512 crypt hash. |
| `{{ .Files }}` | The `nvke-agent` files to write with `write_files`, including `/etc/default/nke-agent`, which sets the agent's arguments and port. `{{ .NodeAgentPort }}` is the port alone. |
| `{{ .KubernetesVersion }}` | The Kubernetes version of the cluster or node pool. |
| `{{ .CNIImagePackage }}`, `{{ .CNIChartsPackage }}` | The CNI packages for the cluster's `cniPlugin` and `cniVersion`, such as `nvke-cni-ovn-images-2.4.2` and `nvke-cni-ovn-charts-2.4.2`. `{{ .OvnEnabled }}` is true for OVN-Kubernetes. |
| `{{ .SpireServerAddress }}`, `{{ .SpireServerPort }}` | Where the node reaches the management cluster's SPIRE server: the management cluster's virtual IP address (VIP) from the site spec's `cluster.controlPlaneVIP`, or its only node's IP address, and port `30440`. |
| `{{ .TrustDomain }}`, `{{ .TrustBundle }}` | The management plane's SPIFFE trust domain, and the PEM trust bundle of the management cluster's SPIRE server. Empty when the node does not use SPIFFE. |

`ipxeScript` is rendered the same way with one value, `{{ .DiskSelectionStrategy }}`: the node pool's `diskSelectionStrategy`, `smallest` or `largest`.

NVKE reads the SPIRE server's current trust bundle each time it creates a node and passes it in `{{ .TrustBundle }}`, so the release carries no CA. A release for the reference node image writes the SPIRE values to the files the image reads on first boot. Indent the block to match the rest of your `write_files` list:

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

## Verify

Check each YAML file with `nvkectl` before creating an object.

```bash
nvkectl create machineimage -f node-config/machineimage.yaml --dry-run=client
```

The command prints the complete `MachineImage` manifest, with its API version and kind, and contacts no cluster. It checks document structure; the management cluster enforces required fields and value rules when you create the resource. Repeat it with the other two files:

```bash
nvkectl create clusterrelease -f node-config/clusterrelease.yaml --dry-run=client
nvkectl create nodepoolrelease -f node-config/nodepoolrelease.yaml --dry-run=client
```

## Create the objects on the management cluster

The YAML files become configuration objects when you create them on the management cluster. There are two separate steps:

1. **Install the CRD schemas.** Follow [Install management-plane services](install-management-plane-services.md). The NVKE controller chart installs the `MachineImage`, `ClusterRelease` and `NodePoolRelease` CustomResourceDefinitions by default (`releaseCRDs.install: true`). These define the cluster-scoped types in `internal.nvke.nvidia.com/v1`; they do not create entries for your images or node settings. If you disable that chart setting, you must install the schemas separately before creating your objects.

2. **Create your objects.** After the install registers the site with `nvkectl config set-site`, use a site configuration whose kubeconfig can write these resources on the management cluster. From the directory containing your `node-config/` files, create the image first, then its control-plane and worker releases:

   ```bash
   nvkectl create machineimage -f node-config/machineimage.yaml
   nvkectl create clusterrelease -f node-config/clusterrelease.yaml
   nvkectl create nodepoolrelease -f node-config/nodepoolrelease.yaml
   ```

   `nvkectl` supplies `apiVersion` and `kind` and writes each object directly to the management cluster's Kubernetes API using the active site's kubeconfig. Each release's `spec.machineImageRef.name` must name an existing `MachineImage`; the CLI checks that reference before creating the release. These commands register metadata and boot instructions; they do not build, upload or download the image files.

Confirm that the objects exist:

```bash
nvkectl get machineimages
nvkectl get clusterreleases
nvkectl get nodepoolreleases
```

These commands also use the management cluster's Kubernetes API. If the site's kubeconfig is configured only for NVKE API authentication, use the management cluster's admin kubeconfig instead, for example:

```bash
kubectl --kubeconfig <MANAGEMENT_KUBECONFIG> get machineimages,clusterreleases,nodepoolreleases
```

When you [create a workload cluster](../workload-clusters/create.md), set `controlPlaneConfig.nico.release` to the `ClusterRelease` name and `nodePools[].nico.release` to each worker pool's `NodePoolRelease` name. NVKE resolves the selected configuration to its `MachineImage`, renders that image's iPXE script and the configuration's user data, and passes them through CAPNICo to NICo for provisioning.

To change an image's boot instructions or a release's image reference or user data, create new objects under new names and select those releases for subsequent provisioning. These fields are immutable; uploading different bytes at an existing URL would bypass that versioning, so keep published artifact paths stable too.

## Retire a node configuration

Use the CLI to deprecate a published control-plane or worker release:

```bash
nvkectl deprecate clusterrelease <RELEASE_NAME>
nvkectl deprecate nodepoolrelease <RELEASE_NAME>
```

Run the command for the release type you want to retire. The CLI updates its lifecycle metadata; do not edit `spec.lifecycle.deprecated` by hand. Existing clusters and node pools can still resolve a deprecated release for provisioning and scaling, but it is not selected as a Kubernetes upgrade target. Choose a current release when creating new clusters or pools. `MachineImage` has no deprecation command.

## Next steps

[Create a workload cluster](../workload-clusters/create.md) using the names you registered for its control-plane and worker nodes.
