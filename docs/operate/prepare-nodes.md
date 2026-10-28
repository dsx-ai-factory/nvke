# Prepare nodes

This page describes how to bring the management cluster's nodes from a first boot of your node image to the state that `nvkectl bootstrap site` requires. You do it after you build the node image and before you bootstrap the management cluster.

## Before you begin

- You have read [Plan your installation](plan-installation.md).
- You have a node image built as described in [Build and host node OS images](build-node-images.md), for the CPU architecture of your nodes.
- You have an odd number of machines, such as one, three or five, that meet [Supported platforms and versions](../overview/supported-platforms.md). Each has a TPM 2.0 device, and your provisioning system can boot it from the image and pass it cloud-init user data.
- Each node keeps the same IP address across reboots. The image configures every `e*` network interface with DHCP and disables cloud-init network configuration, so reserve each node's address in DHCP.
- You have chosen the bootstrap host and know the IP address the nodes use to reach it. It can be one of the nodes.
- You have chosen a SPIFFE trust domain, such as `mgmt.example.com`, and have your SPIRE certificate authority (CA) certificate and key as files. The node image's SPIRE agent does not start without a trust bundle, and the SPIRE server that `nvkectl` deploys has no certificate the nodes could trust until the bootstrap runs, so nodes that attest with their TPM need a CA you bring. The install also makes this CA the upstream authority of the management cluster's SPIRE server, so workload cluster nodes trust it through the bundle NVKE puts in their user data; see [Register node images for provisioning](build-node-images.md#what-nvke-puts-in-the-user-data).
- Your network lets the bootstrap host reach every node on the `nvke-agent` port (15005 by default), and lets every node reach the bootstrap host on 30081.

## What the image provides

The node image is not a ready Kubernetes node. It carries the node agent and the SPIRE agent, and stages the Kubernetes and Container Network Interface (CNI) packages for you to install.

| Component | In the image | What you add |
|---|---|---|
| `nvke-agent` | Installed and enabled, with mutual TLS (mTLS) on and listening on 15005. It starts once the SPIRE agent has an identity. | Nothing. |
| `spire-agent`, its TPM attestor and `get_tpm_pubhash` | Installed and enabled. | The SPIRE server address, trust domain and trust bundle, in user data. |
| Kubernetes packages: kubeadm, kubelet, kubectl and containerd | Staged in a local package repository at `/usr/lib/nvke/debs`, one `nvke-kubeadm-distro-<version>` package per Kubernetes version. | Install the package for your Kubernetes version. |
| OVN-Kubernetes CNI packages | Staged in the same repository, for version 2.4.2. | Install the charts and images packages. |
| Open vSwitch (`doca-openvswitch-switch`) | Installed. This is NVIDIA's DOCA build of Open vSwitch, from the DOCA apt repository, running in kernel mode. | Nothing. |

## Write the user data

1. Write a cloud-init user-data file. Every node can use the same one.

   ```yaml
   #cloud-config
   write_files:
     - path: /etc/nke/spire.env
       permissions: "0600"
       content: |
         SPIRE_SERVER_ADDRESS=<BOOTSTRAP_HOST_IP>
         SPIRE_SERVER_PORT=30081
         SPIRE_TRUST_DOMAIN=<TRUST_DOMAIN>
     - path: /etc/nke/trust-bundle.pem
       permissions: "0644"
       content: |
         <SPIRE_CA_CERT_PEM>
   runcmd:
     - DEBIAN_FRONTEND=noninteractive apt-get install -y nvke-kubeadm-distro-<K8S_VERSION>
     - DEBIAN_FRONTEND=noninteractive apt-get install -y nvke-cni-ovn-images-2.4.2 nvke-cni-ovn-charts-2.4.2
   ```

   - `<BOOTSTRAP_HOST_IP>`: the IP address the nodes use to reach the bootstrap host. During the bootstrap, the SPIRE server listens there on port 30081.
   - `<TRUST_DOMAIN>`: your SPIFFE trust domain. Set the same value as `spire.trustDomain` in the site spec. If the site spec leaves it unset, `nvkectl` uses `nvkectl-bootstrap.nke.local`, so set it explicitly in both places.
   - `<SPIRE_CA_CERT_PEM>`: the PEM contents of the CA certificate you pass as `NVKECTL_SPIRE_CA_CERT_FILE`, with every line indented to match the placeholder.
   - `<K8S_VERSION>`: the Kubernetes version you set as `cluster.k8sVersion` in the site spec, without a leading `v`: `1.36.3`, the supported version. The image also stages 1.34.2, 1.35.3 and 1.37.2, which are not supported; see [Supported platforms and versions](../overview/supported-platforms.md).

   The image stages OVN-Kubernetes 2.4.2, the version the bootstrap uses when the site spec leaves `cluster.cniVersion` unset.

   Do not set `SPIRE_JOIN_TOKEN`. Without it, the SPIRE agent attests with the node's TPM, which is how the bootstrap's SPIRE server admits nodes; it issues no join tokens. Install the Kubernetes package before the CNI images package: the images package loads its image into containerd, which the Kubernetes package provides.

   If you install the management plane without a release description, you later copy two container images to each node over `ssh`; see [Install the management plane](install-management-plane.md#stage-the-controller-images-on-the-management-nodes). Add a `users:` entry for that with your SSH public key in `ssh_authorized_keys` and `sudo: ALL=(ALL) NOPASSWD:ALL`. The image accepts no password logins.

2. Boot each node from the image with the user data.

   On first boot, cloud-init writes the SPIRE files, a configuration service renders `/etc/spire-agent/agent.conf` from them, and `runcmd` installs the packages from the local repository without network access. The SPIRE agent then retries until the bootstrap's SPIRE server is reachable.

## Record each node's TPM thumbprint

1. On each node, as root, print the TPM endorsement-key (EK) thumbprint.

   ```bash
   get_tpm_pubhash
   ```

   The output is 64 lowercase hex digits. If the command reports `device or resource busy`, the SPIRE agent is using the TPM for an attestation attempt; run it again.

2. Record each node's name, IP address and thumbprint. You list them in `hosts.csv` when you write the site spec. A node without a thumbprint in the site spec cannot attest, so it cannot join.

## If the image lacks a component

Each component the bootstrap needs is a Debian package. If a check in [Verify](#verify) shows one is missing, rebuild the image as described in [Build and host node OS images](build-node-images.md).

| Component | Package |
|---|---|
| Node agent | `nvke-agent` |
| SPIRE agent, TPM attestor and `get_tpm_pubhash` | `spire-agent` |
| Kubernetes for one version | `nvke-kubeadm-distro-<K8S_VERSION>` |
| OVN-Kubernetes | `nvke-cni-ovn-charts-2.4.2` and `nvke-cni-ovn-images-2.4.2` |
| Open vSwitch | `doca-openvswitch-switch`. The agent also accepts the stock `openvswitch-switch` package. |

If `nvke-kubeadm-distro-<K8S_VERSION>` is the missing package, the image was built without that Kubernetes version. Choose a staged version, or rebuild the image with it.

Do not add `--disable-mtls` to `AGENT_ARGS` in `/etc/default/nvke-agent` to work around a missing SPIRE agent. Plaintext gRPC is for development only.

## Verify

Run these checks on every node, as root.

1. Check that cloud-init finished.

   ```bash
   cloud-init status
   ```

   The output shows `status: done`. The bootstrap checks this on every node before it runs kubeadm, and fails on `status: error`.

2. Check that the packages are installed.

   ```bash
   dpkg -l nvke-agent spire-agent '*openvswitch-switch' 'nvke-kubeadm-distro-*' 'nvke-cni-ovn-*'
   ```

   Each package is listed with status `ii`, including `doca-openvswitch-switch` (or `openvswitch-switch` on a node you prepared with the stock package), `nvke-kubeadm-distro-<K8S_VERSION>`, `nvke-cni-ovn-charts-2.4.2` and `nvke-cni-ovn-images-2.4.2`. A staged package that is not installed is not listed, and `dpkg` reports that no package matches.

   ```bash
   kubeadm version -o short
   ```

   The output is `v<K8S_VERSION>`, matching the site spec.

3. Check that Open vSwitch is running.

   ```bash
   ovs-vsctl show
   ```

   The command prints the Open vSwitch configuration without an error. OVN-Kubernetes uses the node's Open vSwitch.

4. Check the SPIRE agent configuration.

   ```bash
   grep -E 'server_address|server_port|trust_domain' /etc/spire-agent/agent.conf
   ```

   The output shows the bootstrap host's address, port `30081` and your trust domain. If the file is missing, check that `/etc/nke/spire.env` and `/etc/nke/trust-bundle.pem` exist, then run `journalctl -u nvke-spire-configure`, which names any missing file or value.

5. Check that the SPIRE agent is running.

   ```bash
   systemctl is-active spire-agent
   ```

   The output is `active`: the agent runs and retries attestation until the bootstrap's SPIRE server answers. If the output is `activating` or `failed`, the agent cannot start; `journalctl -u spire-agent` shows the error. Until the bootstrap, `nvke-agent` keeps restarting while it waits for the SPIFFE workload API socket. That is expected.

During the bootstrap, each node's SPIRE agent logs `Node attestation was successful` and `nvke-agent` starts. To watch for it, run `journalctl -u spire-agent -f` on a node.

Workload-cluster nodes are not prepared this way. NVIDIA Infra Controller (NICo) provisions them from your node image, and the management plane supplies their user data from the release, including the Kubernetes and CNI packages to install; see [Register node images for provisioning](build-node-images.md#register-the-image-for-provisioning).

## Next steps

- [Bootstrap the management cluster](bootstrap-management-cluster.md) with the node names, IP addresses and TPM thumbprints you recorded.
