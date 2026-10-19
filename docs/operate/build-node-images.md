# Build and host node OS images

Build a node OS image for NVIDIA Kubernetes Engine (NVKE), make its boot files available over HTTP(S), verify downloads, and register the image for provisioning. Build and host the image before preparing nodes; register it after installing the management plane.

## Before you begin

- You have read [Plan your installation](plan-installation.md).
- You have a checkout of `dsx-ai-factory/nvke` in `nvke/`, at the release you install.
- You have the build tools listed in [Build the OS image](../../os-image/README.md#build-the-os-image).

## Build the node OS image

The reference image is an Ubuntu disk image in QEMU copy-on-write (QCOW2) format. [Build the OS image](../../os-image/README.md#build-the-os-image) documents its prerequisites, package-builder modes and build options. From the root of the NVKE checkout, build and smoke-test a non-encrypted `amd64` image on a matching Linux host with Kernel-based Virtual Machine (KVM) access:

```bash
make -C os-image image-native ARCH=amd64 VARIANT=non-fde
make -C os-image boot-smoke REQUIRE_ACCELERATION=1
```

The build writes `os-image/packer/output-non-fde/nvke-node-non-fde-ubuntu-<VERSION>-amd64.qcow2`, where `<VERSION>` is the Ubuntu point release selected by `UBUNTU_RELEASE` in `os-image/packer/Makefile` (currently `26.04.1`). `ARCH=arm64` builds for an `arm64` host. The [FDE build instructions](../../os-image/README.md#full-disk-encryption-fde-variant) cover `VARIANT=fde`, its passphrase settings and separate verification; the `boot-smoke` target above checks the non-FDE image.

For a source-built EFI imager, see [Build qcow-imager.efi](../../os-image/efi/README.md#building) and its [compatibility limitations](../../os-image/efi/README.md#what-this-does-not-build). Its Makefile provides `make -C os-image/efi build-efi-x86` and `make -C os-image/efi build-efi-arm64`, which write `os-image/efi/output/x86_64/qcow-imager.efi` and `os-image/efi/output/aarch64/qcow-imager.efi`. Use an imager and script validated together for your NICo site; the source-built EFI is not interchangeable with the fleet imager.

After building, [serve the boot artifacts over HTTP(S)](#serve-the-boot-artifacts-over-https), then [Prepare nodes](prepare-nodes.md) from the image. After installing the management plane, [register the image for provisioning](#register-the-image-for-provisioning).

## Serve the boot artifacts over HTTP(S)

For the NICo iPXE boot flow described here, publish the boot artifacts at HTTP or HTTPS URLs that the machines can reach before their installed OS starts. Any endpoint that serves the files to these boot clients can provide this interface: a site-local web server, an object store with an HTTP(S) frontend, or a CDN. You choose and operate the hosting service. NVKE passes the boot instructions to NICo; it does not upload these files or create a server for them.

The files to publish depend on your boot method:

| Boot method | Files served over HTTP(S) | What downloads them |
|---|---|---|
| Write the QCOW2 image to disk | The architecture-matched EFI imager, such as `qcow-imager.efi`, and `nvke-node-<VARIANT>-ubuntu-<VERSION>-<ARCH>.qcow2`. | iPXE chainloads the EFI imager; the imager downloads the QCOW2 and writes it to disk. Use an imager and script validated for your NICo site, including its disk-selection parameters. |
| Install from an Ubuntu ISO | The matching `vmlinuz`, `initrd` and installer ISO. | iPXE loads the kernel and initrd; the installer downloads the ISO. Use an installer script validated for your site. |

If your boot script chains another `.ipxe` script, publish that file at a reachable HTTP(S) URL too. A QCOW2 is a disk image, not an EFI executable: the script must launch an imager to install it.

An OCI registry can store the artifacts, but iPXE has [no native OCI pull support](https://github.com/ipxe/ipxe/blob/master/src/config/general.h). If your build publishes them to a registry, pull and extract the files on your publishing host, verify them under your release policy, and upload the resulting files to the HTTP(S) endpoint. Neither an `oci://` reference nor an `s3://` upload destination is a boot URL.

Use a separate path for each image build and architecture, and keep its files available for future node creation and replacement. Configure DNS, routing and firewall access from the provisioning network to the endpoint's port, usually TCP 80 or 443. For HTTPS, both iPXE and the imager or installer must support TLS and trust the endpoint's certificate. Registry logins and AWS credentials on the publishing host are not automatically available to these boot clients; configure read access that they can use.

### Example: Amazon S3 with CloudFront

S3 with CloudFront is one hosting example; NVKE requires neither service. This example uses an existing private S3 bucket, a CloudFront distribution with that bucket's REST endpoint as its origin, and the [AWS CLI](https://docs.aws.amazon.com/cli/latest/reference/s3/cp.html) on the publishing host. Configure [CloudFront origin access control](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-restricting-access-to-s3.html) so CloudFront can read the bucket, and viewer access so the boot clients can fetch the files over HTTPS. The upload commands below do not configure that access.

Replace the bucket, Region, build ID and local paths below. `QCOW2_PATH` is the image from [Build the node OS image](#build-the-node-os-image); `EFI_PATH` is a compatible imager supplied by your site. Run the commands from the NVKE checkout root.

```bash
BOOT_BUCKET="amzn-s3-demo-bucket"
BOOT_REGION="us-west-2"
BOOT_PREFIX="nvke/build-001/amd64"
QCOW2_PATH="os-image/packer/output-non-fde/nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2"
EFI_PATH="/path/to/site-compatible/qcow-imager.efi"

aws s3 cp "$QCOW2_PATH" \
  "s3://${BOOT_BUCKET}/${BOOT_PREFIX}/nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2" \
  --region "$BOOT_REGION"
aws s3 cp "$EFI_PATH" \
  "s3://${BOOT_BUCKET}/${BOOT_PREFIX}/qcow-imager.efi" \
  --region "$BOOT_REGION"
```

Use your CloudFront distribution's hostname, or its configured custom hostname, to form the boot URLs. Keep the object prefix in the URL; this example assumes the distribution has no additional origin path:

```bash
BOOT_BASE_URL="https://<CLOUDFRONT_HOST>/${BOOT_PREFIX}"
BOOT_LOADER_URL="${BOOT_BASE_URL}/qcow-imager.efi"
IMAGE_URL="${BOOT_BASE_URL}/nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2"
```

For another object store or a web server, upload the same files and set `BOOT_BASE_URL` to that service's HTTP(S) base URL. The boot flow is the same. S3 static website hosting is not required for this example; its [website endpoints support HTTP only](https://docs.aws.amazon.com/AmazonS3/latest/userguide/WebsiteEndpoints.html).

Keep the final URLs for your site's validated iPXE script: the EFI URL is the chainload target, and the QCOW2 URL is an input to the imager. Keep the imager's required parameters and NICo's cloud-init datasource in that script. These URL assignments alone are not a complete imaging script.

The repository includes an [iPXE template used by end-to-end tests](../../controller/test/e2e/internal/nvke/nico_forge_ipxe.tmpl) that chains a separately supplied EFI imager and QCOW2 URL. It is a test template, not a complete, validated customer provisioning procedure. Use it only with an imager whose parameters match the script, and validate the pair on your target hardware; the [source-built EFI compatibility limitations](../../os-image/efi/README.md#what-this-does-not-build) explain why the available imagers are not interchangeable.

### Verify the endpoint before provisioning

From a host on the same provisioning network as the machines, download both files using the URLs and read-access method the boot clients will use. For an endpoint that permits unauthenticated reads:

```bash
curl --fail --location --output /tmp/nvke-qcow-imager.efi "$BOOT_LOADER_URL"
curl --fail --location --output /tmp/nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2 "$IMAGE_URL"
sha256sum /tmp/nvke-qcow-imager.efi /tmp/nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2
```

Compare the downloaded files' SHA-256 digests with the verified source artifacts before using them. Check every URL in an installer or chained-script flow in the same way. This checks network access and file integrity; a successful machine boot is still needed to validate firmware TLS support, imager compatibility, disk selection and first-boot user data.

## Register the image for provisioning

After you build and host a node OS image, register its boot instructions and first-boot settings with NVKE. This lets the management plane use that image when it provisions workload-cluster nodes. You do this after installing the management-plane services.

### Before registering an image

- You have completed the build, hosting and download checks above.
- You have installed the management-plane services and configured a site with `nvkectl config set-site`. The site's kubeconfig can create configuration objects on the management cluster. [Run NVKE end to end on one machine](../get-started/local-site.md) shows an installation example.
- You have `nvkectl` on this host and a checkout of `nvke/` matching the software you installed.

### Prepare the configuration files

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

#### What NVKE puts in the user data

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

### Verify

Check each YAML file with `nvkectl` before creating an object.

```bash
nvkectl create machineimage -f node-config/machineimage.yaml --dry-run=client
```

The command prints the complete `MachineImage` manifest, with its API version and kind, and contacts no cluster. It checks document structure; the management cluster enforces required fields and value rules when you create the resource. Repeat it with the other two files:

```bash
nvkectl create clusterrelease -f node-config/clusterrelease.yaml --dry-run=client
nvkectl create nodepoolrelease -f node-config/nodepoolrelease.yaml --dry-run=client
```

### Create the objects on the management cluster

The YAML files become configuration objects when you create them on the management cluster. There are two separate steps:

1. **Install the CRD schemas.** Follow [Run NVKE end to end on one machine](../get-started/local-site.md). The NVKE controller chart installs the `MachineImage`, `ClusterRelease` and `NodePoolRelease` CustomResourceDefinitions by default (`releaseCRDs.install: true`). These define the cluster-scoped types in `internal.nvke.nvidia.com/v1`; they do not create entries for your images or node settings. If you disable that chart setting, you must install the schemas separately before creating your objects.

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

### Retire a node configuration

Use the CLI to deprecate a published control-plane or worker release:

```bash
nvkectl deprecate clusterrelease <RELEASE_NAME>
nvkectl deprecate nodepoolrelease <RELEASE_NAME>
```

Run the command for the release type you want to retire. The CLI updates its lifecycle metadata; do not edit `spec.lifecycle.deprecated` by hand. Existing clusters and node pools can still resolve a deprecated release for provisioning and scaling, but it is not selected as a Kubernetes upgrade target. Choose a current release when creating new clusters or pools. `MachineImage` has no deprecation command.

## Next steps

[Create a workload cluster](../workload-clusters/create.md) using the names you registered for its control-plane and worker nodes.
