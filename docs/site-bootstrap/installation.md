# Download and Install nvkectl

`nvkectl` is the NVKE control-plane CLI. You use it to validate and bootstrap a site controller cluster from a declarative site spec file, and to manage tenant cluster lifecycle.

NVKR builds `nvkectl` from the same source commit as the controller image and publishes it in the release, so the CLI matches the stack it installs. Take it from the release you verified. Without a release, build it from the clone.

## From the NVKR Release

Verify the release and export its description, as [Install the Site Controller](install-site-controller.md#release) describes. The description names an executable output per architecture. With `jq` and [`oras`](https://oras.land/docs/installation) on the machine, pull it and unpack the binary:

```bash
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
REF=$(jq -r --arg arch "$ARCH" \
  '.outputs[] | select(.kind == "executable" and (.name | startswith("nvkectl")) and .arch == $arch) | .reference' \
  release.json)
oras pull "$REF"              # writes bundle.tar.gz
tar -xzf bundle.tar.gz        # writes files/nvkectl
sudo install -m 0755 files/nvkectl /usr/local/bin/nvkectl
```

The output is an OCI artifact with one layer, a gzip tar of the binaries, so any OCI client that pulls by digest works in place of `oras`. For a release held as a local OCI layout, `nvkr publish extract --layout <dir> --bundle nvkectl --out <dir>` verifies the publication and writes the same binary.

## Verify

```bash
nvkectl version
```

## Build from Source

Without a release, build the CLI from the clone; the controller and profiles images are built from the same clone.

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

## Next Steps

- [Configuration](configuration.md) — write your site site spec file
- [Pre-Provisioned Nodes](pre-provisioned-nodes.md) — prepare nodes and run the bootstrap
