<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Lock files

One `<arch>/<version>.lock` per Kubernetes version in `K8S_VERSIONS` per
architecture, for example `amd64/v1.36.3.lock`. Every artifact URL and every
image digest is architecture-specific, so one file per version could hold only
one of them. Plain text, one sorted record per line, pinning by digest
everything the bundle for that version and architecture is built from:

```text
# nvke-kubeadm-distro lock for v1.36.3 (amd64). Regenerate with `make lock`; review the diff before committing.
kubernetes v1.36.3
artifact kubeadm <sha256hex> https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubeadm
image registry.k8s.io/pause:3.10.2 sha256:<hex>
```

The `kubernetes` record has to match the version being built, so a lock that
is renamed or copied to the wrong filename is rejected rather than used.

These files are **generated, reviewed and committed**, never hand-written:

```bash
make lock                 # amd64, on an amd64 Linux host with network
make lock ARCH=arm64      # arm64, on an arm64 host: bundle.sh runs kubeadm
git diff -- lock/         # read every changed URL and digest
```

`make debs` refuses to build a version with no lock, checks every downloaded
artifact against the locked `sha256` on top of the publisher's own checksum,
and pulls each image by `ref@digest` rather than by tag. A mutable tag or a
rebuilt upstream release therefore cannot change what ships without showing up
as a diff here first, and the `kubeadm` binary is verified against a reviewed
digest *before* the build host executes it to list the images.

A version listed in `K8S_VERSIONS` with no file under an architecture has not
been locked for it yet, and `make debs` for that architecture refuses to run.
