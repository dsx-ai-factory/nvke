# nke-cni-ovn

Builds the two Debian packages `nke-agent` installs when it sets up OVN:

| Package | Contents | Installed to |
|---|---|---|
| `nke-cni-ovn-charts-<stream>` | The `ovn-kubernetes` Helm chart, unpacked | `/etc/dgx/ovn-kubernetes/` |
| `nke-cni-ovn-images-<stream>` | `ovn-kube-ubuntu`, as one docker-archive | `/usr/lib/nke/cni-ovn/<stream>/`, imported into containerd by `postinst` |

`provision.sh` stages both into the image under `/usr/lib/nke/debs` as a local
apt repo. Nothing is installed at bake time: `nke-agent` installs them by name
at cluster create. The build machinery is shared with `cni-calico` in
[`../cni-common/`](../cni-common/README.md); this directory holds only what is
OVN's: `bundle.sh`, the charts spec and the lock.

## The contract with nke-agent

`agent/internal/agent/cni.go` asks apt for `nke-cni-ovn-charts-<stream>` and
`nke-cni-ovn-images-<stream>`, where `<stream>` is the request's version or
`defaultOvnCniChartStream`. It runs helm on `/etc/dgx/ovn-kubernetes` when
`values-nke.yaml` is there, else on the directory of the first `Chart.yaml` that
`dpkg-query -L` lists for the charts package. The chart is unpacked at build
time so both paths hold with no maintainer script.

`values-nke.yaml` is written by `bundle.sh`, not shipped upstream. It sets
`tags.ovs-node: false`, so OVS stays on the host (the node image installs
`openvswitch-switch`; the agent checks for `ovs-vsctl` before helm), and pins
`global.image.repository` and `tag` to the one image the images package
imports into containerd. The chart has no `values.yaml`: its defaults are
`values-single-node-zone.yaml`, which helm must be given before
`values-nke.yaml`.

Every stream unpacks to the same directory, so the charts package `Provides`
and `Conflicts` with `nke-cni-ovn-charts`: one stream per node.

## Access

Both come unauthenticated from the public
[github.com/ovn-kubernetes/ovn-kubernetes](https://github.com/ovn-kubernetes/ovn-kubernetes)
project: the chart is `helm/ovn-kubernetes/` in the `v<stream>` source tarball
(there is no chart repository), the image is
`ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu:release-<X.Y>`. That tag
moves with every merge to the branch, so the lock pins the resolved digest and
only `make lock` re-reads it — a re-run adopts whatever has merged there since,
unlike Calico's immutable `vX.Y.Z` refs, so diff the digest change on review.

## Building

```bash
make debs OUT_DIR=/absolute/path
```

Linux only. `debs` runs `bundle` first, which verifies the chart source and
the image against `lock/<arch>/<stream>.lock` before anything is unpacked,
and reindexes `OUT_DIR` with `dpkg-scanpackages` when it finishes. `make
repro-check` builds twice and compares every `.deb` byte-for-byte.

## Bumping the stream

`OVN_STREAMS` is `X.Y.0`: the stream is the chart's release, and the image
tag is `release-X.Y`; `bundle.sh` refuses a lock whose `image` tag differs.
Seed the amd64 lock's `image` line by hand, lock amd64 first (other arches
take the image list from it), review, commit:

```bash
printf 'image ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu:release-1.5\n' >lock/amd64/1.5.0.lock
make lock OVN_STREAMS=1.5.0
make lock OVN_STREAMS=1.5.0 ARCH=arm64
git diff -- lock
```

Keep the default stream in step with `defaultOvnCniChartStream` in
`agent/internal/agent/cni.go`.

## Size

`ovn-kube-ubuntu` is the only image: every OVN role runs from it, so the
images package is about 350 MiB. `ovs-node`, `ovn-ipsec` and the DPU
subcharts ship in the chart but stay tagged off.
