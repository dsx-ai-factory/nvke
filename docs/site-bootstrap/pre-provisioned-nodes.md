# Using Pre-Provisioned Nodes

> **Which path is this?** This guide covers bootstrap on nodes you bring yourself — no SPIRE workload API, no TPM, `--disable-mtls` on `nke-agent`. If your nodes are built from the NKE OS image (SPIRE agent pre-installed, mTLS on by default), see [`os-image/SPEC.md`](../../os-image/SPEC.md) for the image contract and [`docs/first-cluster.md`](../first-cluster.md) Path C for the full flow.

If you already have nodes provisioned and running, you can manually prepare them for tier-0 bootstrap.

> **Tested platforms:** Tier-0 bootstrap on pre-provisioned nodes is tested on **Ubuntu 24.04 (Noble) and 26.04 (Resolute)** with **Kubernetes up to 1.34**. Kubernetes 1.35 and 1.36 are experimental. Ubuntu 22.04 (Jammy) is no longer supported.

## Self-Bootstrap Models: Managed vs Unmanaged

What you get after bootstrap depends on whether your environment includes a [NVIDIA Infra Controller (NICo)](https://github.com/dsx-ai-factory/infra-controller) site, integrated with NKE via [`cluster-api-provider-nico`](https://github.com/dsx-ai-factory/cluster-api-provider-nico).

> **Note:** During bootstrap, NKE uses `cluster-api-provider-provisioned` as a temporary CAPI provider to bootstrap the site controller cluster from pre-provisioned hosts. For production day-2 operations (tenant cluster creation, node add/remove, rolling upgrades), NICo is the supported infrastructure provider, integrated through CAPNICo. The self-managed path is the supported production deployment model. The unmanaged path produces a working Kubernetes cluster but no NKE day-2 operations are possible — it is not a supported NKE deployment.

| | Self-bootstrapped, **self-managed** | Self-bootstrapped, **unmanaged** |
|---|---|---|
| **Environment** | NICo infrastructure is available or will be deployed | No NICo infrastructure, and none is planned |
| **Your responsibility** | Bring an SBOM that includes NKE and its dependencies (NICo, Temporal, Postgres, SPIRE, and related components) | Bring only what you need to run workloads on Kubernetes |
| **After bootstrap** | Install your SBOM on the new cluster so NKE can drive the site through NICo | Use the Kubernetes cluster as-is |
| **Day-2 operations** | Supported — upgrade clusters, add/remove nodes, create tenant clusters | **Not supported** — NKE has no infrastructure provider to perform these operations |

**Example (self-managed):** An operator self-bootstraps a site controller cluster, then installs the NKE SBOM (NKE, NICo, Temporal, Postgres, SPIRE). The cluster becomes self-managing and supports day-2 operations.

**Example (unmanaged):** A team bootstraps Kubernetes on nodes in an environment with no NICo. They get a working cluster but should not expect NKE day-2 features.

## What `nkectl bootstrap site` Does

`nkectl bootstrap site` has one job on pre-provisioned nodes: turn your prepared nodes into a **Kubernetes cluster**. It does not install your site stack, GitOps controller, or application workloads.

After bootstrap completes:

1. **Verify the cluster** is healthy (see [Step 3](#step-3-verify-the-cluster)).
2. **If self-managed**, install your environment SBOM (NKE, NICo, Temporal, Postgres, SPIRE, and the rest).
3. **Deploy your GitOps tooling** — ArgoCD, Flux, or whatever you use for an app-of-apps layer.

## Prerequisites

- 1 or 3 Ubuntu 24.04 or 26.04 nodes (single-node or HA deployment)
- SSH access to all nodes with sudo privileges
- Nodes can reach the internet for downloading packages and artifacts
- Network connectivity between all nodes
- Each node has an available NVMe device for local storage (non-root, unpartitioned, not in use)
- NGC API key (required for pulling NKE container images)
- On the **init node**: `kind` binary and `nerdctl` with containerd installed — `nkectl` uses these to create the ephemeral bootstrap Kind cluster. Docker is not supported. kind states 1.7.0 as the minimum for its nerdctl provider; 2.3.5 is the version CI exercises
- The two images `nkectl` side-loads: `make -C cli images` builds them on the init node. To use images from a registry instead, pass `--controller-image` and `--provisioned-provider-image`, or set `NKECTL_CWE_IMAGE` (e.g. `nvcr.io/<org>/nke-controller:edge`) and `NKECTL_CAPI_PROVISIONED_IMAGE` (e.g. `nvcr.io/<org>/cluster-api-provider-provisioned:edge`)

If you are targeting a self-managed cluster, prepare your environment SBOM and [deploy repository](deploy-repository.md) ahead of time.

## Step 1: Prepare All Nodes

Run these commands on **all nodes** (both join and init nodes):

### Install Kubernetes Components

How you install kubeadm depends on whether your nodes were built from an NKE OS image.

**Nodes built from the NKE OS image** — the image stages versioned Kubernetes Debian packages in a local apt repository at `/usr/lib/nke/debs`, but does not install them. Install them via cloud-init `runcmd` before the node is handed to bootstrap:

```yaml
runcmd:
  - DEBIAN_FRONTEND=noninteractive apt-get install -y nke-kubeadm-distro-<k8s-version>
  - DEBIAN_FRONTEND=noninteractive apt-get install -y nke-cni-ovn-images-<stream> nke-cni-ovn-charts-<stream>
```

Replace `<k8s-version>` with the version matching `cluster.k8sVersion` in your site spec, and `<stream>` with the CNI stream for your Kubernetes version. If you skip this step, bootstrap runs its full 30-minute timeout and fails on `NodePreflight` / `check_cloud_init_status` — the error does not mention kubeadm.

**Nodes not built from the NKE OS image** — follow the steps below to install kubeadm and all required runtime dependencies from scratch.

> **Warning:** If you skip or partially complete the steps below, `nkectl bootstrap site` will reach `cluster state status=Provisioning` and then sit silently for the full `--timeout` (default 30 minutes) before failing. The log will say nothing useful. All of the steps below are required.

```bash
# Create keyrings directory
sudo mkdir -p -m 755 /etc/apt/keyrings

# Add Kubernetes repository (replace v1.34 with your target minor version)
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.34/deb/Release.key | \
  sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg

echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.34/deb/ /' | \
  sudo tee /etc/apt/sources.list.d/kubernetes.list

# Add Docker repository (for containerd.io).
# Skip this block if containerd is already present on your nodes
# (e.g. installed via DOCA or a pre-baked OS image) — use whatever
# version is already there and proceed to the configuration step below.
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | \
  sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg

echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
  https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | \
  sudo tee /etc/apt/sources.list.d/docker.list

sudo apt-get update
sudo apt-get install -y kubelet kubeadm kubectl containerd.io conntrack cri-tools
sudo apt-mark hold kubelet kubeadm kubectl
```

> **Warning:** The repository minor version (`v1.34` above) determines which `kubeadm` is installed, and it must match `cluster.k8sVersion` in your bootstrap configuration. `kubeadm` refuses to create a control plane more than one minor version older than itself.

### Configure the Container Runtime

containerd ships with the systemd cgroup driver disabled. kubeadm requires it. Without this step, the kubelet and containerd disagree about cgroup management and the control plane never becomes ready.

```bash
sudo containerd config default | sudo tee /etc/containerd/config.toml
sudo sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
sudo systemctl restart containerd
sudo systemctl enable containerd
```

### Configure Kernel Modules and Sysctls

kubeadm's preflight checks require `br_netfilter`, `overlay`, and three sysctls. Load them now and persist them across reboots.

```bash
sudo modprobe overlay
sudo modprobe br_netfilter

cat <<EOF | sudo tee /etc/modules-load.d/k8s.conf
overlay
br_netfilter
EOF

cat <<EOF | sudo tee /etc/sysctl.d/99-kubernetes.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF

sudo sysctl --system

# Disable swap — kubeadm requires it
sudo swapoff -a
sudo sed -i '/\bswap\b/s/^/#/' /etc/fstab
```

### Install CNI Plugin Binaries

`kubernetes-cni` (installed as a kubeadm dependency) does not populate `/opt/cni/bin` in the layout containerd expects. Install the containernetworking plugins explicitly:

```bash
CNI_VERSION="v1.6.0"
ARCH=$(dpkg --print-architecture)
sudo mkdir -p /opt/cni/bin
curl -fsSL "https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-linux-${ARCH}-${CNI_VERSION}.tgz" | \
  sudo tar -C /opt/cni/bin -xz
```

### Install Node-Agent

`nke-agent` installs from the NKE apt repository. The commands below are for Ubuntu 24.04 (Noble); on 26.04 (Resolute) use the `nke-apt-resolute` prefix and `nke-resolute` suite.

> **TODO:** Confirm public apt repo URL and GPG key location before publishing.

```bash
# Configure the NKE apt repository
sudo curl -fsSL -o /etc/apt/keyrings/nke.asc \
  https://packages.nvidia.com/nke/nke-apt-public.key

echo 'deb [signed-by=/etc/apt/keyrings/nke.asc] https://packages.nvidia.com/nke/apt nke-noble main' | \
  sudo tee /etc/apt/sources.list.d/nke-apt.list

sudo apt-get update
sudo apt-get install -y nke-agent

# Pre-provisioned nodes have no SPIRE workload API; disable mTLS (plaintext gRPC)
echo 'AGENT_ARGS="--disable-mtls"' | sudo tee /etc/default/nke-agent
sudo systemctl restart nke-agent

# Verify agent is running
sudo systemctl status nke-agent
```

> **Warning:** Use `nke-agent` **0.1.12 or later**. Older agents do not implement kubeadm preflight checks, so configuration problems only surface mid-bootstrap.

### Install Open vSwitch (OVN-Kubernetes only)

If `cluster.cniName` is `ovn-kubernetes`, install Open vSwitch on every node:

```bash
sudo apt-get install -y openvswitch-switch
```

## Step 2: Run Bootstrap on Init Node

On the **init node only**:

1. [Install `nkectl`](installation.md).
2. Generate your [site spec file](configuration.md). List your hosts in a CSV and let `nkectl` write the spec:

```bash
cat > hosts.csv <<'EOF'
name,ip,tpm_ek_thumbprint
cp-01,10.10.50.2,
cp-02,10.10.50.3,
cp-03,10.10.50.4,
EOF

nkectl generate sitespec my-tier0-site --nodes hosts.csv > sitespec.yaml
```

The result needs no editing. Fill in `tpm_ek_thumbprint` per host to enable TPM
attestation, and use `--set` for anything the CSV does not carry — for example
`--set cluster.podCidr=10.244.0.0/16`. See [configuration.md](configuration.md) for
every field.

3. Run bootstrap:

```bash
# Validate the site spec first (no nodes or clusters touched)
nkectl bootstrap site -f sitespec.yaml --dry-run

# Run bootstrap
REGISTRY_API_KEY="<your-registry-api-key>" \
NKECTL_CWE_IMAGE="nvcr.io/<org>/nke-controller:edge" \
NKECTL_CAPI_PROVISIONED_IMAGE="nvcr.io/<org>/cluster-api-provider-provisioned:edge" \
  nkectl bootstrap site -f sitespec.yaml
```

`nkectl` creates a temporary Kind cluster on the init node, deploys Postgres, Temporal, and CWE onto it, waits for CWE to be healthy, and then dispatches kubeadm init and join workflows to your nodes over `nke-agent` gRPC. The ephemeral Kind cluster is torn down automatically when bootstrap completes. No SSH required.

Use `--keep-bootstrap-cluster` to leave the Kind cluster running after bootstrap for debugging.

When bootstrap finishes it writes two files into the working directory: `<clusterName>.kubeconfig`, the cluster's admin credential, and `<clusterName>-etcd-encryption-key.json`, the key that encrypts the cluster's etcd data. Both are secrets. The cluster does not depend on the key file: every control-plane node holds the key at `/etc/kubernetes/kube-api-encryption/encryption-config.yaml`, and that is what kube-apiserver reads. The file is for two things the nodes cannot do. It is what a controller installed later is given, so nobody has to pull the key off a node by hand, and it is the copy that restores etcd onto new hosts when no control-plane node survives. **Move it off this host** — this host is one of the nodes it protects — and keep it with the cluster's other credentials. Use `--kubeconfig-out` and `--encryption-key-out` to write the files elsewhere.

**Duration**: Approximately **30–45 minutes**

> **Do NOT run bootstrap on join nodes** — they are joined automatically via gRPC from the init node.

## Step 3: Verify the Cluster

```bash
sudo kubectl get nodes --kubeconfig /etc/kubernetes/admin.conf
```

All nodes should show `Ready` status.

## What Happens Next

**Self-managed sites:**

1. Ensure your NICo site is set up and healthy before proceeding. See the [NICo documentation](https://docs.nvidia.com/infra-controller/documentation/home) for site setup and networking requirements.
2. Install your environment SBOM so NKE can drive day-2 operations through NICo.
3. Wait until the SBOM is fully deployed and healthy.
4. Complete NKE and NICo host ingestion with the NKE team. Keep the etcd encryption key file until this step succeeds: ingestion applies it to the controller, which needs it to join control-plane nodes and rotate the key. **TODO:** Publish NKE-specific ingestion runbook here once it exists.
5. Operate the site through NKE day-2 workflows.

**Unmanaged sites:** Deploy workloads with whatever tooling fits your environment. No NKE site-controller installation is required. Note that without NICo, no NKE day-2 operations (tenant cluster creation, node add/remove, upgrades) are available.

## Important Notes

- **Join nodes don't need `nkectl`** — only the init node runs it
- **`nke-agent` must be running on all nodes** — verify `systemctl status nke-agent` before starting bootstrap
- **Network connectivity** — CWE must reach each node on **15005** (nke-agent gRPC) and **6443** (API server); nodes with a TPM EK thumbprint also need **30081** reachable for attestation
- **Hostnames and IPs** — use actual hostnames and IPs that nodes use to communicate with each other
- **User permissions** — commands must run with `sudo` or as root

## Troubleshooting

**Node-agent not starting:**

```bash
sudo systemctl status nke-agent
sudo cat /var/log/nke-agent.log
sudo systemctl restart nke-agent
```

**Bootstrap refuses to start: "bootstrap cluster … from an earlier run is still running":**

The ephemeral Kind cluster from a previous bootstrap is still up — kept by `--keep-bootstrap-cluster`, or retained because that run failed after building the site cluster. In the second case it may hold the site cluster's etcd encryption key. Save the key before deleting it:

```bash
umask 077   # both files below are credentials; keep them owner-readable
export KIND_EXPERIMENTAL_PROVIDER=nerdctl
kind get kubeconfig --name nkectl-bootstrap > bootstrap.kubeconfig
# All namespaces: nke-controller, or cloud-workflow-engine if an older nkectl created the cluster.
kubectl --kubeconfig bootstrap.kubeconfig get secrets -A | grep etcd-encryption-key-
# save each that appears with `kubectl ... -n <namespace> get secret <name> -o json > <file>`, then:
kind delete cluster --name nkectl-bootstrap
```

Bootstrap does not delete or reuse the cluster itself.

**Bootstrap refuses to start: "port 6443, the Kubernetes API port, is accepting connections on …":**

Something on a site spec host accepts connections on the Kubernetes API port. The check is a TCP connect, so it cannot tell a control plane from anything else listening there, but a control plane left behind by an earlier attempt is by far the usual cause. Bootstrap does not provision over one: encryption is configured during `kubeadm init`, so a second run cannot pick up the key that host already holds. Confirm the node's kubeadm state, reset every node that has it, then re-run:

```bash
ls /etc/kubernetes/manifests   # kube-apiserver.yaml here means a control plane
sudo kubeadm reset -f
```

If nothing on the host is a Kubernetes control plane, check the node list in the site spec.

**Bootstrap failed with "its encryption key was not saved":**

The cluster is built and encrypted, and its kubeconfig is written; only the last step, saving the key to disk, failed. The bootstrap Kind cluster is kept because it holds the key. Do not delete it and do not re-run bootstrap. Run the command the error prints: it saves the Secret exactly as the bootstrap cluster holds it, owner-readable only, to the same path a successful run would have used. That file carries a `metadata.namespace` of `nke-controller`; remove that line before applying it anywhere else. Then delete the Kind cluster:

```bash
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nkectl-bootstrap
```

**A control-plane node added by hand never starts its API server:**

`kubeadm join --control-plane` times out with "waiting for the kubelet to boot up the control plane", and the kubelet log shows kube-apiserver failing on a missing `/etc/kubernetes/kube-api-encryption/encryption-config.yaml`. The cluster's kubeadm configuration points every API server at that file, and a node joined outside NKE has not been given it. Copy it from a working control-plane node before running `kubeadm join`:

```bash
sudo install -d -m 0755 /etc/kubernetes/kube-api-encryption
sudo scp <working-node>:/etc/kubernetes/kube-api-encryption/encryption-config.yaml /etc/kubernetes/kube-api-encryption/
sudo chmod 0600 /etc/kubernetes/kube-api-encryption/encryption-config.yaml
```

**Restoring etcd onto new hosts:**

The etcd data is encrypted, so a snapshot restored onto fresh control-plane nodes needs the key at `/etc/kubernetes/kube-api-encryption/encryption-config.yaml` on each of them before kube-apiserver starts. The value that goes after `secret:` in that file is the saved manifest's `data.active`, base64-decoded **once** — the manifest encodes the key a second time, as every Kubernetes Secret does:

```bash
jq -r .data.active <clusterName>-etcd-encryption-key.json | base64 -d
```

Without that file, and without a surviving control-plane node to copy from, the data cannot be read.

**Bootstrap stalled waiting for a node to join:**

Check that the init node can reach the join node on port 15005:

```bash
curl -v telnet://<join-node-ip>:15005
```

Check `nke-agent` on the join node and confirm `--disable-mtls` is set in `/etc/default/nke-agent`.
