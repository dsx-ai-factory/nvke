# nvke-cni-ovn

Builds the two Debian packages `nvke-agent` installs when it sets up OVN:

| Package | Contents | Installed to |
|---|---|---|
| `nvke-cni-ovn-charts-<stream>` | The `ovn-kubernetes` Helm chart, unpacked | `/etc/dgx/ovn-kubernetes/` |
| `nvke-cni-ovn-images-<stream>` | `ovnkube`, as one docker-archive | `/usr/lib/nvke/cni-ovn/<stream>/`, imported into containerd by `postinst` |

`provision.sh` stages both into the image under `/usr/lib/nvke/debs` as a local
apt repo. Nothing is installed at bake time: `nvke-agent` installs them by name
at cluster create. The build machinery is shared with `cni-calico` in
[`../cni-common/`](../cni-common/README.md); this directory holds only what is
OVN's: `bundle.sh`, the charts spec and the lock.

Both are built from source: the chart and the image come from NVIDIA's
[dsx-ai-factory/ovn-kubernetes](https://github.com/dsx-ai-factory/ovn-kubernetes)
at a commit the lock pins. Nothing is pulled from ghcr.io or Artifactory.

## The contract with nvke-agent

`agent/internal/agent/cni.go` asks apt for `nvke-cni-ovn-charts-<stream>` and
`nvke-cni-ovn-images-<stream>`, where `<stream>` is the request's version or
`defaultOvnCniChartStream`. It runs helm on `/etc/dgx/ovn-kubernetes` when
`values-nke.yaml` is there, else on the directory of the first `Chart.yaml` that
`dpkg-query -L` lists for the charts package. The chart is unpacked at build
time so both paths hold with no maintainer script.

`values-nke.yaml` is written by `bundle.sh`, not shipped by the fork. It sets
`tags.ovs-node: false`, so OVS stays on the host (the node image installs
`doca-openvswitch-switch`; the agent checks for `ovs-vsctl` before helm). It also
points every enabled component at the one image the images package imports into
containerd: `global.imageRepositoryPrefix: localhost/nvke`, so the chart's
`ovnkube` repository becomes `localhost/nvke/ovnkube`, and each component's
`image.tag` is the first 12 characters of the locked commit. It also turns on
pod `hostPort` (`global.enablePodHostPort`), which needs the shared gateway mode
of real nodes; the agent turns it off on container nodes, which run the local
gateway. The chart's defaults are its `values.yaml`, which helm must be given
before `values-nke.yaml`. `bundle.sh` renders the chart with helm when it is installed
and fails if the result names any image other than the embedded one, so a new
image key in the fork cannot ship as a registry pull.

The agent's registry override (`global.registry`, or the server's
`CniImageRegistry`) rewrites `global.imageRepositoryPrefix`, keeping the path
after the host.

Every stream unpacks to the same directory, so the charts package `Provides`
and `Conflicts` with `nvke-cni-ovn-charts`: one stream per node.

## Source

`bundle.sh` fetches the commit the lock names from
`https://github.com/dsx-ai-factory/ovn-kubernetes.git` with one shallow `git fetch`
and exports the tree with `git archive`. git checks every object against its hash,
and the lock's `tree` line pins the exported files, so the lock carries no checksum
of a generated archive: GitHub does not promise stable bytes for one, and its API
tarball endpoint is rate-limited for anonymous callers. The chart is
`charts/ovn-kubernetes/` in that tree (upstream's is `helm/ovn-kubernetes/`) and
the image is built from `dist/images/Dockerfile.ubuntu`, the variant on the
public `ubuntu` base. The `ubuntu-nv-image` variant needs Artifactory and is not
built here.

The lock pins the source, not the image: the `Dockerfile.ubuntu` build runs
`apt-get` and fetches the kubectl key, so no registry digest exists to pin. The
Go and Ubuntu base images are pinned by index digest and the build pins
timestamps, but the fork's apt layer and Go binaries still differ between two
fresh builds. The built archive is therefore cached in `DIST_DIR`, keyed by
commit, arch and build arguments, and a second build reuses it. That is what lets
`make repro-check` show the packaging is deterministic; it does not show the
image rebuilds identically. Delete `DIST_DIR` to force a fresh image build.

The fork's `Dockerfile.ubuntu` leaves out four things its chart needs, so `bundle.sh` builds from
a copy of it that adds them to the runtime layer: the node liveness and readiness scripts the chart
runs as probes from `/root` (without them kubelet kills `ovnkube-controller` every two minutes),
`ncat` and `nslookup` (`dnsutils`), which the readiness script runs (without them it exits
"command not found" and `ovnkube-node` is never Ready, so a template change makes the DaemonSet
controller delete every pod at once), and `firewalld` and a real `iptables`.
`ovnkube-cluster-manager` looks up `firewall-cmd` at startup, and the image's iptables wrappers
`chroot /host`, which the chart never mounts. (`Dockerfile.ubuntu.nv` has all of these.)
It also points the readiness script's DNS check at `kubernetes.default.svc.cluster.local` instead of
google.com and cloudflare.com, which the chart cannot change: without internet DNS the pods would
never be Ready, and a template change would again delete every pod at once.

## Building

```bash
make debs OUT_DIR=/absolute/path
```

Linux only; on macOS `prepare-ovn-assets.sh` runs it in a Linux container that builds the image through the host Docker daemon. `debs` runs `bundle` first, which verifies the source against
`lock/<arch>/<stream>.lock`, builds the image, and reindexes `OUT_DIR` with
`dpkg-scanpackages` when it finishes. `make repro-check` builds twice and
compares every `.deb` byte-for-byte.

The build needs `git` and network access to github.com for the source. The image
build needs docker with buildx, nerdctl or podman (`OCI_BIN=nerdctl` or `podman`), and
network access to `quay.io` (the Go builder), Docker Hub (`ubuntu`), the Ubuntu
archive and pkgs.k8s.io. Override the bases with `OVN_BUILDER_IMAGE` and
`OVN_UBUNTU_BASE_IMAGE`. A build for the other architecture runs under
emulation.

## Bumping the stream

`OVN_STREAMS` is the chart version, which is the fork's release tag without
the `v`: `2.4.2` is tag `v2.4.2`. `make lock` resolves that tag to a commit and
records it with its tree hash. Lock amd64 and arm64, review, commit:

```bash
make lock OVN_STREAMS=2.4.3
make lock OVN_STREAMS=2.4.3 ARCH=arm64
git diff -- lock
```

`OVN_KUBERNETES_REF` locks a branch or a commit instead. A moving branch
can advance between the two runs, so resolve it once and pass the full commit to
both (`bundle.sh` fails a build whose two locks pin different commits). Keep the
default stream in step with `defaultOvnCniChartStream` in `agent/internal/agent/cni.go`.

## Size

`ovnkube` is the only image: every OVN role runs from it, so the
images package is on the order of 350 MiB. `ovs-node`, `ovn-ipsec` and the DPU
subcharts ship in the chart but stay tagged off.
