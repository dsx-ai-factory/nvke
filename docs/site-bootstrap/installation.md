# Download and Install nkectl

`nkectl` is the NKE control-plane CLI. You use it to validate and bootstrap a site controller cluster from a declarative site spec file, and to manage tenant cluster lifecycle.

Releases are published under `cli/v*` tags at https://github.com/dsx-ai-factory/nke/releases.

## Download

```bash
# linux/amd64
curl -fsSL -o nkectl \
  https://github.com/dsx-ai-factory/nke/releases/latest/download/nkectl-linux-amd64
chmod +x nkectl
sudo mv nkectl /usr/local/bin/
```

```bash
# linux/arm64
curl -fsSL -o nkectl \
  https://github.com/dsx-ai-factory/nke/releases/latest/download/nkectl-linux-arm64
chmod +x nkectl
sudo mv nkectl /usr/local/bin/
```

```bash
# macOS (Apple Silicon)
curl -fsSL -o nkectl \
  https://github.com/dsx-ai-factory/nke/releases/latest/download/nkectl-darwin-arm64
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
git clone https://github.com/dsx-ai-factory/nke.git
cd nke
make build          # links all binaries including nkectl into bin/
```

The binary lands at `bin/nkectl`.

## Additional Prerequisites for Bootstrap

`nkectl bootstrap site` creates an ephemeral Kind cluster on the init node to run the bootstrap workflow engine. The init node needs:

- **`kind`** — `brew install kind` (macOS) or download from https://kind.sigs.k8s.io/docs/user/quick-start/#installation
- **`nerdctl`** and a **containerd** for it to talk to — the container runtime `nkectl` uses to create the Kind cluster and load images into it. Docker is not supported. kind states 1.7.0 as the minimum for its nerdctl provider; 2.3.5 is the version CI exercises.

Join nodes do not need `kind` or a container runtime — only the init node runs `nkectl`.

## Next Steps

- [Configuration](configuration.md) — write your site site spec file
- [Pre-Provisioned Nodes](pre-provisioned-nodes.md) — prepare nodes and run the bootstrap
