> **Note:** This component is being migrated from internal repositories. Build commands, installation paths, and CLI flags will be finalized during migration. The information here reflects the intended state.

# nke-cli

Command-line tool for NKE operators. Provides cluster access, fleet operations, site deployment, and an operator console web dashboard.

## What it does

- **Cluster connect** — wraps the Teleport login flow into a single command that spawns an isolated shell with `kubectl` configured and built-in helpers for common tasks (`admin`/`drop-admin`, port-forwards to CWE/ArgoCD/Temporal UIs, direct node SSH)
- **Site deploy** — provisions site controller infrastructure via the NICo/Forge API
- **Teleport token management** — generates and rotates Teleport join tokens for enrolling nodes
- **Hardware inspection** — queries infrastructure provider APIs for hardware inventory
- **Operator console** — embedded web dashboard for fleet-wide cluster visibility

## Language and tech

Go, multi-platform (Linux x86_64, aarch64, macOS). Embeds the operator console UI as static assets.

## Installation

> **TODO:** Replace with real release URL once CI publishes nke-cli binaries. Confirm binary naming convention (platform suffix format).

```bash
# Download the latest release binary for your platform
curl -fsSL https://github.com/NVIDIA/nke/releases/latest/download/nke-cli-linux-amd64 -o nke-cli
chmod +x nke-cli
sudo mv nke-cli /usr/local/bin/
```

## Usage

```bash
# Connect to a site controller cluster (opens an isolated shell with kubectl ready)
nke-cli cluster connect <cluster-name>

# Within the connected shell:
admin              # request cluster-admin privileges
cwe-pf             # port-forward to Cloud Workflow Engine UI
argo-pf            # port-forward to ArgoCD UI
temporal-pf        # port-forward to Temporal UI
node-ssh <node>    # SSH to a node via Teleport
```

## Platform support

| OS | x86_64 | aarch64 |
|---|---|---|
| Linux | ✓ | ✓ |
| macOS | ✓ | ✓ |

## Related docs

- [Architecture](../docs/architecture.md)
- [Tier-0 Bootstrap](../docs/tier0-bootstrap/overview.md)
