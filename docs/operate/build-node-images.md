# Build node images and publish releases

This page describes how to build the reference node OS image, serve its boot artifacts over HTTP(S), and publish the machine image and release resources that NVIDIA Kubernetes Engine (NVKE) provisions nodes from. Do it after you plan the installation and before you prepare the nodes.

## Before you begin

- You have read [Plan your installation](plan-installation.md).
- You have a checkout of `dsx-ai-factory/nvke` in `nvke/`, at the release you install.
- You have the build tools listed in [Build the OS image](../../os-image/README.md#build-the-os-image).
- You have `nvkectl` on this host, from [Install nvkectl](bootstrap-management-cluster.md#install-nvkectl), to check the release resources before you publish them.
- To build your own profile content image, you have Docker and a container registry that the management cluster's nodes can pull from.

## Build the node OS image

The reference image is an Ubuntu disk image in QEMU copy-on-write (QCOW2) format. [Build the OS image](../../os-image/README.md#build-the-os-image) documents its prerequisites, package-builder modes and build options. From the root of the NVKE checkout, build and smoke-test a non-encrypted `amd64` image on a matching Linux host with Kernel-based Virtual Machine (KVM) access:

```bash
make -C os-image image-native ARCH=amd64 VARIANT=non-fde
make -C os-image boot-smoke REQUIRE_ACCELERATION=1
```

The build writes `os-image/packer/output-non-fde/nvke-node-non-fde-ubuntu-<VERSION>-amd64.qcow2`, where `<VERSION>` is the Ubuntu point release selected by `UBUNTU_RELEASE` in `os-image/packer/Makefile` (currently `26.04.1`). `ARCH=arm64` builds for an `arm64` host. The [FDE build instructions](../../os-image/README.md#full-disk-encryption-fde-variant) cover `VARIANT=fde`, its passphrase settings and separate verification; the `boot-smoke` target above checks the non-FDE image.

For a source-built EFI imager, see [Build qcow-imager.efi](../../os-image/efi/README.md#building) and its [compatibility limitations](../../os-image/efi/README.md#what-this-does-not-build). Its Makefile provides `make -C os-image/efi build-efi-x86` and `make -C os-image/efi build-efi-arm64`, which write `os-image/efi/output/x86_64/qcow-imager.efi` and `os-image/efi/output/aarch64/qcow-imager.efi`. Use an imager and script validated together for your NICo site; the source-built EFI is not interchangeable with the fleet imager.

After building, [serve the boot artifacts over HTTP(S)](#serve-the-boot-artifacts-over-https), then [Prepare nodes](prepare-nodes.md) from the image.

## Serve the boot artifacts over HTTP(S)

For the NICo iPXE boot flow described here, publish the boot artifacts at HTTP or HTTPS URLs that the machines can reach before their installed OS starts. Any endpoint that serves the files to these boot clients can provide this interface: a site-local web server, an object store with an HTTP(S) frontend, or a CDN. You choose and operate the hosting service. NVKE passes the boot instructions to NICo; it does not upload these files or create a server for them.

The files to publish depend on your boot method:

| Boot method | Files served over HTTP(S) | What downloads them |
|---|---|---|
| Write the QCOW2 image to disk | The architecture-matched EFI imager, such as `qcow-imager.efi`, and `nvke-node-<VARIANT>-ubuntu-<VERSION>-<ARCH>.qcow2`. | iPXE chainloads the EFI imager; the imager downloads the QCOW2 and writes it to disk. Use an imager and script validated for your NICo site, including its disk-selection parameters. |
| Install from an Ubuntu ISO | The matching `vmlinuz`, `initrd` and installer ISO. | iPXE loads the kernel and initrd; the installer downloads the ISO. This is the method in the example MachineImage. |

The `MachineImage.spec.nico.ipxeScript` field holds the script itself. If your script chains another `.ipxe` script, publish that file at a reachable HTTP(S) URL too. A QCOW2 is a disk image, not an EFI executable: the script must launch an imager to install it.

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

Put the final URLs into your site's validated iPXE script in `MachineImage.spec.nico.ipxeScript`: the EFI URL is the chainload target, and the QCOW2 URL is an input to the imager. Keep the imager's required parameters and NICo's cloud-init datasource in that script. These URL assignments alone are not a complete imaging script; this release has no reference QCOW2 boot script.

### Verify the endpoint before provisioning

From a host on the same provisioning network as the machines, download both files using the URLs and read-access method the boot clients will use. For an endpoint that permits unauthenticated reads:

```bash
curl --fail --location --output /tmp/nvke-qcow-imager.efi "$BOOT_LOADER_URL"
curl --fail --location --output /tmp/nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2 "$IMAGE_URL"
sha256sum /tmp/nvke-qcow-imager.efi /tmp/nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2
```

Compare the downloaded files' SHA-256 digests with the verified source artifacts before using them. Check every URL in an installer or chained-script flow in the same way. This checks network access and file integrity; a successful machine boot is still needed to validate firmware TLS support, imager compatibility, disk selection and first-boot user data.

## Define MachineImage and release resources

The former filesystem OS profile bundles under `profiles/osProfiles/` have [been removed](https://github.com/dsx-ai-factory/nvke/pull/1590). Define image and bootstrap content with Kubernetes resources instead. The API field `osProfile` remains: it selects a release resource by name, not a directory of profile files.

NVKE reads image and release content from three cluster-scoped custom resources in the API group `internal.nvke.nvidia.com/v1`:

| Resource | What it holds |
|---|---|
| [`MachineImage`](../../cli/examples/machineimage.yaml) | The image a node boots: its operating system, architecture, build date, and the iPXE script that tells NICo how to boot the machine. |
| [`ClusterRelease`](../../cli/examples/clusterrelease.yaml) | For control-plane nodes: the `MachineImage` to boot and the cloud-init user data the node runs on first boot. |
| [`NodePoolRelease`](../../cli/examples/nodepoolrelease.yaml) | The same, for worker nodes. |

A control-plane node pool resolves its `osProfile` to a `ClusterRelease`, and a worker node pool to a `NodePoolRelease`. This example gives all three resources one name and uses that name as the `osProfile`. The `MachineImage` spec and each release's `machineImageRef` and `machineInit` are immutable once applied: to change an image or its user data, create resources with new names. A release's `lifecycle.deprecated` field can change separately.

Run the file-preparation commands below from the directory containing your `nvke/` checkout. The resulting `releases/` directory is the input you publish after installing the management plane.

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
   | `spec.operatingSystem.family`, `spec.operatingSystem.version` | The distribution, such as `ubuntu` and `"26.04"` for the reference node image. |
   | `spec.architecture` | `amd64` or `arm64`. |
   | `spec.buildDate` | The date you built the image, as `YYYY-MM-DD`. |
   | `spec.variant` | Optional. Distinguishes images built from the same operating system for another purpose, such as `fde`. |
   | `spec.nico.ipxeScript` | The script NICo runs to boot the machine. It starts with a `#!ipxe` line of its own. |

   The example's `ipxeScript` installs Ubuntu 24.04 from the installer ISO; replace its `boot.example.com` and `releases.example.com` URLs with hosts that your NICo machines reach. To boot the reference node image instead, `ipxeScript` must write the QCOW2 file to the machine's disk with a loader your NICo site provides. Publish the files and verify their URLs as described in [Serve the boot artifacts over HTTP(S)](#serve-the-boot-artifacts-over-https). This release has no reference QCOW2 boot script.

4. Write the first-boot user data in `spec.machineInit.userDataTemplate` of both releases. It starts with a cloud-init header such as `#cloud-config`, and it can reference the values in [What NVKE puts in the user data](#what-nvke-puts-in-the-user-data). In the example, replace the `apt.example.com` repository line with a command that adds an apt repository serving the NVKE node packages; NVKE publishes no such repository. A profile for the reference node image installs the packages the image stages, as [Prepare nodes](prepare-nodes.md) does, and writes the SPIRE files shown below.

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
| `{{ .Username }}`, `{{ .HashedPassword }}` | The node login account, `controllerStack.tenant.user` and the password in `controllerStack.tenant.passwordFile` from the site spec, with the password as a SHA-512 crypt hash. |
| `{{ .Files }}` | The `nvke-agent` files to write with `write_files`, including `/etc/default/nke-agent`, which sets the agent's arguments and port. `{{ .NodeAgentPort }}` is the port alone. |
| `{{ .KubernetesVersion }}` | The Kubernetes version of the cluster or node pool. |
| `{{ .CNIImagePackage }}`, `{{ .CNIChartsPackage }}` | The CNI packages for the cluster's `cniPlugin` and `cniVersion`, such as `nvke-cni-ovn-images-1.4.0` and `nvke-cni-ovn-charts-1.4.0`. `{{ .OvnEnabled }}` is true for OVN-Kubernetes. |
| `{{ .SpireServerAddress }}`, `{{ .SpireServerPort }}` | Where the node reaches the management cluster's SPIRE server: the management cluster's virtual IP address (VIP) from the site spec's `cluster.controlPlaneVIP`, or its only node's IP address, and port `30440`. |
| `{{ .TrustDomain }}`, `{{ .TrustBundle }}` | The management plane's SPIFFE trust domain, and the PEM trust bundle of the management cluster's SPIRE server. Empty when the node does not use SPIFFE. |

`ipxeScript` is rendered the same way with one value, `{{ .DiskSelectionStrategy }}`: the node pool's `diskSelectionStrategy`, `smallest` or `largest`.

NVKE reads the SPIRE server's current trust bundle each time it creates a node and passes it in `{{ .TrustBundle }}`, so the profile carries no CA. A profile for the reference node image writes the SPIRE values to the files the image reads on first boot. Indent the block to match the rest of your `write_files` list:

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

Build the profile content image and name it in the site spec's `controllerStack.images.profiles`.

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

4. Set the image in the site spec.

   ```yaml
   controllerStack:
     images:
       profiles: <REGISTRY_HOST>/<NAMESPACE>/nvke-profiles@<PROFILES_DIGEST>
   ```

   `<PROFILES_DIGEST>` is the `sha256:` digest from step 3. A digest pin restarts the controller when the profiles change; `repository:tag` also works.

## Verify

Confirm that each release resource is a document `nvkectl` accepts.

```bash
nvkectl create machineimage -f releases/machineimage.yaml --dry-run=client
```

The command prints the complete `MachineImage` manifest, with its API version and kind, and contacts no cluster. It checks document structure; the management cluster enforces required fields and value rules when you create the resource. Repeat it with the other two files:

```bash
nvkectl create clusterrelease -f releases/clusterrelease.yaml --dry-run=client
nvkectl create nodepoolrelease -f releases/nodepoolrelease.yaml --dry-run=client
```

## Create the image and release resources

Preparing the YAML files and uploading the boot artifacts does not create Kubernetes resources. There are two separate steps:

1. **Install the CRD schemas.** Follow [Install management-plane services](install-management-plane-services.md). The NVKE controller chart installs the `MachineImage`, `ClusterRelease` and `NodePoolRelease` CustomResourceDefinitions by default (`releaseCRDs.install: true`). These define the cluster-scoped types in `internal.nvke.nvidia.com/v1`; they do not create your image or releases. If you disable that chart setting, you must install the schemas separately before publishing resources.

2. **Publish your objects.** After the install registers the site with `nvkectl config set-site`, use a site configuration whose kubeconfig can write these resources on the management cluster. From the directory containing your `releases/` files, create the image first, then its control-plane and worker releases:

   ```bash
   nvkectl create machineimage -f releases/machineimage.yaml
   nvkectl create clusterrelease -f releases/clusterrelease.yaml
   nvkectl create nodepoolrelease -f releases/nodepoolrelease.yaml
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

When you [create a workload cluster](../workload-clusters/create.md), set `controlPlaneConfig.osProfile` to the `ClusterRelease` name and each worker node pool's `osProfile` to its `NodePoolRelease` name. NVKE resolves the selected release to its `MachineImage`, renders that image's iPXE script and the release's user data, and passes them through CAPNICo to NICo for provisioning.

To change an image's boot instructions or a release's image reference or user data, publish new objects under new names and select those releases for subsequent provisioning. These fields are immutable; uploading different bytes at an existing URL would bypass that versioning, so keep published artifact paths stable too.

## Next steps

- [Prepare nodes](prepare-nodes.md) from the node image.
- [Bootstrap the management cluster](bootstrap-management-cluster.md).
- [Install management-plane services](install-management-plane-services.md) and publish the resources in the `releases` directory.
