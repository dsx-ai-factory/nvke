# node-agent

gRPC agent that runs as a systemd service on each NKE node. An orchestrator
calls it to drive kubeadm lifecycle, package/CNI install, disk and API
encryption, and related node-local operations — without interactive SSH.

| Doc | Purpose |
|-----|---------|
| [AGENTS.md](AGENTS.md) | Build, test, proto, and contributor conventions |
| [docs/DESIGN.md](docs/DESIGN.md) | Architecture and API surface |
| [docs/SECURITY.md](docs/SECURITY.md) | Security context (reporting: repo-root `SECURITY.md`) |
| [pkg/agent/proto/node_agent.proto](pkg/agent/proto/node_agent.proto) | Canonical RPC definitions |

**Module:** `github.com/NVIDIA/nke/node-agent`

## Quick start

```bash
# From the monorepo root
./build/node-agent.sh test
./build/node-agent.sh build

# Or from this directory
make test
make build
```

Install a release tarball on a node (as root):

1. Extract the package on the host
2. Run `scripts/agent-installer.sh` (or the bundled installer from the release)
3. Configure flags via `/etc/default/nke-agent` (see below)

## Configuration

Important flags (also settable via env; see `AGENT_ARGS` in `/etc/default/nke-agent`):

| Flag | Notes |
|------|--------|
| `--bundle-server-url` | Bundle source (`https://`, `http://`, `file://`, or `oci://`). Default in this tree still points at a legacy host; prefer an explicit value for your environment. |
| `--index-file-url` | Optional absolute index URL; otherwise derived from the bundle server + OS |
| `--cni-image-registry` | Optional OCI prefix for CNI helm `global.registry` |
| `--disable-mtls` | Plaintext gRPC (**development only**) |
| `--spiffe-socket-path` | SPIFFE workload API socket (ignored with `--disable-mtls`) |
| `--secure-server-port` | gRPC listen port (default `15005`) |
| `--version` | Print build version and exit |

Example:

```bash
AGENT_ARGS="--bundle-server-url https://artifacts.example.com/ --disable-mtls"
```

gRPC reflection is enabled on the same port (useful with `grpcurl`).

### Logs

| Path | Purpose |
|------|---------|
| `/var/log/nke-agent-audit.log` | Audit log (`--audit-log-file`) |
| `/var/log/nke-execute-scripts.log` | `ExecuteScript` / `StreamExecuteScript` results |
| `journalctl -u nke-agent` | Service journal |

`GetSupportBundle` returns a tarball with state, journals, and related diagnostics.

## Core capabilities

- **Kubeadm:** init, join, reset, preflight, join strings, kubeconfig
- **CNI:** `InstallCNI` after packages are present; prefer `noCniInstall: true` on init for new orchestration (legacy in-init CNI remains when `noCniInstall` is false)
- **Packages / collections:** apt packages, apt repos, bundles, collections
- **Encryption:** Kubernetes API/etcd encryption-at-rest
- **Control-plane ops:** graceful shutdown / kube-vip restore, apiserver manifest patch, NKE RBAC apply
- **Utilities:** script execution, file upload, support bundle

Full RPC list: `pkg/agent/proto/node_agent.proto` and [docs/DESIGN.md](docs/DESIGN.md).

## Example: create a small cluster (grpcurl)

Examples below use **obviously fake** tokens, hashes, and keys so scanners do not
treat them as live credentials. Replace with values from your environment.

```bash
# Init first control-plane (TEST-NET address 192.0.2.10 is documentation-only)
grpcurl -plaintext -d '{
  "podNetworkCidr": "10.244.0.0/16",
  "serviceCidr": "10.96.0.0/12",
  "noCniInstall": true
}' 192.0.2.10:15005 proto.NodeAgent.ExecuteKubeadmInit

# Poll status / join strings
grpcurl -plaintext 192.0.2.10:15005 proto.NodeAgent.GetKubeadmStatus
grpcurl -plaintext 192.0.2.10:15005 proto.NodeAgent.GetKubeadmJoinString

# Join another node (placeholders — not real kubeadm material)
grpcurl -plaintext -d '{
  "kubeadmJoinString": "join 192.0.2.10:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:0000000000000000000000000000000000000000000000000000000000000000 --control-plane --certificate-key 0000000000000000000000000000000000000000000000000000000000000000"
}' 192.0.2.11:15005 proto.NodeAgent.ExecuteKubeadmJoin
```

Install CNI after packages are on the control-plane host (`InstallPackages`), then:

```bash
grpcurl -plaintext -d '{
  "cniVariant": "CNI_VARIANT_OVN",
  "podNetworkCidr": "10.244.0.0/16",
  "serviceCidr": "10.96.0.0/12"
}' 192.0.2.10:15005 proto.NodeAgent.InstallCNI
```

More sample clients and manifests: [`samples/`](samples/) and [`sample-clients/`](sample-clients/).

## Encryption (sketch)

API encryption is a multi-step flow; see DESIGN.md and the proto comments.
When posting example keys in docs or tickets, use clearly invalid material, e.g.:

```text
encryptionKey: "EXAMPLEONLY_not-a-real-secretbox-key_000000000000="
```

Never paste production tokens, certificate keys, or encryption keys into the README.

## mTLS

Production should use SPIFFE mTLS (default). `--disable-mtls` is for local/dev only.
Clients need a matching SPIFFE identity (path prefix `/nke-service/`).

## Build notes

```bash
make build          # linux/amd64 + linux/arm64
make package        # release tarballs under release/
make protos         # regenerate *.pb.go via buf
make setup-hooks    # monorepo-aware proto pre-commit hook
```

Version string comes from `git describe` (see Makefile).
