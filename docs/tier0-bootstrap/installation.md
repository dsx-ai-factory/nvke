# Download and Install nkectl

`nkectl` is the NKE control-plane CLI. You use it to validate and bootstrap a site controller cluster from a declarative inventory file, and to manage tenant cluster lifecycle.

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

## Next Steps

- [Configuration](configuration.md) — write your site inventory file
- [Pre-Provisioned Nodes](pre-provisioned-nodes.md) — prepare nodes and run the bootstrap
