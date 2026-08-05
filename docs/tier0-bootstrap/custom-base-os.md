# Custom Base OS

If you need to bootstrap a tier-0 cluster on a **vanilla Ubuntu image** (rather than a pre-built NKE node image), you can add the necessary installation steps to your cloud-init configuration.

Before proceeding, prepare your deploy repository and credentials as described in [Configuration](configuration.md) and [Deploy Repository](deploy-repository.md).

## Cloud-Init Example

Below is a complete cloud-init configuration for bootstrapping a tier-0 cluster on a custom base OS. This installs Kubernetes components, `nke-agent`, and the `tier0-bootstrap` tool, then starts the bootstrap service.

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
      # Bootstrap configuration and secrets
      - path: /etc/default/tier0-bootstrap
        owner: root:root
        permissions: '0644'
        content: |
          NGC_API_KEY=<your-ngc-api-key>
          FORGE_API_KEY=<your-api-key>
          TIER0_BOOTSTRAP_EXTRA_ARGS=--join-nodes "ctrl-2:10.0.0.11,ctrl-3:10.0.0.12" --argocd-repo "url=<site-deploy-repo>,token=<site-token>,path=<deploy-path>" --auto-install
    runcmd:
      # Expand root LVM volume
      - [sh, -c, "lvextend -l +100%FREE /dev/ubuntu-vg/ubuntu-lv"]
      - [sh, -c, "resize2fs /dev/mapper/ubuntu--vg-ubuntu--lv"]
      - [sh, -c, "mkdir -m 755 /etc/apt/keyrings"]
      - [sh, -c, "sysctl -p"]
      - [sh, -c, "systemctl restart procps"]

      # Install Kubernetes components (replace v1.31 with your target minor version)
      - [sh, -c, "curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.31/deb/Release.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg"]
      - [sh, -c, "echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.31/deb/ /' | tee /etc/apt/sources.list.d/kubernetes.list"]
      - [sh, -c, "apt-get update"]
      - [sh, -c, "apt-get install -y kubelet kubeadm kubectl"]
      - [sh, -c, "apt-mark hold kubelet kubeadm kubectl"]

      # TODO: Replace apt repo URL with confirmed public URL before publishing.
      # Install nke-agent from the NKE apt repository
      - [sh, -c, "curl -fsSL -o /etc/apt/keyrings/nke.asc https://packages.nvidia.com/nke/nke-apt-public.key"]
      - [sh, -c, "echo 'deb [signed-by=/etc/apt/keyrings/nke.asc] https://packages.nvidia.com/nke/apt nke-noble main' | tee /etc/apt/sources.list.d/nke-apt.list"]
      - [sh, -c, "apt-get update"]
      - [sh, -c, "apt-get install -y nke-agent"]

      # TODO: Replace with real release URL once the repo is public.
      # Download and install tier0-bootstrap (init node only)
      - [sh, -c, "curl -fsSL https://github.com/NVIDIA/nke/releases/latest/download/bootstrap-installer.sh | bash"]
      - [sh, -c, "systemctl start tier0-bootstrap"]
```

## Key Components

- **Kubernetes Installation**: Adds the Kubernetes apt repository and installs kubelet, kubeadm, and kubectl. Marks packages held to prevent automatic upgrades.
- **nke-agent**: Installed from the NKE apt repository. Provides cluster join capabilities via gRPC; the bootstrap tool calls it to run `kubeadm init/join`.
- **tier0-bootstrap**: Only include on the **init node**. Omit these last two lines for **join nodes**.

## Init Node vs Join Nodes

| | Init Node | Join Nodes |
|---|---|---|
| Kubernetes packages | ✓ | ✓ |
| nke-agent | ✓ | ✓ |
| `/etc/default/tier0-bootstrap` with `--join-nodes` | ✓ | ✗ |
| `bootstrap-installer.sh` + `systemctl start tier0-bootstrap` | ✓ | ✗ |

For join nodes:
- Include the `write_files` section **only** if you need other configuration
- Remove the `--join-nodes` arg from `TIER0_BOOTSTRAP_EXTRA_ARGS`
- Remove the two bootstrap installer lines from `runcmd`

## Customizing

- Update `--join-nodes` with your actual node names and IPs
- Replace `&lt;site-deploy-repo&gt;`, `&lt;site-token&gt;`, and `&lt;deploy-path&gt;` with your repository details (see [Configuration — argocd.repos](configuration.md#argocdrepos))
- Update the Kubernetes repository minor version to match `cluster.k8sVersion` in your bootstrap config
- Update hostnames, passwords, and SSH authorized keys for your environment

> **Ensure `/etc/apt/keyrings` exists** before installing Kubernetes packages — the `mkdir` step above creates it.

See the [reference repository](deploy-repository.md#reference-repository) for a ready-to-use deploy repo containing the minimum required Application manifests.
