# Download and Install nkectl

`nkectl` is the NKE control-plane CLI. You use it to validate and bootstrap a site controller cluster from a declarative site spec file, and to manage tenant cluster lifecycle.

Releases are published under `cli/v*` tags at https://github.com/NVIDIA/nke/releases.

## Download

```bash
# linux/amd64
curl -fsSL -o nkectl \
  https://github.com/NVIDIA/nke/releases/latest/download/nkectl-linux-amd64
chmod +x nkectl
sudo mv nkectl /usr/local/bin/
```

```bash
# linux/arm64
curl -fsSL -o nkectl \
  https://github.com/NVIDIA/nke/releases/latest/download/nkectl-linux-arm64
chmod +x nkectl
sudo mv nkectl /usr/local/bin/
```

```bash
# macOS (Apple Silicon)
curl -fsSL -o nkectl \
  https://github.com/NVIDIA/nke/releases/latest/download/nkectl-darwin-arm64
chmod +x nkectl
sudo mv nkectl /usr/local/bin/
```

Verify the download with `checksums.txt` from the same release.

## Verify

```bash
nkectl version
```

## Build from Source

```bash
git clone https://github.com/NVIDIA/nke.git
cd nke
make build          # links all binaries including nkectl into bin/
```

The binary lands at `bin/nkectl`.

## Additional Prerequisites for Bootstrap

`nkectl bootstrap site` creates an ephemeral Kind cluster on the init node to run the bootstrap workflow engine. The init node needs:

- **`kind`** — `brew install kind` (macOS) or download from https://kind.sigs.k8s.io/docs/user/quick-start/#installation
- **`nerdctl`** (default) or **`docker`** — the container runtime `nkectl` uses to create and load images into the Kind cluster. Use `--bootstrap-provider docker` if you are running Docker rather than nerdctl/containerd.

Join nodes do not need `kind` or a container runtime — only the init node runs `nkectl`.

## Next Steps

- [Configuration](configuration.md) — write your site site spec file
- [Pre-Provisioned Nodes](pre-provisioned-nodes.md) — prepare nodes and run the bootstrap
