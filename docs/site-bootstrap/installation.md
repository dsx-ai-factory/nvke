# Download and Install nvkectl

`nvkectl` is the NVKE control-plane CLI. You use it to validate and bootstrap a site controller cluster from a declarative site spec file, and to manage tenant cluster lifecycle.

Releases are published under `cli/v*` tags at https://github.com/dsx-ai-factory/nvke/releases.

## Download

```bash
# Set RELEASE to a cli/ tag listed at https://github.com/dsx-ai-factory/nvke/releases
export RELEASE=cli/vX.Y.Z

# Detects linux/darwin and amd64/arm64 (uname reports x86_64, aarch64, or arm64)
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
curl -fsSL -o nvkectl \
  "https://github.com/dsx-ai-factory/nvke/releases/download/${RELEASE}/nvkectl-${OS}-${ARCH}"
chmod +x nvkectl && sudo mv nvkectl /usr/local/bin/
```

Verify the download with `checksums.txt` from the same release.

## Verify

```bash
nvkectl version
```

## Build from Source

```bash
git clone https://github.com/dsx-ai-factory/nvke.git
cd nvke
make -C cli build   # → cli/bin/nvkectl; not `go build`, which leaves the Cluster API version unset
```

`make build` at the repository root builds every module and copies `nvkectl` to
`bin/` as well; it needs GNU Make 4.0+ (`gmake` on macOS).

## Additional Prerequisites for Bootstrap

`nvkectl bootstrap site` creates an ephemeral Kind cluster on the init node to run the bootstrap workflow engine. The init node needs:

- **`kind`** — `brew install kind` (macOS) or download from https://kind.sigs.k8s.io/docs/user/quick-start/#installation
- **`nerdctl`** and a **containerd** for it to talk to — the container runtime `nvkectl` uses to create the Kind cluster and load images into it. Docker is not supported. Rootful nerdctl, run as root, is what CI exercises; rootless is untested. kind states 1.7.0 as the minimum for its nerdctl provider; 2.3.5 is the version CI exercises.
- **`helm`**, **`kubectl`** and **`clusterctl`** — `nvkectl` shells out to all three: Helm for the Postgres, Temporal, SPIRE and NVKE controller releases, `kubectl` for manifests, and `clusterctl init` for the Cluster API providers.

Only nerdctl is checked before the run starts. A missing `kind` fails in the pre-flight check for a leftover bootstrap cluster, before anything is created; a missing `helm`, `kubectl` or `clusterctl` surfaces mid-bootstrap, after the Kind cluster is already up.

Join nodes do not need `kind` or a container runtime — only the init node runs `nvkectl`.

### Isolate the Bootstrap Runtime's CNI Configuration

By default nerdctl registers its networks as `nerdctl-*.conflist` files in
`/etc/cni/net.d` — the same directory the node's containerd reads for the real
cluster's pods (`conf_dir`). The bootstrap Kind cluster therefore races the
real cluster's CNI plugin: between `kubeadm init` and the CNI plugin writing
its own configuration, the bootstrap bridge conflist is the only CNI
configuration on the init node. containerd reports the node network as ready,
and the first pod sandboxed in that window binds to the bootstrap bridge and
keeps that address for the life of its sandbox. Observed on a real site
bring-up: a node-local-dns pod pinned to `10.4.0.2`, four seconds before
ovn-kubernetes wrote `10-ovn-kubernetes.conf`.

Point nerdctl at its own directory before the first bootstrap:

```toml
# /etc/nerdctl/nerdctl.toml
cni_netconfpath = "/etc/containerd-nerdctl/net.d"
```

nerdctl creates the directory itself, and kind's nerdctl provider shells out
to nerdctl, so the setting needs no extra flags.

If an earlier bootstrap already wrote conflists into `/etc/cni/net.d`, remove
the `nerdctl-*.conflist` files **and** delete any bridge interface still
holding an address from their subnets before retrying. A live address without
a matching conflist in the directory nerdctl reads fails the next network
creation with `subnet ... overlaps with other one on this address space`.

## Next Steps

- [Configuration](configuration.md) — write your site site spec file
- [Pre-Provisioned Nodes](pre-provisioned-nodes.md) — prepare nodes and run the bootstrap
