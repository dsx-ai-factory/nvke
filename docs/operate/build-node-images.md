# Get NVKE artifacts and OS profiles built with NVKR

This page describes where the artifacts that NVIDIA Kubernetes Engine (NVKE) installs come from, how to hand them to the install, and how to write the OS profiles that the management plane provisions nodes from. Do it after you plan the installation and before you prepare the nodes.

NVIDIA Kubernetes Recipes (NVKR) builds the artifacts NVKE runs: the controller image and chart, the charts NVKE installs alongside it, `nvkectl`, and the node OS image. NVKR's own documentation covers building, publishing and verifying them. NVKE owns the OS profiles, which are Kubernetes resources you write and apply to the management cluster.

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

### What the install takes from a Release

The install reads a Release by output name. Each item of the `deploy/nico` deploy path names the outputs it uses:

| Output | What it is | Used by |
|---|---|---|
| `cwe` | The NVKE controller image. | The `controller` item. |
| `controller-chart` | The NVKE controller Helm chart. | The `controller` item. |
| `controller-profiles` | The profile content image, which holds config profiles and hook scripts. | The `controller` item. |
| `capnico-controller` | The Cluster API provider for NVIDIA Infra Controller (NICo) image. | The `capnico` item. |
| `postgresql`, `spire-crds`, `spire`, `temporal`, `nktp` | Helm charts. | The item of the same name. |

The install selects each output for the site's architecture, `controllerStack.arch` in the site spec. An output name may carry an `-amd64` or `-arm64` suffix. A chart the Release does not carry comes from the checkout or from its public repository. An image the Release does not carry stops the render, which names every missing image.

### Hand the Release to the install

1. Verify the Release under your policy, as described in [Verify against an accepted policy](https://github.com/dsx-ai-factory/nvkr/blob/main/docs/release-quickstart.md#verify-against-an-accepted-policy).

2. Export the Release description.

   ```bash
   nvkr release export --reference <RELEASE_REF> > release.json
   ```

   `<RELEASE_REF>` is the Release reference you verified in step 1. The file is `nvkr.dev/release-description/v1` JSON that lists every output with its digest-pinned reference. `export` verifies nothing, so run it only on a reference that `verify` accepted. See [Export the complete description](https://github.com/dsx-ai-factory/nvkr/blob/main/docs/release-quickstart.md#export-the-complete-description).

3. Keep `release.json`. You pass it to the install as `--release` in [Install management-plane services](install-management-plane-services.md). For a local Run that was never published, also pass `--nvkr-run-dir .nvkr/runs/<ATTEMPT>`: the install then reads charts and images from the Run's OCI layouts instead of a registry, and imports the images into the node's containerd.

> **Note:** NVKR's maintained M2 Definition (`definitions/nvke-m2`) does not build the `controller-profiles` or `capnico-controller` outputs, so the install refuses a Release built from it. Install without `--release` instead. The controller chart then comes from your checkout, and the site spec names the controller and profiles images in `controllerStack.images.controller` and `controllerStack.images.profiles`, each as `repository:tag` or `repository@sha256:<DIGEST>`. To use the Release's controller image, take the `cwe` output's `Derived artifact reference:` from `nvkr release browse`. To build the profiles image, see [Build the profile content image](#build-the-profile-content-image).

### What a Release proves

NVKR records an evidence lane for every plan, Run and Release. No lane qualifies a Release.

| Lane | Comes from | Signed |
|---|---|---|
| `static-unqualified` | A plan or a dry run. Nothing is built. | No |
| `unqualified-local-execution` | A Run on your own host. | No |
| `pipeline-attested` | A Run of the hosted pipeline that NVKR generates. It publishes signatures and build provenance that `nvkr release verify` checks. | Yes |

See [Preview status and known limitations](../overview/preview-status.md).

### The node OS image

NVKR builds the node OS image, `nvke-node-image-<ARCH>.qcow2`: Ubuntu 26.04.1 in QEMU copy-on-write (QCOW2) format, for `amd64` or `arm64`. A local build runs NVKR's `scripts/nvke-os-local.sh` with `NVKE_OS_ARCH` set to the architecture, on native Linux with Kernel-based Virtual Machine (KVM) access, at least 8 CPUs, 10 GiB of memory and 64 GiB of free disk space. It writes `.nvkr/runs/<ATTEMPT>/outputs/nvke-node-image-<ARCH>.qcow2`, which is unsigned and unqualified. The maintained M2 Definition also builds node images for both architectures in the hosted pipeline. [NVKE node image](https://github.com/dsx-ai-factory/nvkr/blob/main/definitions/nvke-os/README.md) has the procedure. [Serve the boot artifacts over HTTP(S)](#serve-the-boot-artifacts-over-https), then [Prepare nodes](prepare-nodes.md) from the image.

## Serve the boot artifacts over HTTP(S)

For the NICo iPXE boot flow described here, publish the boot artifacts at HTTP or HTTPS URLs that the machines can reach before their installed OS starts. You provide the endpoint, for example an object store with an HTTP interface, a site-local web server, or an Amazon S3 bucket served through CloudFront. NVKE passes the boot instructions to NICo; it does not upload these files or create a server for them.

The files to publish depend on your boot method:

| Boot method | Files served over HTTP(S) | What downloads them |
|---|---|---|
| Write the NVKR QCOW2 image to disk | The architecture-matched EFI imager, such as `qcow-imager.efi`, and `nvke-node-image-<ARCH>.qcow2`. | iPXE chainloads the EFI imager; the imager downloads the QCOW2 and writes it to disk. Use an imager and script validated for your NICo site, including its disk-selection parameters. |
| Install from an Ubuntu ISO | The matching `vmlinuz`, `initrd` and installer ISO. | iPXE loads the kernel and initrd; the installer downloads the ISO. This is the method in the example MachineImage. |

The `MachineImage.spec.nico.ipxeScript` field holds the script itself. If your script chains another `.ipxe` script, publish that file at a reachable HTTP(S) URL too. A QCOW2 is a disk image, not an EFI executable: the script must launch an imager to install it.

An OCI registry can store the artifacts, but iPXE has [no native OCI pull support](https://github.com/ipxe/ipxe/blob/master/src/config/general.h). If your build publishes them to a registry, pull and extract the files on your publishing host, verify them under your release policy, and upload the resulting files to the HTTP(S) endpoint. Neither an `oci://` reference nor an `s3://` upload destination is a boot URL.

Use a separate path for each image build and architecture, and keep its files available for future node creation and replacement. Configure DNS, routing and firewall access from the provisioning network to the endpoint's port, usually TCP 80 or 443. For HTTPS, both iPXE and the imager or installer must support TLS and trust the endpoint's certificate. Registry logins and AWS credentials on the publishing host are not automatically available to these boot clients; configure read access that they can use.

### Example: upload to Amazon S3 and serve over HTTPS

This example uses an existing bucket and the [AWS CLI](https://docs.aws.amazon.com/cli/latest/reference/s3/cp.html) on the publishing host. Replace the bucket, Region, build ID and local paths below. `QCOW2_PATH` is the image from your NVKR Run; `EFI_PATH` is a compatible imager supplied by your site. The example uploads files only: the bucket or HTTP frontend must already allow the boot clients to read them.

```bash
BOOT_BUCKET="amzn-s3-demo-bucket"
BOOT_REGION="us-west-2"
BOOT_PREFIX="nvke/build-001/amd64"
QCOW2_PATH="/path/to/nvke-node-image-amd64.qcow2"
EFI_PATH="/path/to/site-compatible/qcow-imager.efi"

aws s3 cp "$QCOW2_PATH" \
  "s3://${BOOT_BUCKET}/${BOOT_PREFIX}/nvke-node-image-amd64.qcow2" \
  --region "$BOOT_REGION"
aws s3 cp "$EFI_PATH" \
  "s3://${BOOT_BUCKET}/${BOOT_PREFIX}/qcow-imager.efi" \
  --region "$BOOT_REGION"
```

If the bucket permits direct reads from the boot clients, its [regional S3 object endpoint](https://docs.aws.amazon.com/AmazonS3/latest/userguide/VirtualHosting.html) gives these HTTPS URLs:

```bash
BOOT_BASE_URL="https://${BOOT_BUCKET}.s3.${BOOT_REGION}.amazonaws.com/${BOOT_PREFIX}"
BOOT_LOADER_URL="${BOOT_BASE_URL}/qcow-imager.efi"
IMAGE_URL="${BOOT_BASE_URL}/nvke-node-image-amd64.qcow2"
```

To keep the bucket private, you can instead serve it through [CloudFront with origin access control](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-restricting-access-to-s3.html), configure viewer access for the boot clients, and use the CloudFront hostname in `BOOT_BASE_URL`. A site-local object store or web server can provide the same HTTP(S) interface. S3 static website hosting is not required; its [website endpoints support HTTP only](https://docs.aws.amazon.com/AmazonS3/latest/userguide/WebsiteEndpoints.html).

Put the final URLs into your site's validated iPXE script in `MachineImage.spec.nico.ipxeScript`: the EFI URL is the chainload target, and the QCOW2 URL is an input to the imager. Keep the imager's required parameters and NICo's cloud-init datasource in that script. These URL assignments alone are not a complete imaging script; this release has no reference QCOW2 boot script.

### Verify the endpoint before provisioning

From a host on the same provisioning network as the machines, download both files using the URLs and read-access method the boot clients will use. For an endpoint that permits unauthenticated reads:

```bash
curl --fail --location --output /tmp/nvke-qcow-imager.efi "$BOOT_LOADER_URL"
curl --fail --location --output /tmp/nvke-node-image-amd64.qcow2 "$IMAGE_URL"
sha256sum /tmp/nvke-qcow-imager.efi /tmp/nvke-node-image-amd64.qcow2
```

Compare the downloaded files' SHA-256 digests with the verified source artifacts before using them. Check every URL in an installer or chained-script flow in the same way. This checks network access and file integrity; a successful machine boot is still needed to validate firmware TLS support, imager compatibility, disk selection and first-boot user data.

## Write the OS profiles

The `osProfile` field of a workload cluster or node pool names a release that the management plane provisions its nodes from. NVKE reads releases only from three cluster-scoped custom resources in the API group `internal.nvke.nvidia.com/v1`:

| Resource | What it holds |
|---|---|
| `MachineImage` | The image a node boots: its operating system, architecture, build date, and the iPXE script that tells NICo how to boot the machine. |
| `ClusterRelease` | For control-plane nodes: the `MachineImage` to boot and the cloud-init user data the node runs on first boot. |
| `NodePoolRelease` | The same, for worker nodes. |

A control-plane node pool resolves its `osProfile` to a `ClusterRelease`, and a worker node pool to a `NodePoolRelease`. Give all three resources one name, and use that name as the `osProfile`. The specs are immutable once applied: to change an image or its user data, apply new resources with a new name.

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

   The example's `ipxeScript` installs Ubuntu 24.04 from the installer ISO; replace its `boot.example.com` and `releases.example.com` URLs with hosts that your NICo machines reach. To boot the NVKR node image instead, `ipxeScript` must write the QCOW2 file to the machine's disk with a loader your NICo site provides. Publish the files and verify their URLs as described in [Serve the boot artifacts over HTTP(S)](#serve-the-boot-artifacts-over-https). This release has no reference QCOW2 boot script.

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
| `{{ .Username }}`, `{{ .HashedPassword }}` | The node login account, `controllerStack.tenant.user` and the password in `controllerStack.tenant.passwordFile` from the site spec, with the password as a SHA-512 crypt hash. |
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

With a Release, the profile content image is the Release's `controller-profiles` output. Without one, or to use your own config profiles, build the image and name it in the site spec's `controllerStack.images.profiles`, which overrides the Release's.

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
