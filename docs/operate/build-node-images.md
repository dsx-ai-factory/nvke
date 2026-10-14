# Build node images with NVKR

This page describes how to build the artifacts that NVIDIA Kubernetes Engine (NVKE) runs with NVIDIA Kubernetes Recipes (NVKR), and how to package the node images so that the management plane can use them. It is the first step of an installation, after you plan it and before you prepare the nodes.

NVKR builds from Definitions, YAML files that name each output and the pinned sources it is built from. This page uses the Definitions in two directories of the NVKR repository:

| Directory | Outputs |
|---|---|
| `definitions/nvke-v1` | `nvkectl`, the NVKE controller image (`cwe`) and the controller Helm chart (`nvke-helm`). |
| `definitions/nvke-os` | The node OS image: an Ubuntu 26.04 disk image in QEMU copy-on-write (QCOW2) format, without full-disk encryption. |

Artifacts you build on your own host are unsigned and unqualified. Signatures and build evidence come only from a run of the GitHub Actions workflow that NVKR generates, and NVKR does not qualify those Releases either. The node OS image builds only on a local host, so it is always unsigned and NVKR does not publish it; see [Preview status and known limitations](../overview/preview-status.md).

## Before you begin

- You have read [Plan your installation](plan-installation.md).
- You have a Linux AMD64 build host with Docker, Git, GNU Make, `jq`, and Go at exactly the version in the NVKR repository's `.go-version` file (1.27.1). The host kernel supports unprivileged overlayfs (Linux 5.11 or later, or an Ubuntu kernel), and the host reaches the source repositories and image registries the builds fetch from.
- To build the node OS image, the build host also has Kernel-based Virtual Machine (KVM) access, at least 8 CPUs, 10 GiB of memory and 64 GiB of free disk space. The build does not fall back to software emulation.
- You have a container registry you control and an account that can push to it.
- To produce signed Releases, you have a GitHub organization where you can fork `dsx-ai-factory/nvkr` and run GitHub Actions, and a Linux host with a statically linked `cosign` executable for verification.

## Install nvkr

1. Clone the NVKR repository and build `nvkr`.

   ```bash
   git clone https://github.com/dsx-ai-factory/nvkr.git
   ```

   ```bash
   cd nvkr && make build
   ```

   The binary is `bin/nvkr`. Run every `nvkr` command on this page from the repository root. `make build` fails if your Go version differs from `.go-version`.

## Validate and plan the Definition

Planning reads the Definition and an Environment, the file that sets where a run executes, publishes and signs, without building or publishing anything.

1. Validate the Definition.

   ```bash
   bin/nvkr definition validate --definition definitions/nvke-v1/definition.yaml
   ```

   The output names the Definition, its digest and `outputs: 3`.

2. Plan a run against the local Environment.

   ```bash
   bin/nvkr run plan \
     --definition definitions/nvke-v1/definition.yaml \
     --environment definitions/nvke-v1/environment-local.yaml
   ```

   The output shows the Definition and Environment digests, the run plan digest, `expected outputs: 3` and the evidence lane, `static-unqualified`.

## Build and publish the NVKE components

A local run builds the three outputs on the build host and, with `--publish`, pushes them to the registry named in the Environment.

1. Copy the local Environment and point it at your registry.

   ```bash
   cp definitions/nvke-v1/environment-local.yaml definitions/nvke-v1/environment-example.yaml
   ```

   In the copy, replace the `publication` block:

   ```yaml
   publication:
     credentialRef: registry
     repositoryPrefix: <REGISTRY_HOST>/<NAMESPACE>
   ```

   `<REGISTRY_HOST>/<NAMESPACE>` is the registry path that receives the outputs, such as `registry.example.com/nvke`. Each output gets its own repository under it. Keep `signing.mode: dry-run`: a local run records the signing it would do and creates no signature.

2. Set the registry credentials for the run.

   ```bash
   export NKD_REGISTRY_USER=<REGISTRY_USER>
   ```

   ```bash
   export NKD_CREDENTIAL=<REGISTRY_TOKEN>
   ```

   `<REGISTRY_USER>` and `<REGISTRY_TOKEN>` are the account and token that can push to your registry.

3. Check the run without building anything.

   ```bash
   bin/nvkr run local \
     --definition definitions/nvke-v1/definition.yaml \
     --environment definitions/nvke-v1/environment-example.yaml \
     --attempt example-check --dry-run
   ```

   A dry run creates no attempt storage and reports `observed outputs: 0`.

4. Run the build and publish the outputs.

   ```bash
   bin/nvkr run local \
     --definition definitions/nvke-v1/definition.yaml \
     --environment definitions/nvke-v1/environment-example.yaml \
     --attempt example-001 --jobs 1 --publish
   ```

   Every attempt name must be new; NVKR never reuses one. The command prints `Published immutable reference:` followed by the Release reference, `<REGISTRY_HOST>/<NAMESPACE>/releases@sha256:` and a digest. It ends with `lane: unqualified-local-execution`, `status: complete` and `record: .nvkr/runs/example-001/lifecycle.json`.

5. List the published outputs and their references.

   ```bash
   bin/nvkr release browse --reference <RELEASE_REF>
   ```

   `<RELEASE_REF>` is the Release reference from step 4. Each output, such as `cwe`, shows a `Derived artifact reference:` line with its digest-pinned reference. This command checks integrity only; it does not check signatures.

Keep the reference for the `cwe` output, `<REGISTRY_HOST>/<NAMESPACE>/cwe@sha256:` and a digest. It has no tag. You pass it to `nvkectl bootstrap site --controller-image` in [Bootstrap the management cluster](bootstrap-management-cluster.md), and you split it into the chart's `image.repository` and `image.digest` values, with an `image.tag` of your choice, in [Install management-plane services](install-management-plane-services.md). This Definition does not build the provisioned infrastructure provider image that the bootstrap also needs. `make -C nvke/cli images` builds it into the bootstrap host's local image store, as described in [Bootstrap the management cluster](bootstrap-management-cluster.md).

## Produce a signed Release

A Release is an immutable set of artifacts and their build evidence behind one digest-pinned reference. NVKR produces signed Releases from a GitHub Actions workflow that it generates and that signs with the workflow's own GitHub identity, so you need no signing key.

1. Fork `dsx-ai-factory/nvkr` into your GitHub organization and clone the fork.

2. Copy `definitions/nvke-v1/environment-github.yaml` to a new file in the fork, such as `definitions/nvke-v1/environment-example-github.yaml`. In the copy, replace `dsx-ai-factory` with your organization in `publication.repositoryPrefix`, in lowercase, and in `signing.identity`.

   The workflow publishes to GitHub Container Registry under that prefix, using the workflow's `GITHUB_TOKEN`. If the copy keeps its `sourceAccess` entry, add a repository secret named `NKE_SOURCE_TOKEN` with read access to `dsx-ai-factory/nvke`.

3. Generate the workflow, then commit the Environment copy and the generated files and push them to `main`.

   ```bash
   bin/nvkr run generate \
     --definition definitions/nvke-v1/definition.yaml \
     --environment definitions/nvke-v1/environment-example-github.yaml \
     --engine github --root .
   ```

   The command writes `.github/workflows/nvkr-run.yml` and its inputs under `.github/nkd-run/`, and prints the plan digest.

4. In the fork's settings, create a deployment environment named `nkd-release` with a branch policy that admits only `main`.

5. Run the workflow.

   ```bash
   gh workflow run nvkr-run.yml --repo <GITHUB_ORG>/nvkr --ref main
   ```

   `<GITHUB_ORG>` is your GitHub organization. The workflow builds, publishes, signs and attests every output. After publication, its log prints `Published immutable reference:` followed by the Release reference, `<REPOSITORY_PREFIX>/releases@sha256:` and a digest, where `<REPOSITORY_PREFIX>` is the value you set in step 2.

6. Verify the Release on a Linux host.

   ```bash
   bin/nvkr release verify \
     --reference <RELEASE_REF> \
     --lane pipeline-attested --scope artifact-publication \
     --identity '<IDENTITY_REGEXP>' \
     --issuer https://token.actions.githubusercontent.com \
     --cosign <COSIGN_PATH> --cosign-digest <COSIGN_DIGEST>
   ```

   - `<RELEASE_REF>`: the Release reference from step 5.
   - `<IDENTITY_REGEXP>`: the `signing.identity` value from your Environment copy.
   - `<COSIGN_PATH>` and `<COSIGN_DIGEST>`: the path of a statically linked Linux `cosign` executable and its digest, in the form `sha256:` followed by 64 lowercase hex digits.

   Add `--anonymous` if the packages are public. Otherwise the command uses the registry credentials stored on the host, for example by `docker login ghcr.io`. The command checks each artifact's signature and build provenance against your identity and issuer, then prints the verified description of the Release. Any failure stops with an error. This check covers artifact publication and build evidence only; it is not a qualification of the Release.

## Build the node OS image

1. Build the image for your node architecture.

   ```bash
   CONTAINER_RUNTIME=docker NVKE_OS_ARCH=amd64 scripts/nvke-os-local.sh example-os-001 1
   ```

   The first argument is a new attempt name and the second is the number of parallel jobs. Use `NVKE_OS_ARCH=arm64` on a Linux ARM64 host with KVM. The script builds a local Packer toolchain image, freezes the inputs under `.nvkr/inputs/example-os-001`, and runs the build.

2. Find the image.

   ```text
   .nvkr/runs/example-os-001/outputs/nvke-node-image-amd64.qcow2
   ```

   The file is read-only and its record is `.nvkr/runs/example-os-001/lifecycle.json`. It is an unqualified local build: NVKR does not sign it or publish it to a registry. Copy it to where your provisioning system serves images, and use it to [Prepare nodes](prepare-nodes.md).

To free disk space after you copy the image, move the attempt's directories under `.nvkr/inputs`, `.nvkr/bootstrap` and `.nvkr/runs` to an archive together. An archived attempt name still cannot be reused.

## Package OS profiles for the management plane

The management plane provisions each node from an OS profile. An OS profile pairs a machine image, which tells NVIDIA Infra Controller (NICo) how to boot a machine, with the cloud-init user data the machine runs on first boot. The `osProfile` field of a cluster or node pool names one. Config profiles set node labels and the hook scripts that run before a node joins; the optional `configProfile` field names one.

You publish OS profiles to the management cluster as three Kubernetes resources per profile, `MachineImage`, `ClusterRelease` and `NodePoolRelease`, all with the profile's name. Control-plane nodes use the `ClusterRelease` and workers use the `NodePoolRelease`. Config profiles and hook scripts come from a profile content image that the controller copies in at startup. Prepare both now; you load them when you install the management-plane services.

1. Clone the NVKE repository and copy the example profile bundle.

   ```bash
   git clone https://github.com/dsx-ai-factory/nvke.git
   ```

   ```bash
   cp -R nvke/controller/profiles/example example-profiles
   ```

   The bundle holds one OS profile, `example-ubuntu-24.04-amd64`, one config profile, `example-worker`, a hook script and a `Dockerfile`.

2. Replace every `REPLACE_` value, and rename the profile if you need to. Then confirm that none remain.

   | Value | File | What it is and where you get it |
   |---|---|---|
   | `REPLACE_KUBERNETES_VERSION` | `profiles/osProfiles/metadata.yaml` | The Kubernetes version the profile's nodes run, as `X.Y.Z` with no leading `v`, such as `1.34.2`. When you upgrade a cluster, NVKE looks for a profile with this version and the same architecture and Ubuntu version. |
   | `REPLACE_BUILD_DATE` | `profiles/osProfiles/metadata.yaml` | The date you built the profile, as `YYYY-MM-DD`. When several profiles match an upgrade, NVKE picks the latest date. |
   | `REPLACE_BOOT_ASSET_BASE_URL` | `metadata.yaml` and `ipxeScript` | An HTTP URL that your NICo machines reach and that serves `vmlinuz` and `initrd`, the Ubuntu installer kernel and initial RAM disk that `ipxeScript` loads. NVKE does not read the copy in `metadata.yaml`. |
   | `REPLACE_UBUNTU_24_04_SERVER_ISO_URL` | `ipxeScript` | The URL of the Ubuntu 24.04 server installer ISO, which the installer downloads at boot. |
   | `REPLACE_APT_REPOSITORY_SETUP_COMMAND` | `userData.tmpl` | A shell command that adds an apt repository serving the NVKE node packages. NVKE publishes no such repository; you host one. |
   | `REPLACE_APT_PACKAGES` | `userData.tmpl` | The packages to install before the CNI packages: `nvke-agent`, `spire-agent` and `nvke-kubeadm-distro-{{ trimPrefix .KubernetesVersion "v" }}`, which selects the cluster's Kubernetes version. |
   | `REPLACE_SYSCTL_VALUE` | `profiles/configProfiles/example-worker.yaml` | The value the example hook script writes for `net.ipv4.ip_forward`. |

   ```bash
   test -z "$(grep -R 'REPLACE_' example-profiles)"
   ```

   The example profile installs Ubuntu 24.04 from the installer ISO, not the Ubuntu 26.04.1 node image that NVKR builds, and installs `nvke-agent` and the Kubernetes packages with apt. A profile for the NVKR image sets `ubuntu_version` in `metadata.yaml` to match the image, and installs the packages the image stages, as [Prepare nodes](prepare-nodes.md) does. To boot the NVKR image, `ipxeScript` must write the QCOW2 file to the machine's disk. Your NICo site provides that script and the loader it runs; this release has no reference script. See [Preview status and known limitations](../overview/preview-status.md).

3. Reference the values that NVKE fills in, as described in [What NVKE puts in the user data](#what-nvke-puts-in-the-user-data). The example's user data writes the `nvke-agent` files but no SPIRE configuration. A profile for the NVKR node image adds the SPIRE files shown there.

4. Generate the OS profile resources.

   ```bash
   go -C nvke/controller run ./hack/osprofiles-to-crs \
     --profiles "$PWD/example-profiles/profiles" \
     --output "$PWD/example-releases" \
     --license Apache-2.0 --gitops=false
   ```

   The command writes `<PROFILE>-machineimage.yaml`, `<PROFILE>-clusterrelease.yaml` and `<PROFILE>-nodepoolrelease.yaml` for each profile into `example-releases`, after it validates each one against the resource schemas. Without `--license Apache-2.0`, the file headers carry a proprietary NVIDIA license identifier. The specs of these resources are immutable once applied: to change an image or its user data, publish a profile with a new name.

5. Build and push the profile content image.

   ```bash
   docker build -t <REGISTRY_HOST>/<NAMESPACE>/nvke-profiles:<TAG> example-profiles
   ```

   ```bash
   docker push <REGISTRY_HOST>/<NAMESPACE>/nvke-profiles:<TAG>
   ```

   `<TAG>` is a version you choose for this set of profiles, such as `v1`. Note the digest that `docker push` prints.

6. Record the controller chart values that load the content image. You set them when you install the management-plane services.

   ```yaml
   profiles:
     path: /profiles
     image:
       enabled: true
       repository: <REGISTRY_HOST>/<NAMESPACE>/nvke-profiles
       digest: <PROFILES_DIGEST>
   ```

   `<PROFILES_DIGEST>` is the `sha256:` digest from step 5. A digest pin restarts the controller when the profiles change; `tag` also works when `digest` is empty. An init container named `materialize-profiles` copies `/profiles` from the image into `profiles.path`. If the registry is private, add an `imagePullSecrets` entry that can pull the image.

   > **Note:** The example content image runs as root, and the chart's default pod security context (`runAsNonRoot: true`) refuses to start it. The install in [Install management-plane services](install-management-plane-services.md) already sets `podSecurityContext: null`, which allows it, and lifts the same restriction from the controller. If you install the chart another way, set `podSecurityContext: null` too.

You apply the manifests in `example-releases` to the management cluster in [Install management-plane services](install-management-plane-services.md).

### What NVKE puts in the user data

NVKE renders `userData.tmpl` as a Go template for each node it creates, and `ipxeScript` the same way. You do not write the values below into the profile; you reference them, and NVKE fills them in.

| Template value | What NVKE fills in |
|---|---|
| `{{ .Hostname }}` | The node's name. |
| `{{ .Username }}`, `{{ .HashedPassword }}` | The node login account from the install's `install-config.json`, with the password as a SHA-512 crypt hash. |
| `{{ .Files }}` | Files for `nvke-agent`, written with `write_files`: `/etc/default/nvke-agent`, which sets its port, `15005` on the documented install. |
| `{{ .KubernetesVersion }}` | The Kubernetes version of the cluster or node pool. |
| `{{ .CNIImagePackage }}`, `{{ .CNIChartsPackage }}` | The CNI packages for the cluster's `cniPlugin` and `cniVersion`, such as `nvke-cni-ovn-images-1.4.0` and `nvke-cni-ovn-charts-1.4.0`. `{{ .OvnEnabled }}` and `{{ .CalicoEnabled }}` say which plugin. |
| `{{ .SpireServerAddress }}`, `{{ .SpireServerPort }}` | Where the node reaches the management cluster's SPIRE server: `spiffe.spire.serverAddress` from the install's `controller.yaml`, which is `<MANAGEMENT_ADDRESS>`, and port `30440`. |
| `{{ .TrustDomain }}`, `{{ .TrustBundle }}` | The management plane's SPIFFE trust domain, and the PEM trust bundle of the management cluster's SPIRE server. |
| `{{ .DiskSelectionStrategy }}` | The node pool's `diskSelectionStrategy`, `smallest` or `largest`. Available in `ipxeScript`. |

Workload cluster nodes attest to the management cluster's SPIRE server, not to the SPIRE server that the bootstrap ran, so they do not use the CA from [Prepare nodes](prepare-nodes.md). The install configures that server with no upstream CA, so it signs with a CA it creates itself, and you supply none. NVKE reads the server's current trust bundle each time it creates a node and passes it in `{{ .TrustBundle }}`.

A profile for the NVKR node image writes the SPIRE values to the files the image reads on first boot. Indent the block to match the rest of your `write_files` list:

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

`indent 6` adds six spaces to each line of the bundle so that it sits under `content`. The install turns SPIFFE on, so the management plane reaches `nvke-agent` over mutual TLS with a SPIFFE identity: a node whose SPIRE agent cannot attest never becomes reachable.

## Verify

1. Confirm that each NVKE component is in your registry: `nvkr release browse` succeeds against the Release reference and lists the `nvkectl`, `cwe` and `nvke-helm` outputs.

2. Confirm that the node image exists.

   ```bash
   ls -l .nvkr/runs/example-os-001/outputs/
   ```

   The directory contains `nvke-node-image-amd64.qcow2`, or the `arm64` file for an ARM64 build.

3. Confirm that `example-releases` holds a `<PROFILE>-machineimage.yaml`, `<PROFILE>-clusterrelease.yaml` and `<PROFILE>-nodepoolrelease.yaml` file for each profile.

## Next steps

- [Prepare nodes](prepare-nodes.md) from the node image.
- [Bootstrap the management cluster](bootstrap-management-cluster.md) with the controller image you published.
