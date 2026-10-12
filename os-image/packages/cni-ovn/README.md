# nke-cni-ovn

Builds the two Debian packages `nke-agent` installs when it sets up OVN:

| Package | Contents | Installed to |
|---|---|---|
| `nke-cni-ovn-charts-<stream>` | The `ovn-kubernetes` Helm chart, unpacked | `/etc/dgx/ovn-kubernetes/` |
| `nke-cni-ovn-images-<stream>` | `ovnkube`, as one docker-archive | `/usr/lib/nke/cni-ovn/<stream>/`, imported into containerd by `postinst` |

`provision.sh` stages both into the image under `/usr/lib/nke/debs` as a local
apt repo. Nothing is installed at bake time: `nke-agent` installs them by name
at cluster create. The build machinery is shared with `cni-calico` in
[`../cni-common/`](../cni-common/README.md); this directory holds only what is
OVN's: `bundle.sh`, the charts spec and the lock.

Both are built from source: the chart and the image come from NVIDIA's
[dsx-ai-factory/ovn-kubernetes](https://github.com/dsx-ai-factory/ovn-kubernetes)
at a commit the lock pins. Nothing is pulled from ghcr.io or Artifactory.

## The contract with nke-agent

`agent/internal/agent/cni.go` asks apt for `nke-cni-ovn-charts-<stream>` and
`nke-cni-ovn-images-<stream>`, where `<stream>` is the request's version or
`defaultOvnCniChartStream`. It runs helm on `/etc/dgx/ovn-kubernetes` when
`values-nke.yaml` is there, else on the directory of the first `Chart.yaml` that
`dpkg-query -L` lists for the charts package. The chart is unpacked at build
time so both paths hold with no maintainer script.

`values-nke.yaml` is written by `bundle.sh`, not shipped by the fork. It sets
`tags.ovs-node: false`, so OVS stays on the host (the node image installs
`openvswitch-switch`; the agent checks for `ovs-vsctl` before helm). It also
points every enabled component at the one image the images package imports into
containerd: `global.imageRepositoryPrefix: localhost/nke`, so the chart's
`ovnkube` repository becomes `localhost/nke/ovnkube`, and each component's
`image.tag` is the first 12 characters of the locked commit. The chart's
defaults are its `values.yaml`, which helm must be given before
`values-nke.yaml`. `bundle.sh` renders the chart with helm when it is installed
and fails if the result names any image other than the embedded one, so a new
image key in the fork cannot ship as a registry pull.

The agent's registry override (`global.registry`, or the server's
`CniImageRegistry`) rewrites `global.imageRepositoryPrefix`, keeping the path
after the host.

Every stream unpacks to the same directory, so the charts package `Provides`
and `Conflicts` with `nke-cni-ovn-charts`: one stream per node.

## Source and access

`bundle.sh` fetches `https://api.github.com/repos/dsx-ai-factory/ovn-kubernetes/tarball/<commit>`
and verifies it against the lock's `artifact` line. The chart is
`charts/ovn-kubernetes/` in that tree (upstream's is `helm/ovn-kubernetes/`) and
the image is built from `dist/images/Dockerfile.ubuntu`, the variant on the
public `ubuntu` base. The `ubuntu-nv-image` variant needs Artifactory and is not
built here.

While the fork is internal, set `GITHUB_TOKEN` to a token that can read it. It
goes to curl through a config file, never the command line, and is dropped on
the redirect to codeload. Once the fork is public the variable is optional.

The lock pins the source, not the image: the `Dockerfile.ubuntu` build runs
`apt-get` and fetches the kubectl key, so no registry digest exists to pin. The
build pins timestamps instead, and `make repro-check` is what shows that two
builds of the same lock agree.

## Building

```bash
make debs OUT_DIR=/absolute/path
```

Linux only. `debs` runs `bundle` first, which verifies the source against
`lock/<arch>/<stream>.lock`, builds the image, and reindexes `OUT_DIR` with
`dpkg-scanpackages` when it finishes. `make repro-check` builds twice and
compares every `.deb` byte-for-byte.

The image build needs docker with buildx, or podman (`OCI_BIN=podman`), and
network access to `quay.io` (the Go builder), Docker Hub (`ubuntu`), the Ubuntu
archive and pkgs.k8s.io. Override the bases with `OVN_BUILDER_IMAGE` and
`OVN_UBUNTU_BASE_IMAGE`. A build for the other architecture runs under
emulation. `GITHUB_TOKEN` is needed while the fork is internal.

## Bumping the stream

`OVN_STREAMS` is `X.Y.0`: the chart's version, built from the fork's
`release-X.Y.0` branch. `make lock` resolves that branch to a commit and records
it with the tarball's checksum. Lock amd64 and arm64 from the same ref, review,
commit:

```bash
export GITHUB_TOKEN=...   # while the fork is internal
make lock OVN_STREAMS=1.7.0
make lock OVN_STREAMS=1.7.0 ARCH=arm64
git diff -- lock
```

`OVN_KUBERNETES_REF` locks another branch or a commit instead. Keep the default
stream in step with `defaultOvnCniChartStream` in `agent/internal/agent/cni.go`.

## Size

`ovnkube` is the only image: every OVN role runs from it, so the
images package is on the order of 350 MiB. `ovs-node`, `ovn-ipsec` and the DPU
subcharts ship in the chart but stay tagged off.
