# Build and verify your own node image

This page describes how to build the reference node OS image from the `os-image/` recipe in `dsx-ai-factory/nvke`, boot it, and check it with `nvke-os-image-check`. Use it to develop and test changes to the image.

Run the checker against every image you build, in the environment where its nodes will run. A pass shows that a node booted from the image attests to your SPIRE server, and that its `nvke-agent` accepts mTLS calls from an NVKE service identity, which is what the management plane needs to manage the node.

To build the node images you deploy, use NVIDIA Kubernetes Recipes (NVKR), as described in [Get NVKE artifacts from NVKR and write release manifests](build-node-images.md#the-node-os-image).

> **Note:** The image is built from Canonical Ubuntu. For internal/dev use only - contact Canonical before commercial production use.

The build produces one QCOW2 disk image: Ubuntu Server with the NVIDIA-tuned kernel (`linux-nvidia` on amd64, `linux-nvidia-64k` on arm64), `nvke-agent` and the SPIRE agent. It is not a ready Kubernetes node: `nvke-agent` installs Kubernetes later, and the NVIDIA GPU Operator installs GPU drivers. The agent is built from your checkout's `agent/` source. [SPEC](https://github.com/dsx-ai-factory/nvke/blob/main/os-image/SPEC.md) is the image contract.

## Before you begin

- You have read [Plan your installation](plan-installation.md).
- You have a Linux build host of the image's architecture with a writable `/dev/kvm`. A build takes about 15 minutes with KVM and hours under TCG emulation, which the build uses when KVM is unavailable. The build VM uses 4 vCPUs, 8 GiB of memory and a 12 GiB disk by default.
- You have the build tools: Packer 1.15.4, Python 3.10 or later, `curl`, `ssh-keygen`, QEMU (`qemu-system-x86_64` or `qemu-system-aarch64`, and `qemu-img`), `cloud-localds`, `swtpm`, UEFI firmware (OVMF for amd64, AAVMF for arm64), `xorriso`, `p7zip-full` and `dpkg-dev`.
- You have Go, at the version in `go.work`, and a C compiler. The build compiles `nvke-agent` against the host's glibc, so the host must run the image's Ubuntu release or an older one. On a newer Ubuntu the build compiles the packages in a NixOS VM instead; on another Linux distribution, set `NKE_PACKAGE_BUILDER=nix` to do the same. The VM needs Nix with `nix-command` and `flakes` enabled; see [Nix setup](https://github.com/dsx-ai-factory/nvke/blob/main/os-image/nix/README.md).
- You have a clean checkout of `dsx-ai-factory/nvke`. The build refuses to run on uncommitted or untracked changes.
- You have a SPIRE server you control, with the `join_token` node attestor enabled. For a test, a local one is enough; see [Run a local SPIRE server](#run-a-local-spire-server). The image contains no SPIRE address, trust domain, trust bundle or join token; you supply them at first boot.
- You have a hypervisor or machine that boots a UEFI QCOW2 with cloud-init user data and gives the node an address by DHCPv4. The image ignores cloud-init network configuration. The checker must reach the node on TCP port 15005. The FDE variant also needs a TPM 2.0 device.

## Build the image

1. Choose the variant, architecture and Ubuntu release.

   | Variable | Values | Default |
   |---|---|---|
   | `VARIANT` | `non-fde` or `fde` | `non-fde` |
   | `ARCH` | `amd64` or `arm64` | `amd64` |
   | `UBUNTU_RELEASE` | `26.04.1` or `24.04.5` | `26.04.1` |

   - `non-fde` has an unencrypted LVM root volume that grows to fill the disk on first boot. Use it unless you need disk encryption.
   - `fde` encrypts the root volume with LUKS2. Until first boot binds the key to the node's TPM, the disk also opens with the build-time passphrase. After the bind removes the passphrase, losing or resetting the TPM makes the disk unrecoverable. See [Full disk encryption](https://github.com/dsx-ai-factory/nvke/blob/main/os-image/docs/full-disk-encryption.md).

2. From the repository root, build the image.

   ```bash
   make -C os-image image-native ARCH=amd64 VARIANT=non-fde
   ```

   `image-native` fails early if KVM is unavailable. To build without KVM, use `make -C os-image image` with the same variables. For the FDE variant, also set the build-time passphrase:

   ```bash
   make -C os-image image-native ARCH=amd64 VARIANT=fde LUKS_PASSPHRASE=<PASSPHRASE>
   ```

   The default passphrase, `changeit`, is a placeholder.

   The build downloads the Ubuntu installer ISO, builds the Debian packages, runs an unattended Ubuntu install in a QEMU VM under Packer, and compresses the result.

3. Find the image in `os-image/packer/output-non-fde/` (or `output-fde/`). Its name carries the variant, release and architecture, for example `nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2`.

4. For the non-FDE variant, boot-test the image.

   ```bash
   make -C os-image boot-smoke REQUIRE_ACCELERATION=1
   ```

   If you built without KVM, omit `REQUIRE_ACCELERATION=1`. The test boots a copy of the image and checks that cloud-init completes, that `nvke-agent` and `spire-agent` are installed and enabled, and that the root filesystem grew. It does not contact SPIRE. For the FDE variant, run `os-image/demo-fde.sh`; `--help` prints its options.

To rebuild, first remove the previous output with `make -C os-image/packer clean-non-fde-output` or `clean-fde-output`.

## Build `nvke-os-image-check`

1. From the repository root, build the checker.

   ```bash
   make -C os-image build
   ```

   The binary is `os-image/bin/nvke-os-image-check`.

2. Copy it to the host that runs the checker. That host needs a SPIRE agent configured for your server and trust domain, with the `join_token` node attestor and the `unix` workload attestor. The next section issues its join token.

## Run a local SPIRE server

Skip this section if you have a SPIRE server. Otherwise, run one on the checker host, with a SPIRE agent beside it for the checker. Use SPIRE release binaries for your host.

1. Write `server.conf`.

   ```text
   server {
     bind_address = "0.0.0.0"
     bind_port = "8081"
     socket_path = "/tmp/spire-server/private/api.sock"
     trust_domain = "<TRUST_DOMAIN>"
     data_dir = "./server-data"
   }
   plugins {
     DataStore "sql" { plugin_data { database_type = "sqlite3" connection_string = "./server-data/datastore.sqlite3" } }
     NodeAttestor "join_token" { plugin_data {} }
     KeyManager "memory" { plugin_data {} }
   }
   ```

   `<TRUST_DOMAIN>` is a name you choose, such as `example.org`. Nodes reach the server on port 8081.

2. Start the server, and save its trust bundle for the agents.

   ```bash
   spire-server run -config server.conf
   ```

   ```bash
   spire-server bundle show > bundle.pem
   ```

3. Write `agent.conf` for the checker host's agent.

   ```text
   agent {
     server_address = "127.0.0.1"
     server_port = "8081"
     socket_path = "/tmp/spire-agent/public/api.sock"
     trust_bundle_path = "./bundle.pem"
     trust_domain = "<TRUST_DOMAIN>"
     data_dir = "./agent-data"
   }
   plugins {
     NodeAttestor "join_token" { plugin_data {} }
     KeyManager "disk" { plugin_data { directory = "./agent-data" } }
     WorkloadAttestor "unix" { plugin_data {} }
   }
   ```

   You start this agent with the checker host's join token in the next section:

   ```bash
   spire-agent run -config agent.conf -joinToken <CHECKER_HOST_TOKEN>
   ```

## Register SPIRE identities

Run these commands on your SPIRE server.

1. Create a join token for the node's SPIRE agent.

   ```bash
   spire-server token generate -spiffeID spiffe://<TRUST_DOMAIN>/<NODE_AGENT_PATH> -ttl 3600
   ```

   `<NODE_AGENT_PATH>` is a path you choose for the node. The token works once and expires after `-ttl` seconds, so boot the node within the hour.

2. Register `nvke-agent`, which runs as root, under the node's agent.

   ```bash
   spire-server entry create \
     -parentID spiffe://<TRUST_DOMAIN>/<NODE_AGENT_PATH> \
     -spiffeID spiffe://<TRUST_DOMAIN>/<AGENT_WORKLOAD_PATH> \
     -selector unix:uid:0
   ```

   `<AGENT_WORKLOAD_PATH>` is any path you choose.

3. Create a join token for the checker host's SPIRE agent, and start that agent with it.

   ```bash
   spire-server token generate -spiffeID spiffe://<TRUST_DOMAIN>/<CHECKER_HOST_PATH> -ttl 3600
   ```

4. Register the checker under the checker host's agent.

   ```bash
   spire-server entry create \
     -parentID spiffe://<TRUST_DOMAIN>/<CHECKER_HOST_PATH> \
     -spiffeID spiffe://<TRUST_DOMAIN>/tenant/nvke/service/<CHECKER_NAME> \
     -selector unix:uid:<CHECKER_UID>
   ```

   `<CHECKER_UID>` is the user ID that runs the checker. `nvke-agent` accepts only callers under `/tenant/nvke/service/` or a legacy prefix listed in [Checker](https://github.com/dsx-ai-factory/nvke/blob/main/os-image/README.md#checker).

To attest the node by TPM instead of a join token, see [Prepare nodes](prepare-nodes.md).

## Boot with cloud-init

1. Write the first-boot user data.

   ```yaml
   #cloud-config
   ssh_authorized_keys:
     - <SSH_PUBLIC_KEY>
   write_files:
     - path: /etc/nke/spire.env
       permissions: "0600"
       content: |
         SPIRE_SERVER_ADDRESS=<SPIRE_SERVER_ADDRESS>
         SPIRE_SERVER_PORT=<SPIRE_SERVER_PORT>
         SPIRE_TRUST_DOMAIN=<TRUST_DOMAIN>
         SPIRE_JOIN_TOKEN=<JOIN_TOKEN>
     - path: /etc/nke/trust-bundle.pem
       permissions: "0644"
       content: |
         <SPIRE_TRUST_BUNDLE_PEM>
   ```

   - `<SSH_PUBLIC_KEY>`: your key, for logging in as `ubuntu`.
   - `<SPIRE_SERVER_ADDRESS>` and `<SPIRE_SERVER_PORT>`: where the node reaches your SPIRE server.
   - `<JOIN_TOKEN>`: the node token from the previous section.
   - `<SPIRE_TRUST_BUNDLE_PEM>`: the output of `spire-server bundle show`, indented under `content`.

2. Boot the image with this user data on your hypervisor or provisioning system. `make -C os-image run-vm` cannot boot this user data: it injects only an SSH key and forwards only port 22.

On first boot the node writes its SPIRE agent configuration from these files, `spire-agent` attests, and `nvke-agent` starts once it has an identity.

## Verify

1. On the checker host, run the checker against the node.

   ```bash
   ./nvke-os-image-check --endpoint <NODE_IP>:15005
   ```

   If the SPIRE agent's workload API socket is not `/tmp/spire-agent/public/api.sock`, add `--spiffe-socket unix://<SOCKET_PATH>`.

   A pass exits 0 and logs `nvke-agent reachable` with the agent version. It means the mTLS handshake succeeded in both directions and the agent answered a gRPC call. A failure exits 1 and logs `probe failed` with the stage that failed. [Checker](https://github.com/dsx-ai-factory/nvke/blob/main/os-image/README.md#checker) lists every flag and error.

2. For the FDE variant, confirm the TPM binding on the node, as root.

   ```bash
   /usr/local/bin/luks-tpm-verify
   ```

   Exit code 0 means the disk is bound to this node's TPM.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Build stops with `source checkout is dirty` | Commit or stash your changes, including untracked files. |
| Build stops with `output-non-fde already exists` | Run `make -C os-image/packer clean-non-fde-output` (or `clean-fde-output`). |
| `image-native` stops with `cannot be hardware accelerated on this` | No usable KVM. Check that `/dev/kvm` is writable and the host architecture matches `ARCH`, or build with `make -C os-image image`. |
| `journalctl -u nvke-agent` on the node shows `timed out waiting for SPIFFE workload API socket` | The node has not attested. Check `journalctl -u nvke-spire-configure -u spire-agent`, then the server address, trust bundle and join token in the user data. |
| Checker exits 1 with `probe failed` | The error names the failed stage; [Checker](https://github.com/dsx-ai-factory/nvke/blob/main/os-image/README.md#checker) maps each to its cause. |
| FDE: `luks-tpm-verify` exits 1 or 3 | 1: the disk is not bound yet, because first boot has not finished or the bind failed. 3: the node has no TPM device, or a required tool is missing. Read `/var/log/luks-tpm-firstboot.log` before you reboot the node. |

## Next steps

- [Prepare nodes](prepare-nodes.md), to boot development nodes from the image and attest them by TPM.
- [Supported platforms and versions](../overview/supported-platforms.md) for the Ubuntu releases, architectures and kernels NVKE supports.
