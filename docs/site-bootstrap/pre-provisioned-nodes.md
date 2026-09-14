# Using Pre-Provisioned Nodes

If you already have nodes provisioned and running, you can manually prepare them for tier-0 bootstrap.

> **Tested platforms:** Tier-0 bootstrap on pre-provisioned nodes is tested on **Ubuntu 24.04 (Noble) and 26.04 (Resolute)** with **Kubernetes up to 1.34**. Kubernetes 1.35 and 1.36 are experimental. Ubuntu 22.04 (Jammy) is no longer supported.

## Self-Bootstrap Models: Managed vs Unmanaged

What you get after bootstrap depends on whether your environment includes a [NVIDIA Infra Controller (NICo)](https://github.com/NVIDIA/infra-controller) infrastructure provider for NKE, integrated via [`cluster-api-provider-nico`](https://github.com/dsx-ai-factory/cluster-api-provider-nico).

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
- `NKECTL_CWE_IMAGE` — full image reference for the CWE controller (e.g. `nvcr.io/<org>/controller:edge`)
- `NKECTL_CAPI_PROVISIONED_IMAGE` — full image reference for the provisioned Cluster API infrastructure provider (e.g. `nvcr.io/<org>/cluster-api-provider-provisioned:edge`)

If you are targeting a self-managed cluster, prepare your environment SBOM and [deploy repository](deploy-repository.md) ahead of time.

## Step 1: Prepare All Nodes

Run these commands on **all nodes** (both join and init nodes):

### Install Kubernetes Components

```bash
# Create keyrings directory
sudo mkdir -p -m 755 /etc/apt/keyrings

# Add Kubernetes repository (replace v1.34 with your target minor version)
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.34/deb/Release.key | \
  sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg

echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.34/deb/ /' | \
  sudo tee /etc/apt/sources.list.d/kubernetes.list

sudo apt-get update
sudo apt-get install -y kubelet kubeadm kubectl
sudo apt-mark hold kubelet kubeadm kubectl
```

> **Warning:** The repository minor version (`v1.34` above) determines which `kubeadm` is installed, and it must match `cluster.k8sVersion` in your bootstrap configuration. `kubeadm` refuses to create a control plane more than one minor version older than itself.

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
`--set cluster.cniName=calico`. See [configuration.md](configuration.md) for
every field.

3. Run bootstrap:

```bash
# Validate the site spec first (no nodes or clusters touched)
nkectl bootstrap site -f sitespec.yaml --dry-run

# Run bootstrap
REGISTRY_API_KEY="<your-registry-api-key>" \
NKECTL_CWE_IMAGE="nvcr.io/<org>/controller:edge" \
NKECTL_CAPI_PROVISIONED_IMAGE="nvcr.io/<org>/cluster-api-provider-provisioned:edge" \
  nkectl bootstrap site -f sitespec.yaml
```

`nkectl` creates a temporary Kind cluster on the init node, deploys Postgres, Temporal, and CWE onto it, waits for CWE to be healthy, and then dispatches kubeadm init and join workflows to your nodes over `nke-agent` gRPC. The ephemeral Kind cluster is torn down automatically when bootstrap completes. No SSH required.

Use `--keep-bootstrap-cluster` to leave the Kind cluster running after bootstrap for debugging.

**Duration**: Approximately **30–45 minutes**

> **Do NOT run bootstrap on join nodes** — they are joined automatically via gRPC from the init node.

## Step 3: Verify the Cluster

```bash
sudo kubectl get nodes --kubeconfig /etc/kubernetes/admin.conf
```

All nodes should show `Ready` status.

## What Happens Next

**Self-managed sites:**

1. Install your environment SBOM so NKE can drive day-2 operations through NICo.
2. Wait until the SBOM is fully deployed and healthy.
3. Complete NKE and NICo ingestion with the NKE team. Keep the ephemeral Kind cluster until this step succeeds. **TODO:** Publish ingestion runbook here once it exists.
4. Delete the Kind cluster, then operate the site through NKE day-2 workflows.

**Unmanaged sites:** Deploy workloads with whatever tooling fits your environment. No NKE site-controller installation is required.

## Important Notes

- **Join nodes don't need `nkectl`** — only the init node runs it
- **`nke-agent` must be running on all nodes** — verify `systemctl status nke-agent` before starting bootstrap
- **Network connectivity** — init node must reach join nodes on the configured gRPC port (default `15005`)
- **Hostnames and IPs** — use actual hostnames and IPs that nodes use to communicate with each other
- **User permissions** — commands must run with `sudo` or as root

## Troubleshooting

**Node-agent not starting:**

```bash
sudo systemctl status nke-agent
sudo cat /var/log/nke-agent.log
sudo systemctl restart nke-agent
```

**Bootstrap stalled waiting for a node to join:**

Check that the init node can reach the join node on port 15005:

```bash
curl -v telnet://<join-node-ip>:15005
```

Check `nke-agent` on the join node and confirm `--disable-mtls` is set in `/etc/default/nke-agent`.
