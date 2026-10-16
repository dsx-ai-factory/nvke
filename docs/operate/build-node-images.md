# Build and host node OS images

Build a node OS image for NVIDIA Kubernetes Engine (NVKE), make its boot files available over HTTP(S), and verify that machines can download them. Do it after you plan the installation and before you prepare the nodes.

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

After building, [serve the boot artifacts over HTTP(S)](#serve-the-boot-artifacts-over-https), then [Prepare nodes](prepare-nodes.md) from the image.

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

## Next steps

[Prepare the management cluster's nodes](prepare-nodes.md) from the image, then bootstrap the cluster and install the management-plane services. Once the management plane is installed, [register the node image](register-node-images.md) so NVKE can use it to provision workload-cluster nodes.
