# nkectl

NKE control-plane CLI. Bootstraps site controllers and manages tenant cluster lifecycle.

**Module:** `github.com/NVIDIA/nke/cli`  
**Binary:** `nkectl`  
**Releases:** `cli/v*` tags

## What it does

- **Site bootstrap** — `nkectl bootstrap site` creates an ephemeral Kind cluster, deploys CWE onto it, then drives kubeadm init/join across pre-provisioned nodes via `nke-agent` gRPC; no SSH required
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
NKECTL_CWE_IMAGE=nvcr.io/<org>/controller:edge \
NKECTL_CWE_PROFILES_IMAGE=nvcr.io/<org>/profiles:edge \
  nkectl bootstrap site -i inventory.yaml
nkectl bootstrap site -i inventory.yaml --dry-run              # validate only
nkectl bootstrap site -i inventory.yaml --bootstrap-provider docker  # use Docker instead of nerdctl
nkectl bootstrap site -i inventory.yaml --keep-bootstrap-cluster     # leave Kind cluster for debugging

# Point the CLI at a site controller
nkectl config set-site --url https://cwe.example.com --name default

# Tenant cluster lifecycle
nkectl get clusters                    # table with AGE and status
nkectl get clusters -o json            # JSON output
nkectl describe cluster <name>
nkectl create cluster [spec-file]      # or: cat spec.yaml | nkectl create cluster -
nkectl delete cluster <name>           # prompts [y/N]; use --yes to skip
```

## When a bootstrap fails

`bootstrap site` removes the ephemeral Kind cluster on every exit path, so by
the time the error reaches you there is nothing left to ask. To keep the
evidence, a failed run collects a `kubectl cluster-info dump` — every
namespace's objects and pod logs — plus the events, object state and helm
releases that explain a pod which never started, and writes it out *before* the
teardown.

It lands in `cluster-diagnostics/`, overridable with `--diagnostics-dir` or
`NKECTL_BOOTSTRAP_DIAGNOSTICS_DIR`. A successful run writes nothing. CI uploads
the directory as an artifact when the bootstrap E2E fails.

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

- [Site Controller Bootstrap](../docs/site-bootstrap/overview.md)
- [Architecture](../docs/architecture.md)
