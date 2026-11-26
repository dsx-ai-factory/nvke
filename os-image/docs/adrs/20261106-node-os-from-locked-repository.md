# Build the node OS from a locked, single-origin apt repository

**Status:** proposed
**Date:** 2026-11-06

## Context

Packer boots the Ubuntu Server installer ([`packer/`](../../packer/)). Autoinstall lays out the disk and installs the base system, and [`provision.sh`](../../packer/scripts/provision.sh) installs NVKE's debs and configures the system by script. Package versions are whatever the ISO and the archive serve at bake time. The [apt acquisition capture](../apt-acquisition-capture.md) records afterwards what was fetched, but it pins nothing in advance.

As a result, two bakes of the same commit can differ. Nothing can tell whether a running node still holds the release it was imaged with, and the only way to change a node is to reimage it. A team that needs another disk layout or one more package has to fork the Packer project. The Kubernetes streams and CNI debs reach the node as a flat repository staged inside the image.

[`docs/design/byo-os.md`](../../../docs/design/byo-os.md) proposes that operators may build their own OS against a readiness contract that starts with an NVKE apt repository plus `nvke-agent` and `spire-agent`. That requires the repository to be an artifact in its own right, not a by-product of the image bake.

NVKR builds and publishes the node disks today by running this Packer project.

## Decision

We will define each node OS release in a hand-written release spec, resolve it into a committed lock, publish exactly the locked package set as a signed single-origin apt repository, build images only from that repository, and verify every image against the lock.

- **Spec, lock and manifest.** Each document has one producer:
  - The spec (`apiVersion: nvke.dev/v1`, `kind: Release`) states intent. It carries no version and no snapshot ID.
  - The lock pins the Ubuntu snapshot, the third-party mirror digests and every package's version and SHA256, per architecture.
  - The manifest and a CycloneDX SBOM record what an image holds.

  An `os-image/v*` tag on the spec and lock names a release, and `git describe` gives the version.
- **apt is the only resolver.** The lock is apt's own answer, resolved against the pinned sources in an isolated root. Verify is then a set comparison, not a prediction of what apt will do: the packages installed in an image or on a node must be a subset of the lock.
- **One origin.**
  - A release is one immutable suite with the components `main`, `security` and `kubernetes`. It holds Ubuntu at a snapshot, pinned third-party packages and our own debs. Our own debs, `nvke-agent` included, are built from the release's commit before the repository is assembled, and the lock records them like any other package.
  - A pointer package, `nvke-release`, carries the sources, keyring and apt settings with no hostname in them; the site writes the mirror list at first boot.
- **The OS is packages.**
  - Metapackages are the content of the OS: `nvke-node-base` plus one per named group.
  - Image configuration ships as tested debs rather than as build scripts.
  - Storage and sshd configuration, which are tightly coupled to the image, sit behind virtual package names that a downstream can replace.
- **Replaceable seams.** Resolution, the repository engine and the image builder each sit behind a narrow boundary. Packer is the first builder. Others, a downstream's own included, plug into the same contract and pass the same verify.
- **One CLI, `nvke-os`, in this module.** It follows the command, flag, error and exit-code conventions of `nvkr` but stays a separate tool. NVKR invokes it and keeps signing, distribution and remote publishing. The node-side verbs ship as a separate small binary.

In-place update of running nodes and patch releases build on these artifacts and are decided separately.

## Consequences

What gets easier:
- A release rebuilds with the same package set for as long as its snapshot or its published repository is kept, and a reviewer reads every change to the spec as a lock diff.
- Verify answers "is this image, or this node, the release we shipped" at package level: a package the lock does not hold shows up as foreign, and a version it does not name shows up as drift.
- The repository and pointer package are what BYO-OS phase 1 needs, and the same artifacts serve a later in-place update.
- A downstream extends a spec instead of forking the Packer project.

What gets harder, and what this gives up:
- snapshot.ubuntu.com keeps snapshots for at least two years, which bounds how long a lock can be re-resolved from upstream. Third-party sources have no snapshot service. The lock pins each package's SHA256, so a rebuild gets the same bytes or fails. Guaranteeing a rebuild for the whole support window means keeping the release's published repository, which already holds every deb the release installs.
- A node must consume only the in-site repository, and every site has to host one. Adding another source leaves the verified path, by design.
- apt's validity checks are off for our source so that frozen and air-gapped repositories never expire. An old signed suite can therefore be replayed; consumers name an explicit suite and verify against its lock.
- The installer installs from its own ISO pool, so making Packer install only from the suite is new work, and NVKR's disk build changes with it.
- Script-based image configuration has to be re-expressed as packages.
- The image keeps a bootstrap cache of the Kubernetes streams NVKE names, published in the `kubernetes` component. The base metapackage depends on none of them, which keeps Kubernetes out of the OS as BYO-OS proposes.

## Options considered

- **Locked spec, single-origin repository, verify (chosen):** reproducible and verifiable, and one artifact set serves images, BYO-OS operators and in-place updates.
- **Keep the installer bake with acquisition capture:** records what happened, but cannot pin a release in advance or verify a node later.
- **Resolve with aptly's dependency filter:** adds a second resolver beside apt, so verify has to predict apt and reports false drift when the two disagree.
- **mkosi as the first builder:** a cleaner build, but it would change the disk path that NVKR and sites rely on at the same time as the package model. It can follow behind the builder contract.
- **Build the OS inside `nvkr`:** one tool fewer, but NVKR orchestrates many producers, and downstream OS teams need the OS tool without adopting the rest of NVKR.

## Example release spec

The first milestone's spec, from the build design. It states intent only. The lock adds the snapshot ID and every package's version and SHA256, per architecture.

```yaml
apiVersion: nvke.dev/v1
kind: Release
metadata:
  name: nvke-node
  # no version field: the git tag names the release; git describe supplies it to render and publish
  series: noble
spec:
  architectures: [amd64, arm64]

  inputs:                     # read by lock and publish
    ubuntu:
      # no snapshot ID: the lock pins the snapshot it resolved against
      pockets: [release, updates, security]
    mirrors: []               # third-party inputs: none today. A future example, the
                              # DOCA host packages the DSX CSC DPU stack needs:
                              # - name: doca
                              #   url: https://linux.mellanox.com/public/repo/doca/3.2.0/ubuntu24.04/
                              #   suite: ./
                              #   keyring: keyrings/mellanox.gpg
    local:
      - {name: nvke, path: out/debs}   # our own debs, built from the release's commit
    locks:
      - os-image/packages/spire-agent/lock
      - os-image/packages/kubeadm-distro/lock   # lock/<arch>/v<patch>.lock, one per stream below
      - os-image/packages/cni-calico/lock
      - os-image/packages/cni-ovn/lock

  kernel:                     # read by the image build
    amd64: linux-nvidia
    arm64: linux-nvidia-64k

  pointer:                    # rendered into nvke-release
    name: nvke-release
    sources: {mirrorList: /etc/nvke/apt-mirrors.txt, components: [main, security, kubernetes]}
    pins: [{component: kubernetes, priority: 90}]   # install on request only; dist-upgrade never moves it
    keys: {current: keys/nvke-archive-2026.pub, next: keys/nvke-archive-2027.pub}
    apt: {installRecommends: false, checkValidUntil: false, checkDate: false}

  packages:                   # rendered into the metapackages
    base:
      name: nvke-node-base
      depends: [ubuntu-server-minimal, nvke-agent, spire-agent,
                nvke-node-sysconfig, nvke-node-boot, nvke-node-cloudinit,
                lvm2, openssh-server, python3-apt, tpm2-tools,
                openvswitch-switch, iptables, conntrack, socat, ethtool]
      conflicts: [snapd, unattended-upgrades]
    components:
      storage: {provides: nvke-node-storage, default: nvke-node-storage-lvm,
                alternatives: [nvke-node-storage-lvm-luks]}

  kubernetes:                 # the catalog the kubernetes component carries and the image caches
    streams: [1.36.3, 1.35.3, 1.34.2]   # nvke-kubeadm-distro-<patch>, each built from its input lock
    cni:
      calico: [3.30.1]                  # nvke-cni-calico-{manifests,images}-<stream>
      ovn: [2.4.2]                      # nvke-cni-ovn-{images,charts}-<stream>
    # nothing here is a Depends of the base: NVKE installs one stream by name, the pin keeps apt away

  metapackages:               # each group renders nvke-node-<group>; the base depends on every group
    kata: ["erofs-utils (>= 1.9)", "kata-containers (>= 3.12)"]

  patch:                      # read by lock (the patch gate) and publish (the security filter)
    exclude: ["linux-*", rdma-core, "libibverbs*", nvidia-container-toolkit]

  verify:                     # read by verify
    foreignPackages: fail
    allowForeign: []
    components:               # expected set = the lock + these
      kubernetes: installed-stream   # the installed nvke-kubeadm-distro stream and its CNI debs
```

What each section decides:

- `inputs`: where packages come from. Ubuntu pockets at a snapshot, third-party mirrors, our own debs, and the per-component locks that already pin SPIRE, the Kubernetes streams and the CNI debs.
- `kernel`: the kernel package per architecture.
- `pointer`: the contents of `nvke-release`, the sources, keyring and apt settings every node gets. The site supplies only the mirror list.
- `packages`: `nvke-node-base` and its Depends and Conflicts, plus `storage`, a component behind a virtual name that a downstream can replace.
- `kubernetes`: the streams and CNI debs published in the `kubernetes` component and cached in the image. The base depends on none of them.
- `metapackages`: named groups, each a metapackage the base depends on. A package leaves a node only when no group holds it.
- `patch.exclude`: packages only a full release may move, never a patch.
- `verify`: how a manifest is compared with the lock, and what to do with a package the lock does not hold.
