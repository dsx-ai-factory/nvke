<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# The Ubuntu snapshot the image is built from

Every Ubuntu package in the node image comes from one snapshot of the Ubuntu
archive on [snapshot.ubuntu.com](https://snapshot.ubuntu.com), so two builds of
the same commit install the same versions
([#2206](https://github.com/dsx-ai-factory/nvke/issues/2206)). This is
transitional: the release lock (#2204) will take over the snapshot ID, and the
signed suite (#2213) will replace the snapshot sources.

## How the build uses it

- `ubuntu_snapshot` in `packer/variables.pkr.hcl` is the one place the ID
  (`YYYYMMDDTHHMMSSZ`) is pinned. `UBUNTU_SNAPSHOT` overrides it for one build,
  and `UBUNTU_SNAPSHOT_BASE_URL` points the build at a mirror of the service.
- The autoinstall points the installer's primary and security mirrors at the
  snapshot and writes the installed system's sources from an explicit
  template. Without the template, curtin kept the ISO's live archive URI on
  24.04.5 arm64.
- `updates: all` brings every package up to the snapshot. The base system
  comes from the ISO, and the snapshot does not serve the point-release
  versions that `-updates` has since replaced.
- arm64 uses the same `/ubuntu` archive as amd64. The service's
  `/ubuntu-ports` answers 401.

## Image tests

`packer/image-tests/test_ubuntu_snapshot.py` tests the image, not this
repository. `provision.sh` runs it inside the build VM, before it removes apt's
indexes, and the bake fails when:

- apt reads any source other than the snapshot and the local repository
  (`/usr/lib/nvke/debs`), or
- an installed package is at a version neither of them serves. `nvke-agent`,
  installed from its own `.deb`, is exempt.

The release verify step (#2205) will replace these tests. After first boot,
`boot-smoke` checks the sources again.

## What nodes need

The image keeps the snapshot as its Ubuntu source. Nodes do not run apt in the
background, but `nvke-agent` runs `apt-get update` when it adds a third-party
repository or upgrades itself, and Ubuntu dependencies then resolve from the
snapshot. A node therefore needs outbound access to the snapshot host rather
than `archive.ubuntu.com`, and it gets only that snapshot's versions. Canonical
keeps snapshots for at least two years.

## Moving to a new snapshot

Check that the service serves the new ID, then commit it as `ubuntu_snapshot`'s
default:

```bash
curl -fsI https://snapshot.ubuntu.com/ubuntu/<ID>/dists/resolute-updates/InRelease
```

If the build cannot reach the snapshot, the installer aborts at its mirror
check, and Packer reports only "Timeout waiting for SSH" once `ssh_timeout`
passes. The reason is in the installer's serial log,
`build-logs/<variant>-install-console.log`.
