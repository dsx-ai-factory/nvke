# Reference node OS image

Ubuntu 26.04 image that ships `nke-agent` and `spire-agent` so a site
controller can reach the node over SPIFFE mTLS. The contract is [SPEC.md](SPEC.md).

| Path | What |
|---|---|
| `SPEC.md` | Image contract (bake vs first-boot, ports, identity) |
| `packer/` | Packer + QEMU recipe that produces a qcow2 |
| `files/` | systemd units, SPIRE template, first-boot script |
| `cmd/nke-os-image-check` | Connectivity checker (Go) |

## Build the OS image

Prereqs, on a Linux host with KVM:

| Tool | Version | Install |
|---|---|---|
| [Packer](https://developer.hashicorp.com/packer) | >= 1.10.0 | https://developer.hashicorp.com/packer/install |
| [ORAS](https://oras.land/) | any | https://oras.land/docs/installation |
| `curl` | any | via your package manager |

`make os-image` checks for `oras` on `PATH` before it boots the build VM and
fails fast with the install link above if it's missing.

If `ghcr.io/nvidia/nke/agent-deb` (or the binary fallback `ghcr.io/nvidia/nke/agent`) is private, log in first with a GitHub PAT
(`read:packages` scope):

```bash
echo "<github-pat>" | oras login ghcr.io -u <your-github-username> --password-stdin
```

Credentials are cached (`~/.docker/config.json` by default), so `oras pull`
in `fetch-artifacts.sh` picks them up automatically.

```bash
cd packer
make os-image NKE_AGENT_VERSION=edge
# or a release tag, without the agent/ prefix: NKE_AGENT_VERSION=v0.3.0
```

`edge` (the default) is tip of `main`. The recipe **prefers** the nke-agent
`.deb` from `ghcr.io/nvidia/nke/agent-deb` (published on `agent/v*` tags) and
falls back to the OCI **binary** at `ghcr.io/nvidia/nke/agent` when no package
exists for that tag. Pass a release tag without the `agent/` prefix to pull a
`.deb`: `NKE_AGENT_VERSION=v0.3.0`. SPIRE agent is pinned (version + sha256)
from GitHub Releases.

Output: `packer/output/nke-node` (qcow2, Packer's `vm_name`).

The build-time cloud-init seed is only for Packer's SSH. The provisioner runs
`cloud-init clean` and drops the build key, so the qcow2 is a golden image.

## Release

An `os-image/vX.Y.Z` tag (or `os-image/vX.Y.Z-rc.N`) runs
`.github/workflows/os-image-artifact.yml` on a KVM runner. The job builds the
qcow2, boots it (`make boot-smoke`), and **only then** publishes:

```bash
oras pull ghcr.io/nvidia/nke/os-image:v0.1.0 --platform linux/amd64
```

The GitHub Release for that tag attaches `nke-os-image-check` and `checksums.txt`.
The disk stays on GHCR; it is not a GitHub Release asset.

Default bake uses `NKE_AGENT_VERSION=edge`. Override on a `workflow_dispatch`
run, or by passing `NKE_AGENT_VERSION=v0.3.0` to `make os-image` locally.

## First boot (cloud-init)

Do **not** bake SPIRE server, trust domain, bundle, or join token. Pass them
as instance user-data, for example:

```yaml
#cloud-config
write_files:
  - path: /etc/nke/spire.env
    permissions: "0600"
    content: |
      SPIRE_SERVER_ADDRESS=spire.example.com
      SPIRE_SERVER_PORT=8081
      SPIRE_TRUST_DOMAIN=example.org
      SPIRE_JOIN_TOKEN=REDACTED
  - path: /etc/nke/trust-bundle.pem
    permissions: "0644"
    content: |
      -----BEGIN CERTIFICATE-----
      ...
      -----END CERTIFICATE-----
```

`nke-spire-configure.service` waits for `cloud-final`, renders
`/etc/spire-agent/agent.conf`, and only then does `spire-agent` start.
`nke-agent` starts after the workload API socket exists.

The site SPIRE server must also register a workload entry for root
`nke-agent` (unix attestor, uid 0) and a `/nke-service/...` identity for
whatever will dial the node (controller or `nke-os-image-check`).

## Checker

From the repository root (`make build` writes `bin/nke-os-image-check`):

```bash
nke-os-image-check --endpoint 192.0.2.10:15005
```

The process needs a SPIFFE workload API with an SVID whose path starts with
`/nke-service/`, in the same trust domain as the node. Default socket:

`unix:///tmp/spire-agent/public/api.sock`

Override with `--spiffe-socket`. This is the same shape as the controller
dialer; it is not a SPIRE server. For a local SPIRE, see
`controller/scripts/dev-env-spire.sh`.
