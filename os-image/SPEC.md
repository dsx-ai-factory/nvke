# NKE reference node image — contract

This is the contract an OS image must satisfy for NKE to manage a node.
It is **not** a cluster-ready Kubernetes image: kubeadm, a CNI, and GPU
drivers are installed later through `nke-agent`. The bar for v1 is that the
controller can reach `nke-agent` over SPIFFE mTLS.

The in-tree Packer recipe (`packer/`) is one implementation of this contract,
not the only allowed one.

## Must have (baked into the image)

TODO:  Need to refine the requirements around spire-agent below

| Item | Requirement |
|---|---|
| OS | Ubuntu 26.04 LTS, `linux/amd64` for the first reference image. |
| `nke-agent` | Installed at `/usr/local/bin/nke-agent` from the nke-agent `.deb` when GHCR has one for the requested tag, otherwise the OCI binary. systemd unit `nke-agent.service` **enabled**. Listens on TCP **15005**. mTLS is on (no `--disable-mtls` in `/etc/default/nke-agent`). A drop-in waits for the SPIRE workload API before exec. |
| SPIRE agent | `spire-agent` at `/usr/local/bin/spire-agent`, unit `spire-agent.service` **enabled**. Workload API socket at `unix:///tmp/spire-agent/public/api.sock` (the `nke-agent` default). |
| Unix workload attestor | SPIRE `WorkloadAttestor "unix"` so root `nke-agent` can fetch an X.509 SVID. |
| Node attestor | Join-token (`NodeAttestor "join_token"`). TPM attestation is out of scope for this reference image. |
| Unit ordering | `nke-spire-configure.service` before `spire-agent.service` before `nke-agent.service`. `nke-agent` waits for the workload API socket before exec. |
| First-boot renderer | `/usr/lib/nke/spire-configure.sh`, driven by `nke-spire-configure.service`. Renders `/etc/spire-agent/agent.conf` from instance identity. Does nothing unless `/etc/nke/spire.env` exists. |

## Must not bake (instance identity)

These are **site- or node-specific**. They arrive via cloud-init (or equivalent
user-data) on first boot, never at image-build time:

- SPIRE server address and port
- Trust domain
- Trust bundle (PEM)
- Join token (single-use, minted per node)

The Packer build **enables** the units but does not start them against a live
SPIRE server, and it runs `cloud-init clean` so the next boot is a real
instance.

### Cloud-init input

Write these two files. Permissions: `spire.env` and the join token inside it
are secrets (`0600`).

`/etc/nke/spire.env`:

```sh
SPIRE_SERVER_ADDRESS=spire.example.com
SPIRE_SERVER_PORT=8081
SPIRE_TRUST_DOMAIN=example.org
SPIRE_JOIN_TOKEN=REDACTED
```

`/etc/nke/trust-bundle.pem`: the SPIRE server's trust bundle, PEM-encoded.

A worked `#cloud-config` is in [README.md](README.md).

After a successful `nke-agent` start, the image touches
`/var/lib/spire-agent/.nke-attested` and subsequent boots omit `-joinToken`
(the token is single-use).

## Identity rules (runtime)

These are enforced by `nke-agent`, not by the image:

- Client and agent SVIDs share a trust domain.
- The **client** SPIFFE path must start with `/nke-service/` or the agent
  rejects the handshake.
- The **agent** SVID is whatever this node's SPIRE registration issues. The
  site SPIRE server must have a workload entry for unix uid 0 (root
  `nke-agent`) parented to the node's agent SPIFFE ID.

Join-token attestation and those workload entries are the operator's SPIRE
server, not this image.

## Ports and paths

| What | Value |
|---|---|
| `nke-agent` gRPC | TCP `15005` |
| SPIFFE workload API | `unix:///tmp/spire-agent/public/api.sock` |
| Agent state | `/var/lib/nke/state.json` |
| Agent env | `/etc/default/nke-agent` (`AGENT_ARGS`) |
| SPIRE config | `/etc/spire-agent/agent.conf` (rendered, not a baked secret) |
| SPIRE data | `/var/lib/spire-agent` |

## Out of scope for this image

- kubeadm / kubelet / container runtime as a cluster-ready stack
- CNI, GPU drivers, NVIDIA fabric
- TPM attestor plugin and swtpm
- `--disable-mtls` as a supported production mode

## How to prove it

Boot a VM from the image, feed it the cloud-init files above against a SPIRE
server that has issued a join token and a `/nke-service/...` identity for the
checker, then:

```bash
nke-os-image-check --endpoint <node-ip>:15005
```

The checker is a **client**. It uses the local workload API (same pattern as
the controller). It does not embed a SPIRE server.
