# cni-common

Shared by [`cni-calico/`](../cni-calico/README.md) and
[`cni-ovn/`](../cni-ovn/README.md), which differ only in what they fetch.

| File | Role |
|---|---|
| `bundle-lib.sh` | Sourced by each `bundle.sh`: arguments, the lock file, and the images pulled by digest and repacked deterministically |
| `build-deb.sh` | Builds the content deb from the package's own spec and the images deb from the spec here |
| `deb.mk` | The targets: `debs`, `bundle`, `lock`, `repro-check`, the pinned `crane` and `nfpm`, host checks |
| `nfpm-images.yaml`, `postinst` | The images deb: one docker-archive under `/usr/lib/nvke/<cni>/<stream>/`, imported into containerd |

A package is four things: a `Makefile` that sets `CNI`, `PRODUCT`, `VERSIONS`,
`CONTENT_SPEC` and `CONTENT_KIND` and includes `deb.mk`; a `bundle.sh` that
stages its artifacts with `fetch` and `verify_artifact`, then calls
`stage_images` and `finish_bundle`; the content deb's nfpm spec under
`packaging/`; and `lock/<arch>/<version>.lock`.

The lock has one `artifact <name> <sha256> <url>` line per fetched file and one
`image <ref> <digest>` line per image. `make lock` rewrites the digests from
upstream, with the image list seeded by hand.

A build fetches images at the digest the lock names and never resolves the tag,
so what upstream has retagged since does not reach it. Fetched files are still
verified by sha256 after download, because a URL has no digest to fetch by. A
build does read the tag, through the configured mirror, to report one that has
moved away from its locked digest — on stderr and without failing, as a prompt
to review a `make lock` refresh rather than a broken build. Only `make lock`
lets a tag decide what is fetched, so only a reviewed commit changes what gets
built.

One lock per architecture, because a digest resolves per platform. `make lock`
takes the image list from the amd64 lock for every other architecture, so the
two cannot drift apart, and `ARCH=arm64` needs amd64 locked first. Nothing here
runs a downloaded binary, so an arm64 build works on an amd64 host.

The images debs `Provides` and `Conflicts` with their unversioned name, so a
node holds one stream per CNI. They depend on `containerd | nvke-kubeadm-distro`:
the distro package ships its own containerd and conflicts with Ubuntu's.

Set `CNI_DOCKERHUB_MIRROR` to a registry host when Docker Hub reads must use a
pull-through mirror. The mirror changes only Crane's transport reference. The
committed lock and the image names imported on the node remain `docker.io/...`.
A build uses the mirror when it serves the locked digest, and the canonical
`docker.io` reference otherwise; `make lock` resolves the tag through the mirror
first and falls back the same way. Either route transfers the same digest.
