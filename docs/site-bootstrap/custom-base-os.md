# Custom Base OS

If you need to bootstrap a tier-0 cluster on a **vanilla Ubuntu image** (rather than a pre-built NKE node image), you can add the necessary installation steps to your cloud-init configuration.

Before proceeding, write your [inventory file](configuration.md) so it can be placed on the init node by cloud-init.

## Cloud-Init Example

Below is a complete cloud-init configuration for bootstrapping a tier-0 cluster on a custom base OS. This installs Kubernetes components, `nke-agent`, and `nkectl`, then runs the bootstrap on first boot.

```yaml
#cloud-config
users:
  - default
  - name: nke-admin
    gecos: NKE Admin User
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: root
    lock_passwd: true
autoinstall:
  version: 1
  identity:
    hostname: <node-name>
    password: $6$...  # hashed password
    username: nke-admin
  packages:
    - chrony
    - jq
  ntp:
    enabled: true
    ntp_client: chrony
  keyboard:
    layout: us
    toggle: null
    variant: ""
  locale: en_US
  ssh:
    allow-pw: true
    authorized-keys: []
    install-server: true
  late-commands:
    - curtin in-target --target=/target -- sed -i 's|^GRUB_CMDLINE_LINUX=.*|GRUB_CMDLINE_LINUX="ipv6.disable=1 console=ttyS1,115200"|' /etc/default/grub
    - curtin in-target --target=/target -- update-grub
  user-data:
    write_files:
      # nkectl inventory file (bootstrap host only — omit on other nodes)
      - path: /etc/nkectl/inventory.yaml
        owner: root:root
        permissions: '0640'
        content: |
          clusterName: "nke-site"
          cluster:
            nodes:
              - name: "ctrl-1"
                ip: "10.0.0.10"
              - name: "ctrl-2"
                ip: "10.0.0.11"
              - name: "ctrl-3"
                ip: "10.0.0.12"
            k8sVersion: "1.34.2"
            cniName: "ovn-kubernetes"
            podCidr: "192.168.0.0/16"
            serviceCidr: "10.96.0.0/12"
            nodeAgentPort: "15005"
          registry:
            bootstrapRepo: "nvcr.io/org/cloud-workflow-engine"
            bootstrapChart: "cloud-workflow-engine"
    runcmd:
      # Expand root LVM volume
      - [sh, -c, "lvextend -l +100%FREE /dev/ubuntu-vg/ubuntu-lv"]
      - [sh, -c, "resize2fs /dev/mapper/ubuntu--vg-ubuntu--lv"]
      - [sh, -c, "mkdir -m 755 /etc/apt/keyrings"]
      - [sh, -c, "sysctl -p"]
      - [sh, -c, "systemctl restart procps"]

      # Install Kubernetes components (replace v1.32 with your target minor version)
      - [sh, -c, "curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.32/deb/Release.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg"]
      - [sh, -c, "echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.32/deb/ /' | tee /etc/apt/sources.list.d/kubernetes.list"]
      - [sh, -c, "apt-get update"]
      - [sh, -c, "apt-get install -y kubelet kubeadm kubectl"]
      - [sh, -c, "apt-mark hold kubelet kubeadm kubectl"]

      # TODO: Replace apt repo URL with confirmed public URL before publishing.
      # Install nke-agent from the NKE apt repository
      - [sh, -c, "curl -fsSL -o /etc/apt/keyrings/nke.asc https://packages.nvidia.com/nke/nke-apt-public.key"]
      - [sh, -c, "echo 'deb [signed-by=/etc/apt/keyrings/nke.asc] https://packages.nvidia.com/nke/apt nke-noble main' | tee /etc/apt/sources.list.d/nke-apt.list"]
      - [sh, -c, "apt-get update"]
      - [sh, -c, "apt-get install -y nke-agent"]
      - [sh, -c, "echo 'AGENT_ARGS=\"--disable-mtls --secure-server-port 15005\"' > /etc/default/nke-agent"]
      - [sh, -c, "systemctl restart nke-agent"]

      # TODO: Replace with real release URL once the repo is public.
      # Download and install nkectl (bootstrap host only)
      - [sh, -c, "curl -fsSL -o /usr/local/bin/nkectl https://github.com/NVIDIA/nke/releases/latest/download/nkectl-linux-amd64 && chmod +x /usr/local/bin/nkectl"]

      # Run bootstrap (bootstrap host only)
      - [sh, -c, "REGISTRY_API_KEY=<your-registry-api-key> nkectl bootstrap site -i /etc/nkectl/inventory.yaml >> /var/log/nkectl-bootstrap.log 2>&1"]
```

## Key Components

- **Kubernetes Installation**: Adds the Kubernetes apt repository and installs kubelet, kubeadm, and kubectl. Marks packages held to prevent automatic upgrades.
- **nke-agent**: Installed from the NKE apt repository; configured with `--disable-mtls` and `--secure-server-port 15005` for pre-provisioned node bootstrap.
- **nkectl**: Install it only on the **bootstrap host**. Omit the nkectl download and bootstrap commands on other nodes.

## Bootstrap Host and Other Nodes

| | Bootstrap Host | Other Nodes |
|---|---|---|
| Kubernetes packages | ✓ | ✓ |
| nke-agent | ✓ | ✓ |
| `/etc/nkectl/inventory.yaml` | ✓ | ✗ |
| `nkectl` download + bootstrap run | ✓ | ✗ |

On other nodes, omit the `write_files` section and the last two `runcmd` lines
(the nkectl download and bootstrap run).

## Customizing

- Update `cluster.nodes` with your actual node names and IPs
- Set `registry.bootstrapRepo` and `registry.bootstrapChart` to your CWE registry path
- Supply `REGISTRY_API_KEY` via your secrets management (not hardcoded in cloud-init)
- Update the Kubernetes repository minor version to match `cluster.k8sVersion` in the inventory
- Update hostnames, passwords, and SSH authorized keys for your environment

> **Ensure `/etc/apt/keyrings` exists** before installing Kubernetes packages — the `mkdir` step above creates it.

Bootstrap logs are in `/var/log/nkectl-bootstrap.log` on the bootstrap host.
