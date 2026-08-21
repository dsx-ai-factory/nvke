# nkectl

NKE control-plane CLI. Bootstraps site controllers and manages tenant cluster lifecycle.

**Module:** `github.com/NVIDIA/nke/cli`  
**Binary:** `nkectl`  
**Releases:** `cli/v*` tags

## What it does

- **Site bootstrap** — `nkectl bootstrap site` drives kubeadm init/join across pre-provisioned nodes via `nke-agent` gRPC; no SSH required
- **Cluster lifecycle** — `nkectl get/create/delete/describe clusters` manages tenant clusters through the CWE API
- **Config** — `nkectl config set-site` points the CLI at a site controller; `nkectl config validate` validates a bootstrap inventory file

## Installation

```bash
# linux/amd64
curl -fsSL -o nkectl \
  https://github.com/NVIDIA/nke/releases/latest/download/nkectl-linux-amd64
chmod +x nkectl && sudo mv nkectl /usr/local/bin/

# linux/arm64
curl -fsSL -o nkectl \
  https://github.com/NVIDIA/nke/releases/latest/download/nkectl-linux-arm64
chmod +x nkectl && sudo mv nkectl /usr/local/bin/

# macOS (amd64 / arm64)
curl -fsSL -o nkectl \
  https://github.com/NVIDIA/nke/releases/latest/download/nkectl-darwin-arm64
chmod +x nkectl && sudo mv nkectl /usr/local/bin/
```

```bash
nkectl version
```

## Quick reference

```bash
# Bootstrap a new site controller
nkectl bootstrap site -i inventory.yaml
nkectl bootstrap site -i inventory.yaml --dry-run   # validate only

# Point the CLI at a site controller
nkectl config set-site https://cwe.example.com

# Tenant cluster lifecycle
nkectl get clusters
nkectl describe cluster <name>
nkectl create cluster [spec-file]
nkectl delete cluster <name>
```

## Build from source

```bash
# From monorepo root
make build          # builds all binaries including cli/nkectl into bin/

# Or from this directory
make build
```

## Platform support

| OS | amd64 | arm64 |
|---|---|---|
| Linux | ✓ | ✓ |
| macOS | ✓ | ✓ |

## Related docs

- [Site Controller Bootstrap](../docs/tier0-bootstrap/overview.md)
- [Architecture](../docs/architecture.md)
