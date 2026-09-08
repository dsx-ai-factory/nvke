# cni-common

Shared by [`cni-calico/`](../cni-calico/README.md) and
[`cni-ovn/`](../cni-ovn/README.md), which differ only in what they fetch.

| File | Role |
|---|---|
| `bundle-lib.sh` | Sourced by each `bundle.sh`: arguments, the lock file, and the images pulled by digest and repacked deterministically |
| `build-deb.sh` | Builds the content deb from the package's own spec and the images deb from the spec here |
| `deb.mk` | The targets: `debs`, `bundle`, `lock`, `repro-check`, the pinned `crane` and `nfpm`, host checks |
| `nfpm-images.yaml`, `postinst` | The images deb: one docker-archive under `/usr/lib/nke/<cni>/<stream>/`, imported into containerd |

A package is four things: a `Makefile` that sets `CNI`, `PRODUCT`, `VERSIONS`,
`CONTENT_SPEC` and `CONTENT_KIND` and includes `deb.mk`; a `bundle.sh` that
stages its artifacts with `fetch` and `verify_artifact`, then calls
`stage_images` and `finish_bundle`; the content deb's nfpm spec under
`packaging/`; and `lock/<version>.lock`.

The lock has one `artifact <name> <sha256> <url>` line per fetched file and one
`image <ref> <digest>` line per image. Builds enforce it; `make lock` rewrites
the digests from upstream, with the image list seeded by hand.

The images debs `Provides` and `Conflicts` with their unversioned name, so a
node holds one stream per CNI. They depend on `containerd | nke-kubeadm-distro`:
the distro package ships its own containerd and conflicts with Ubuntu's.
