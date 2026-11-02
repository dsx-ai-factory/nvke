# agent

gRPC agent that runs as a systemd service on each NVKE node. An orchestrator
calls it to drive kubeadm lifecycle, package/CNI install, API encryption, and
related node-local operations — without interactive SSH.

| Doc | Purpose |
|-----|---------|
| [AGENTS.md](AGENTS.md) | Build, test, proto, versioning, and contributor conventions |
| [docs/DESIGN.md](docs/DESIGN.md) | Architecture and API surface |
| [docs/SECURITY.md](docs/SECURITY.md) | Security context (reporting: repo-root `SECURITY.md`) |
| [pkg/agent/proto/node_agent.proto](pkg/agent/proto/node_agent.proto) | Canonical RPC definitions |
| [config.env.example](config.env.example) | Build/runtime artifact endpoint knobs |

**Module:** `github.com/dsx-ai-factory/nvke/agent`

## Quick start

```bash
# From the monorepo root
./build/agent.sh test
./build/agent.sh build

# Or from this directory
make test
make build
```

Install a release tarball on a node (as root):

1. Extract the package on the host
2. Run `scripts/agent-installer.sh` (or the bundled installer from the release)
3. Configure flags via `/etc/default/nvke-agent` (see below)

## Configuration

Copy `config.env.example` → `config.env` (gitignored) for local build/publish
overrides. On nodes, set flags via `/etc/default/nvke-agent`.

Important flags (also settable via env; see `AGENT_ARGS`):

| Flag | Notes |
|------|--------|
| `--bundle-server-url` | Optional. Bundle source for InstallBundle (`https://`, `http://`, `file://`, or `oci://`). Also `NKE_BUNDLE_SERVER_URL`. Prefer apt when unset. |
| `--index-file-url` | Optional absolute index URL (`NKE_BUNDLE_INDEX_URL`); otherwise derived from the bundle server + OS |
| `--cni-image-registry` | Optional OCI prefix for CNI helm `global.registry` (`NKE_CNI_IMAGE_REGISTRY`) |
| `--disable-mtls` | Plaintext gRPC (**development only**) |
| `--spiffe-socket-path` | SPIFFE workload API socket. Used for the gRPC server's mTLS unless `--disable-mtls` is set, and always by the SPIRE agent RPCs |
| `--secure-server-port` | gRPC listen port (default `15005`) |
| `--version` | Print build version and exit |

Example:

```bash
# Apt-first (no bundle server required)
AGENT_ARGS="--disable-mtls"

# Optional: enable legacy HTTP/OCI bundle installs
AGENT_ARGS="--bundle-server-url https://artifacts.example.com/ --disable-mtls"
# OCI bundles:
# AGENT_ARGS="--bundle-server-url oci://registry.example.com/nvke/bundles --disable-mtls"
```

There is no built-in artifact host. When using bundles, consumers must point the
agent at their own S3/HTTP or OCI layout. Index entries use paths relative to
the bundle server URL (for example
`nke_bundles/Ubuntu_24.04/kubeadm_v1.30.0.tgz`).

gRPC reflection is enabled on the same port (useful with `grpcurl`).

### Logs

| Path | Purpose |
|------|---------|
| `/var/log/nvke-agent-audit.log` | Audit log (`--audit-log-file`); records are also mirrored to the service journal |
| `/var/log/nvke-execute-scripts.log` | `ExecuteScript` / `StreamExecuteScript` results |
| `journalctl -u nvke-agent` | Service journal |

`GetSupportBundle` returns a tarball with state, journals, and related diagnostics.

## Core capabilities

- **Kubeadm:** init, join, reset, preflight, join strings, kubeconfig
- **CNI:** `InstallCNI` after packages are present; prefer `noCniInstall: true` on init for new orchestration (legacy in-init CNI remains when `noCniInstall` is false)
- **Packages / collections:** apt packages, apt repos, bundles, collections
- **Encryption:** Kubernetes API/etcd encryption-at-rest
- **Control-plane ops:** graceful shutdown / kube-vip restore, apiserver manifest patch
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

Version comes from `git describe --match 'agent/v*'` (component-scoped tags;
see [AGENTS.md](AGENTS.md)). Override with `VERSION=...` when needed.

## Published artifacts

### Binaries (GHCR)

Every commit on `main` and every `agent/vX.Y.Z` tag publishes the binaries to
`ghcr.io/dsx-ai-factory/nvke/agent` as OCI artifacts, joined by a multi-arch index
(`.github/workflows/agent-artifact.yml`). Pull one by platform:

```bash
oras pull ghcr.io/dsx-ai-factory/nvke/agent:v0.3.0 --platform linux/arm64   # release
oras pull ghcr.io/dsx-ai-factory/nvke/agent:edge   --platform linux/amd64   # tip of main
```

These are binaries, **not** container images — `docker pull` will fetch one but
there is nothing to run. Use `oras`.

⚠️ **The path changed twice.** Releases `agent/v0.2.1-rc.1` to `agent/v0.2.1-rc.4`
were published before the repository moved to `dsx-ai-factory` on 2026-09-12 and
are at `ghcr.io/nvidia/nke/agent`. Releases up to and including `agent/v0.2.1-rc.0`
are at the older `ghcr.io/nvidia/nvke-agent`. Both are frozen and still serve them.

Tags are `edge`, `sha-<commit>`, and `vX.Y.Z`, each with `-amd64` / `-arm64`
single-platform variants underneath. These are artifacts, not container images
— the layer is the executable itself, so `docker pull` is not the way in.

### Debian packages (apt intake)

Every `agent/vX.Y.Z` tag also builds native amd64 and arm64 `.deb`s and uploads
them to the apt-publisher S3 intake (`.github/workflows/agent-deb-publish.yml`,
via `scripts/upload-s3-apt-intake.sh`). Main merges and branch workflow runs do
not ingest — only `refs/tags/agent/v*`. The same packages are attached as assets
on the GitHub Release for that tag and published to GHCR as an OCI artifact:

```bash
oras pull ghcr.io/dsx-ai-factory/nvke/agent-deb:v0.3.0 --platform linux/amd64
```

Apt remains the node install path; the OCI binary above is the same executable
without the packaging/systemd unit. The reference OS image (`os-image/`) installs the
`.deb` built from this checkout. The package spec and maintainer scripts live in
`os-image/packages/nvke-agent/packaging/`; `make -C agent deb-amd64` or
`deb-arm64` builds it with the shared pinned nfpm.

The intake destination comes from the GitHub `apt-intake` environment
(`APT_INTAKE_S3_URI`, `APT_INTAKE_AWS_ROLE_ARN`, …) — nothing bucket-specific
is committed here. Once apt-publisher is pointed at that intake, cut over from
the previous GitLab package source.

