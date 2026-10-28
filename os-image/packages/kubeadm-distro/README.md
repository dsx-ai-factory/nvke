<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# kubeadm-distro

Builds `nvke-kubeadm-distro-<x.y.z>` Debian packages, one per Kubernetes patch
release. Each carries kubelet, kubeadm, kubectl, containerd, runc, the CNI
plugins, crictl, Helm and the control-plane images for that release. The
Packer build stages the packages and a flat apt index in the OS image, and a
node installs the one it needs at cluster create without network access.

## How it is built

- **dpkg owns every file.** Nothing is unpacked, generated or downloaded by a
  maintainer script. `postinst` only sets OS state kubeadm's preflight needs
  (swap, sysctls, kernel modules), enables units, and imports the bundled
  images with `ctr`. It never calls a package manager.
- **Every input is pinned and reviewed.** `lock/<arch>/<version>.lock` holds the
  sha256 of every upstream artifact and the manifest digest of every image.
  `make debs` refuses to build without a lock, checks each download against
  both the lock and the publisher's checksum, and pulls images by digest.
  runc's checksum list is OpenPGP clearsigned and verified against
  `packaging/keyrings/runc.keyring`.
- **Builds are reproducible.** `make repro-check` builds twice and compares
  every byte. `nfpm.yaml` pins mtimes and compression so this passes.
- **Exactly one version is installed at a time.** The Kubernetes patch is part
  of the package name; every package `Provides: nvke-kubeadm-distro` and
  `Conflicts` with its peers, with `containerd`, and with the standalone
  `kubeadm`/`kubelet`/`kubectl` packages.
- **Linux build host, matching the target architecture.** The build runs the
  downloaded `kubeadm` binary for its image list, so `ARCH=arm64` needs an
  arm64 host. `amd64` and `arm64` are both locked.

## Layout

| File | What it is |
|---|---|
| [`bundle.sh`](bundle.sh) | Download, verify and stage one version. The `pins_for` table maps a Kubernetes minor to its containerd, runc and Helm releases. |
| [`packaging/nfpm.yaml`](packaging/nfpm.yaml) | The package: file list, `Depends`, `Conflicts`, conffiles. |
| [`packaging/postinst`](packaging/postinst), [`prerm`](packaging/prerm), [`postrm`](packaging/postrm) | Maintainer scripts. `prerm` disables units before dpkg deletes them; `postrm` runs after the files are gone, so it can only `daemon-reload` and remove the drop-ins. |
| [`packaging/build-deb.sh`](packaging/build-deb.sh) | Turns a staged tree into a `.deb` with nfpm. |
| [`lock/`](lock/README.md) | The committed pins and how to regenerate them. |
| [`test/`](test/) | Go tests that run `bundle.sh` against a local fixture server and build real debs with `build-deb.sh`. |

## What a package contains

| Path | Contents |
|---|---|
| `/usr/bin/{kubeadm,kubectl,kubelet,crictl,helm}`, `/usr/sbin/runc` | pkgs.k8s.io-style binary paths |
| `/usr/local/bin/{containerd,containerd-shim-runc-v2,ctr}` | containerd, from its release tarball |
| `/opt/cni/bin/` | the CNI plugins |
| `/etc/containerd/config.toml` | containerd 2.x config, rendered at build time with the sandbox image pinned to this release's pause image. Rewritten on every install; put local overrides in `/etc/containerd/conf.d/*.toml`, which it imports |
| `/etc/crictl.yaml` | crictl pointed at the containerd socket. Also rewritten on every install |
| `/etc/default/kubelet` | conffile for operator `KUBELET_EXTRA_ARGS` |
| `/lib/systemd/system/kubelet.service{,.d/10-kubeadm.conf}`, `containerd.service` | vendor units plus the kubeadm drop-in |
| `/usr/lib/nvke/k8s-matrix/<version>/<arch>/` | the control-plane image tarball and its `.sha256` sidecar |

`/etc/cni/net.d/` is shipped empty. CNI config is the OS image's job.

The package `Depends` on `iptables, iproute2, conntrack, socat, ethtool`.
kubeadm's preflight errors without `conntrack` and warns without `socat` or
`ethtool`.

The bundled images are the ones kubeadm asks for by default, from
`registry.k8s.io`. A controller whose `kubeadmImageRepository` points at a
mirror makes kubeadm ask for different names, so the node pulls from the mirror
and is no longer offline.

## Building

```bash
apt install dpkg-dev gnupg     # dpkg-scanpackages for the apt index; gpg for runc's signature
make -C os-image/packages/kubeadm-distro debs OUT_DIR=/abs/path/to/apt-repo
make -C os-image/packages/kubeadm-distro test
make -C os-image/packages/kubeadm-distro repro-check
```

`OUT_DIR` is required and must be absolute. `nfpm` is `go install`ed into
`./bin` at the version in
[`make/tool-versions.mk`](../../../make/tool-versions.mk). `crane` is
downloaded from its GitHub release at the same pinned version, and its
checksum is verified. `curl`, `gpg`, `gzip`, `sha256sum` and `tar` must be on
`PATH`; `make debs` checks all of this before downloading anything.

| Variable | Default | Purpose |
|---|---|---|
| `K8S_VERSIONS` | `v1.37.2 v1.36.3 v1.35.3 v1.34.2` | Kubernetes releases to build |
| `DEB_REVISION` | `1` | Debian packaging revision; the Kubernetes patch is in the package name |
| `LOCK_DIR` | `./lock` | The committed lock files |
| `DIST_DIR` | `./dist` | Artifact cache. Anything that still hashes to its lock entry is reused offline; anything else is re-downloaded and re-verified |

`repro-check` is not in CI. Run it by hand after touching `packaging/` or
`build-deb.sh`. A failure usually means a new content entry carries
build-host metadata (a timestamp, a path, ownership) that nfpm is not pinning.

## Bumping versions

1. Edit `K8S_VERSIONS` in the [Makefile](Makefile): the newest patch of each of
   the three newest minors. v1.34.2 stays as a fourth until the CI and dev
   defaults move off it.
2. If the bump crosses a minor, add a row to `pins_for` in `bundle.sh`. There
   is no fallback row, so check containerd's Kubernetes support matrix and
   Helm's version skew policy.
3. `make lock` on a Linux host with network, once per architecture:
   `make lock` on amd64 and `make lock ARCH=arm64` on arm64. Each rewrites
   `lock/<arch>/<version>.lock` and prints what changed.
4. Review the diff with `git diff -- lock/`, every URL and every digest.
5. Commit `lock/` with the `K8S_VERSIONS` and `bundle.sh` changes.

## Provenance

Ported from the internal `nke/kubeadm-bundler` repository, which builds the
packages served from the S3 apt repository. The intended differences: dpkg owns
every file (no `nke-k8s-installer`, no raw tarballs under
`/usr/lib/nvke/k8s-matrix`), `config.toml` and `containerd.service` are rendered
at build time, `Depends` adds `conntrack`, `socat` and `ethtool`, `Conflicts`
adds `containerd` and `containerd.io`, and `/etc/default/kubelet` is a
conffile. Package names, `Provides`, peer `Conflicts`, binaries, paths, the
image archive and the systemd units are unchanged. Dropped: S3 delivery, the
manual tarball and the internal customer flavors.
