# Reference node image

Ubuntu 26.04 image that ships `nke-agent` and `spire-agent` so a site
controller can reach the node over SPIFFE mTLS. The contract is [SPEC.md](SPEC.md).

| Path | What |
|---|---|
| `SPEC.md` | Image contract (bake vs first-boot, ports, identity) |
| `packer/` | Packer + QEMU recipe that produces a qcow2 |
| `files/` | systemd units, SPIRE template, first-boot script |
| `cmd/nke-image-check` | Connectivity checker (Go) |

## Build the image

On a Linux host with KVM, [Packer](https://developer.hashicorp.com/packer),
[ORAS](https://oras.land/), and `curl`:

```bash
cd packer
make image NKE_AGENT_VERSION=edge
# or a release tag, without the agent/ prefix: NKE_AGENT_VERSION=v0.3.0
```

`edge` (the default) is tip of `main` at `ghcr.io/nvidia/nke/agent`. The recipe
pulls that OCI **binary** on the Packer host and copies it in; it does not
install from apt. SPIRE agent is pinned (version + sha256) from GitHub
Releases.

Output: `packer/output/nke-node.qcow2`.

The build-time cloud-init seed is only for Packer's SSH. The provisioner runs
`cloud-init clean` and drops the build key, so the qcow2 is a golden image.

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
whatever will dial the node (controller or `nke-image-check`).

## Checker

From the repository root (`make build` writes `bin/nke-image-check`):

```bash
nke-image-check --endpoint 192.0.2.10:15005
```

The process needs a SPIFFE workload API with an SVID whose path starts with
`/nke-service/`, in the same trust domain as the node. Default socket:

`unix:///tmp/spire-agent/public/api.sock`

Override with `--spiffe-socket`. This is the same shape as the controller
dialer; it is not a SPIRE server. For a local SPIRE, see
`controller/scripts/dev-env-spire.sh`.
