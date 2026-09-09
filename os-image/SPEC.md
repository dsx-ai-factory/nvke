# NKE reference node image — contract

This is the contract an OS image must satisfy for NKE to manage a node.
It is **not** a cluster-ready Kubernetes image: kubeadm and GPU drivers are
installed later through `nke-agent`. The image includes the Calico manifests
package that `nke-agent` installs after kubeadm init. The bar for v1 is that
the controller can reach `nke-agent` over SPIFFE mTLS.

The in-tree Packer recipe (`packer/`) is one implementation of this contract,
not the only allowed one.

## Must have (baked into the image)

| Item | Requirement |
|---|---|
| OS | Ubuntu 26.04 LTS, `linux/amd64` or `linux/arm64`. |
| `nke-agent` | Built from this repository's `agent/` source at bake time and installed at `/usr/local/bin/nke-agent`. systemd unit `nke-agent.service` **enabled**. Listens on TCP **15005**. mTLS is on (no `--disable-mtls` in `/etc/default/nke-agent`). The unit waits for the SPIRE workload API before exec. |
| SPIRE agent | `spire-agent` at `/usr/local/bin/spire-agent`, unit `spire-agent.service` **enabled**. Workload API socket at `unix:///tmp/spire-agent/public/api.sock` (the `nke-agent` default). |
| Unix workload attestor | SPIRE `WorkloadAttestor "unix"` so root `nke-agent` can fetch an X.509 SVID. |
| Node attestor | Join-token (`NodeAttestor "join_token"`) by default. The image also includes the TPM agent plugin at `/usr/local/lib/spire-agent/tpm_attestor_agent`, `/usr/local/bin/get_tpm_pubhash`, and TPM device access through the `tss` group for the optional bootstrap mTLS mode below. |
| Calico manifests | Debian package `nke-cni-calico-manifests-3.30.1` installed. Its version must match the supported bootstrap Calico version. |
| Unit ordering | `nke-spire-configure.service` before `spire-agent.service` before `nke-agent.service`. `nke-agent` waits for the workload API socket before exec. |
| First-boot renderer | `/usr/lib/nke/spire-configure.sh`, driven by `nke-spire-configure.service`. Renders `/etc/spire-agent/agent.conf` from instance identity. Does nothing unless `/etc/nke/spire.env` exists. |

## Must not bake (instance identity)

These are **site- or node-specific**. They arrive via cloud-init (or equivalent
user-data) on first boot, never at image-build time:

- SPIRE server address and port
- Trust domain
- Trust bundle (PEM)
- Join token when join-token attestation is selected

The Packer build **enables** the units but does not start them against a live
SPIRE server, and it runs `cloud-init clean` so the next boot is a real
instance.

### Cloud-init input

Write these two files. Set `spire.env` mode to `0600` so only root can change
the node identity endpoint.

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

## Addendum: `nkectl bootstrap site` identity modes

The site spec selects one mode for the whole bootstrap. Either every node has
a `tpmEkThumbprint`, or no node has one.

### Single node without mTLS

A single-node site can omit its TPM thumbprint. In this mode, `nkectl` does not
deploy SPIRE and tells CWE that the CAPI SPIRE attestation method is `none`.
Cloud-init must select the plaintext agent mode:

```yaml
#cloud-config
runcmd:
  - [/usr/lib/nke/configure-nke-agent-plaintext.sh]
```

The helper first calls `GetKubeadmStatus` over plaintext gRPC. If the agent
already responds, it changes nothing. Otherwise, it writes the systemd
override, disables the SPIRE units, and restarts `nke-agent` with
`--disable-mtls`.

This mode is only for the single-node bootstrap path. It does not change the
default NKE node-agent security model.

### Bootstrap with TPM mTLS

For mTLS, collect every node's TPM EK thumbprint with `get_tpm_pubhash` and put
the values in the site spec. `nkectl` then deploys SPIRE into the bootstrap
Kind cluster, allows those thumbprints, and exposes the server on TCP port
`30081` of the init host.

Before the hosts boot, choose the init host and generate a bootstrap CA. Give
the CA certificate to every host through cloud-init. Give the same certificate
and its private key to `nkectl` through `spire.caCertFile` and
`spire.caKeyFile` in the site spec. The certificate must have `CA:TRUE` and
`keyCertSign`. This command creates a suitable short-lived test CA:

```bash
openssl req -x509 -newkey rsa:4096 -sha256 -nodes -days 7 \
  -subj '/CN=nkectl bootstrap CA' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -keyout spire-ca.key -out spire-ca.crt
chmod 0600 spire-ca.key
```

All hosts point at the init host IP:

```yaml
#cloud-config
write_files:
  - path: /etc/nke/spire.env
    permissions: "0600"
    content: |
      SPIRE_SERVER_ADDRESS=192.0.2.10
      SPIRE_SERVER_PORT=30081
      SPIRE_TRUST_DOMAIN=nkectl-bootstrap.nke.local
      SPIRE_ATTESTATION_METHOD=tpm
  - path: /etc/nke/trust-bundle.pem
    permissions: "0644"
    content: |
      -----BEGIN CERTIFICATE-----
      ...
      -----END CERTIFICATE-----
  - path: /etc/default/nke-agent
    permissions: "0644"
    content: |
      AGENT_ARGS=""
runcmd:
  - [systemctl, daemon-reload]
  - [systemctl, enable, nke-spire-configure.service, spire-agent.service, nke-agent.service]
  - [systemctl, restart, --no-block, nke-spire-configure.service, spire-agent.service, nke-agent.service]
```

The image enables `nke-spire-configure.service`, `spire-agent.service`, and
`nke-agent.service`. After cloud-init writes these files and completes, systemd
renders the TPM configuration, starts SPIRE, and starts `nke-agent` after its
SPIFFE workload socket becomes available. The SPIRE agent retries until
`nkectl bootstrap site` exposes the server.

## Must have (FDE variant)

`nke-node-fde.pkr.hcl` is an **opt-in** build variant, not the default image.
It satisfies everything above, plus:

| Item | Requirement |
|---|---|
| Root volume | LVM-on-LUKS2 (`ubuntu-vg`/`ubuntu-lv` on a LUKS2 container), created by `autoinstall-lvm.pkrtpl` (`luks_enabled = true`) at install time. |
| TPM binding | `luks-tpm-firstboot.service` binds the LUKS key to the node's own TPM 2.0 (Clevis `tpm2`) on first real boot — TPM binding is machine-specific and cannot happen at image-build time. TPM-only policy by default (no PCR/measured-boot binding); see [docs/full-disk-encryption.md](docs/full-disk-encryption.md) for the tradeoff. |
| Passphrase removal | The build-time LUKS passphrase slot is removed once the TPM bind is verified unsealable. After that, the volume unlocks **only** via that node's TPM — moving the disk to different hardware, or losing the TPM state, makes it unrecoverable by design. |
| No key material off-image | The only secret on the cidata/autoinstall medium is the shared build-time passphrase (documented default, not a per-node secret); no per-node key, join token, or TPM-derived material is ever written to cloud-init, user-data, or the genesis medium. |
| Unattended first unlock | `GRUB_CMDLINE_LINUX_DEFAULT` carries `luks.key=<passphrase>` (build-time, same shared default) until `luks-tpm-firstboot.sh` strips it post-bind; `files/luks/luks-cmdline-script` reads it. Matches how production feeds an iPXE `luks.key=` kernel parameter. |
| Serial console | `GRUB_CMDLINE_LINUX` carries `console=tty0 console=ttyS0,115200`, so boot/kernel messages (including any LUKS prompt) reach a serial console, not only the VGA framebuffer. |
| Node replacement | Each new node image boot re-runs first-boot binding against its own local TPM — no old-TPM/new-TPM migration step exists or is needed. |

## Must have (non-FDE variant, no encryption)

`nke-node-non-fde.pkr.hcl` is also **opt-in**, not the default image. It uses
the Ubuntu installer and autoinstall, but it uses a direct root partition. It
configures no LUKS container, Clevis, TPM binding, or LVM layer. Use it to test
the installer and node services without the FDE storage stack.

| Item | Requirement |
|---|---|
| Root volume | Direct ext4 partition with a separate `/boot` partition, no LVM or LUKS container. Its three partition boundaries match the FDE/LVM image. This lets Forge mount the new root even if the kernel keeps the old table while a stale LVM mapping is active. |
| Storage growth | The Forge qcow imager grows the root partition and filesystem after it writes the image. |
| Serial console | Same `console=tty0 console=ttyS0,115200` GRUB parameter as the FDE variant (installer-based images otherwise carry no `console=` at all). |

## Identity rules (runtime)

These are enforced by `nke-agent`, not by the image:

- Client and agent SVIDs share a trust domain.
- The **client** SPIFFE path must start with `/nke-service/` or the agent
  rejects the handshake.
- The **agent** SVID is whatever this node's SPIRE registration issues. The
  site SPIRE server must have a workload entry for unix uid 0 (root
  `nke-agent`) parented to the node's agent SPIFFE ID.

Join-token or TPM attestation and the workload entries are owned by the SPIRE
server, not by the base image.

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
- GPU drivers and NVIDIA fabric
- `--disable-mtls` outside the single-node bootstrap mode described above
- Kubernetes Secrets / etcd encryption-at-rest — a controller/agent concern,
  not an image-build concern; out of scope for `os-image/`

## How to prove it

Boot a VM from the image, feed it the base cloud-init files above against a
SPIRE server that has issued a join token and a `/nke-service/...` identity for the
checker, then:

```bash
nke-os-image-check --endpoint <node-ip>:15005
```

The checker is a **client**. It uses the local workload API (same pattern as
the controller). It does not embed a SPIRE server.
