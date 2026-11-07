# Build the node OS from a locked, single-origin apt repository

**Status:** proposed
**Date:** 2026-11-06

## Context

Packer boots the Ubuntu Server installer ([`packer/`](../../packer/)). Autoinstall lays out the disk and installs the base system, and [`provision.sh`](../../packer/scripts/provision.sh) installs NVKE's debs and configures the system by script. Package versions are whatever the ISO and the archive serve at bake time. The [apt acquisition capture](../apt-acquisition-capture.md) records afterwards what was fetched, but it pins nothing in advance.

As a result, two bakes of the same commit can differ. Nothing can tell whether a running node still holds the release it was imaged with, and the only way to change a node is to reimage it. A team that needs another disk layout or one more package has to fork the Packer project. The Kubernetes streams and CNI debs reach the node as a flat repository staged inside the image.

[`docs/design/byo-os.md`](../../../docs/design/byo-os.md) proposes that operators may build their own OS against a readiness contract that starts with an NVKE apt repository plus `nvke-agent` and `spire-agent`. That requires the repository to be an artifact in its own right, not a by-product of the image bake.

NVKR builds and publishes the node disks today by running this Packer project.

## Decision

We will define each node OS release in a hand-written release spec, resolve it into a committed lock, publish exactly the locked package set as a signed single-origin apt repository, build images only from that repository, and verify every image against the lock. The detailed design is in [`docs/design/node-os-build.md`](../../../docs/design/node-os-build.md).

- **Spec, lock and manifest.** Each document has one producer:
  - The spec (`apiVersion: nvke.dev/v1`, `kind: Release`) states intent. It carries no version and no snapshot ID.
  - The lock pins the Ubuntu snapshot, the third-party mirror digests and every package's version and SHA256, per architecture.
  - The manifest and a CycloneDX SBOM record what an image holds.

  An `os-image/v*` tag on the spec and lock names a release, and `git describe` gives the version.
- **apt is the only resolver.** The lock is apt's own answer, resolved against the pinned sources in an isolated root. Verify is then a set comparison, not a prediction of what apt will do.
- **One origin.**
  - A release is one immutable suite with the components `main`, `security` and `kubernetes`. It holds Ubuntu at a snapshot, pinned third-party packages and our own debs.
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
- A release rebuilds with the same package set for as long as its snapshot is retained, and a reviewer reads every change to the spec as a lock diff.
- Verify answers "is this image, or this node, the release we shipped" at package level. Anything installed from outside the repository shows up as foreign packages or drift.
- The repository and pointer package are what BYO-OS phase 1 needs, and the same artifacts serve a later in-place update.
- A downstream extends a spec instead of forking the Packer project.

What gets harder, and what this gives up:
- snapshot.ubuntu.com keeps snapshots for two years, so a release can be rebuilt for two years and no longer. Third-party sources have no snapshot service, so keeping their debs for that window falls to us.
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
