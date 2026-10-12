# nvke-cni-calico

Builds the two Debian packages `nvke-agent` installs when it sets up Calico:

| Package | Contents | Installed to |
|---|---|---|
| `nvke-cni-calico-manifests-<stream>` | Upstream `operator-crds.yaml`, `tigera-operator.yaml`, and `custom-resources.yaml` | `/usr/share/nvke-cni/calico/<stream>/manifests/` |
| `nvke-cni-calico-images-<stream>` | Every image the operator resolves, as one docker-archive | `/usr/lib/nvke/cni-calico/<stream>/`, imported into containerd by `postinst` |

`provision.sh` stages both into the image under `/usr/lib/nvke/debs` as a local
apt repo. Nothing is installed at bake time: `nvke-agent` installs them by name
at cluster create. The build machinery is shared with `cni-ovn` in
[`../cni-common/`](../cni-common/README.md); this directory holds only what is
Calico's: `bundle.sh`, the manifests spec and the lock.

## Why both

The manifests name only `quay.io/tigera/operator`. The operator then resolves
the rest of Calico itself, so a node with no registry access needs the whole set
present in containerd before the operator starts. The images package is what
makes that true; without it a cluster comes up and Calico never does.

## The contract with nvke-agent

`agent/internal/agent/cni.go` asks apt for `nvke-cni-calico-manifests-<stream>`
and `nvke-cni-calico-images-<stream>`, where `<stream>` is the request's Calico
version or `defaultCalicoCniStream`. It then finds the install root by looking
through `dpkg-query -L` for a path ending in `manifests/tigera-operator.yaml`.
It applies `operator-crds.yaml` and waits for the CRDs to be established. It
then applies the operator and `custom-resources.yaml`.

All three manifests must be owned by the package and live in the same
`manifests/` directory. `TestCalicoInstallRootFromFiles` in the agent has a case
pinned to this package's exact layout.

## Building

```bash
make debs OUT_DIR=/absolute/path
```

Linux only. `debs` runs `bundle` first, which verifies every artifact against
`lock/<arch>/<version>.lock` before anything is unpacked, and reindexes `OUT_DIR` with
`dpkg-scanpackages` when it finishes. `make repro-check` builds twice and
compares every `.deb` byte-for-byte.

## Bumping Calico

Set `CALICO_VERSIONS`, refresh the lock, review it, commit it:

```bash
make lock CALICO_VERSIONS=v3.30.2
git diff -- lock
```

`make lock` needs network. It re-resolves every image digest and re-checksums
the manifests; the image *list* comes from the lock's existing `image` lines, so
a release that adds a component needs that line added by hand first. Deriving
the list from the manifests would miss exactly those images, because the
operator resolves them at runtime rather than naming them.

Keep the default stream in step with `defaultCalicoCniStream` in
`agent/internal/agent/cni.go`; the agent asks for that one when a request names
no version.

## Size

The images package is around 490 MiB, roughly half a gigabyte added to the
node image. That is the cost of installing Calico with no registry. If a site
has a reachable registry, the manifests package alone is enough.
