# nke-cni-ovn

Builds the two Debian packages `nke-agent` installs when it sets up OVN:

| Package | Contents | Installed to |
|---|---|---|
| `nke-cni-ovn-charts-<stream>` | The nv-ngn `ovn-kubernetes` chart, unpacked | `/etc/dgx/ovn-kubernetes/` |
| `nke-cni-ovn-images-<stream>` | `ovn-c` and `ovnkube-c`, as one docker-archive | `/usr/lib/nke/cni-ovn/<stream>/`, imported into containerd by `postinst` |

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

`values-nke.yaml` runs OVS on the host (`ovs-node: false`), so the node image
installs `openvswitch-switch`; the agent checks for `ovs-vsctl` before helm.

Every stream unpacks to the same directory, so the charts package `Provides`
and `Conflicts` with `nke-cni-ovn-charts`: one stream per node.

## Access

The chart comes from `helm.ngc.nvidia.com/nv-ngn/sdn` and the images from
`nvcr.io/nv-ngn/sdn`. Both need an NGC API key with access to the `nv-ngn` org,
in `NGC_API_KEY`, for `make lock` and for every build. `fetch-artifacts.sh`
skips these debs when the key is unset, so a CI image has no OVN packages until
the workflow gets that secret.

## Building

```bash
NGC_API_KEY=... make debs OUT_DIR=/absolute/path
```

Linux only. `debs` runs `bundle` first, which verifies the chart and every
image against `lock/<stream>.lock` before anything is unpacked, and reindexes
`OUT_DIR` with `dpkg-scanpackages` when it finishes. `make repro-check` builds
twice and compares every `.deb` byte-for-byte.

## Bumping the stream

Set `OVN_STREAMS`, refresh the lock, review it, commit it:

```bash
NGC_API_KEY=... make lock OVN_STREAMS=1.4.5
git diff -- lock
```

The image *list* comes from the lock's existing `image` lines. Check them
against the new chart's `values-images.yaml`: with `values-nke.yaml` only
`ovnkube-control-plane` and `ovnkube-single-node-zone` run, so `ovn-c` and
`ovnkube-c` at the tag that file pins are all it needs. The digests `make lock`
writes should match the `sha256` fields beside those tags.

Keep the default stream in step with `defaultOvnCniChartStream` in
`agent/internal/agent/cni.go`.

## Size

`ovn-c` and `ovnkube-c` unpack to about 2.2 GB together, so the images deb is
the largest thing in the node image. The other charts in the legacy bundle
(multus, sriov-device-plugin, sdn-mutator, sdn-monitoring, ovn-sdn-debug) and
their images are not packaged: nothing in this repository installs them.
