# Using Pre-Provisioned Nodes

> **Which path is this?** This guide covers bootstrap on nodes you bring yourself — no SPIRE workload API, no TPM, `--disable-mtls` on `nvke-agent`. If your nodes are built from the NVKE OS image (SPIRE agent pre-installed, mTLS on by default), see [`os-image/SPEC.md`](../../os-image/SPEC.md) for the image contract and [`docs/first-cluster.md`](../first-cluster.md#real-site), the Real Site path, for the full flow.

If you already have nodes provisioned and running, you can manually prepare them for tier-0 bootstrap.

> **Tested platforms:** Tier-0 bootstrap on pre-provisioned nodes is tested on **Ubuntu 24.04 (Noble) and 26.04 (Resolute)** with **Kubernetes up to 1.34**. Kubernetes 1.35 and 1.36 are experimental. Ubuntu 22.04 (Jammy) is no longer supported.

## Self-Bootstrap Models: Managed vs Unmanaged

What you get after bootstrap depends on whether your environment includes a [NVIDIA Infra Controller (NICo)](https://github.com/dsx-ai-factory/infra-controller) site, integrated with NVKE via [`cluster-api-provider-nico`](https://github.com/dsx-ai-factory/cluster-api-provider-nico).

> **Note:** During bootstrap, NVKE uses `cluster-api-provider-provisioned` as a temporary CAPI provider to bootstrap the site controller cluster from pre-provisioned hosts. For production day-2 operations (tenant cluster creation, node add/remove, rolling upgrades), NICo is the supported infrastructure provider, integrated through CAPNICo. The self-managed path is the supported production deployment model. The unmanaged path produces a working Kubernetes cluster but no NVKE day-2 operations are possible — it is not a supported NVKE deployment.

| | Self-bootstrapped, **self-managed** | Self-bootstrapped, **unmanaged** |
|---|---|---|
| **Environment** | NICo infrastructure is available or will be deployed | No NICo infrastructure, and none is planned |
| **Your responsibility** | Bring an SBOM that includes NVKE and its dependencies (NICo, Temporal, Postgres, SPIRE, and related components) | Bring only what you need to run workloads on Kubernetes |
| **After bootstrap** | Install your SBOM on the new cluster so NVKE can drive the site through NICo | Use the Kubernetes cluster as-is |
| **Day-2 operations** | Supported — upgrade clusters, add/remove nodes, create tenant clusters | **Not supported** — NVKE has no infrastructure provider to perform these operations |

**Example (self-managed):** An operator self-bootstraps a site controller cluster, then installs the NVKE SBOM (NVKE, NICo, Temporal, Postgres, SPIRE). The cluster becomes self-managing and supports day-2 operations.

**Example (unmanaged):** A team bootstraps Kubernetes on nodes in an environment with no NICo. They get a working cluster but should not expect NVKE day-2 features.

## What `nvkectl bootstrap site` Does

`nvkectl bootstrap site` has one job on pre-provisioned nodes: turn your prepared nodes into a **Kubernetes cluster**. It does not install your site stack, GitOps controller, or application workloads.

After bootstrap completes:

1. **Verify the cluster** is healthy (see [Step 3](#step-3-verify-the-cluster)).
2. **If self-managed**, install your environment SBOM (NVKE, NICo, Temporal, Postgres, SPIRE, and the rest).
3. **Deploy your GitOps tooling** — ArgoCD, Flux, or whatever you use for an app-of-apps layer.

## Prerequisites

- An odd number of Ubuntu 24.04 or 26.04 nodes — one, three, five; an even count is rejected
- SSH access to all nodes with sudo privileges
- Nodes can reach the internet for downloading packages and artifacts
- Network connectivity between all nodes
- Each node has an available NVMe device for local storage (non-root, unpartitioned, not in use)
- NGC API key (required for pulling NVKE container images)
- On the **init node**: `kind` binary and `nerdctl` with containerd installed — `nvkectl` uses these to create the ephemeral bootstrap Kind cluster. Docker is not supported. kind states 1.7.0 as the minimum for its nerdctl provider; 2.3.5 is the version CI exercises
- The two images `nvkectl` side-loads: `make -C cli images` builds them on the init node. To use images from a registry instead, pass `--nvke-controller-image` and `--provisioned-provider-image`, or set `NVKECTL_NVKE_CONTROLLER_IMAGE` (e.g. `nvcr.io/<org>/nvke-controller:edge`) and `NVKECTL_CAPI_PROVISIONED_IMAGE` (e.g. `nvcr.io/<org>/cluster-api-provider-provisioned:edge`)
- On the **init node**, also on `PATH`: `helm`, `kubectl`, and `clusterctl`. `nvkectl`
  shells out to all three during bootstrap — Helm for the Postgres, Temporal, SPIRE
  and NVKE controller releases, `kubectl` for the manifests, and `clusterctl init` for the
  Cluster API providers. Only nerdctl is checked before the run starts; a
  missing `kind` fails in the pre-flight check for a leftover bootstrap cluster,
  before anything is created; a missing `helm`, `kubectl` or `clusterctl`
  surfaces mid-bootstrap, after the Kind cluster is already up
- The init node must reach `go.temporal.io` and `spiffe.github.io` (the Temporal
  and SPIRE charts), `github.com` (`clusterctl init` and cert-manager), and the
  registries the bootstrap images come from — Docker Hub, `ghcr.io`,
  `registry.k8s.io` and `cgr.dev` — in addition to your own image registry.
  `nvkectl get bootstrap-manifest` lists those charts and images. It leaves out
  what `clusterctl init` installs: Cluster API and cert-manager come from
  `github.com` and their images from their own registries

If you are targeting a self-managed cluster, prepare your environment SBOM and [deploy repository](deploy-repository.md) ahead of time.

## Step 1: Prepare All Nodes

Run these commands on **all nodes** (both join and init nodes):

### Install Kubernetes Components

How you install kubeadm depends on whether your nodes were built from an NVKE OS image.

**Nodes built from the NVKE OS image** — the image stages versioned Kubernetes Debian packages in a local apt repository at `/usr/lib/nvke/debs`, but does not install them. Install them via cloud-init `runcmd` before the node is handed to bootstrap:

```yaml
runcmd:
  - DEBIAN_FRONTEND=noninteractive apt-get install -y nvke-kubeadm-distro-<k8s-version>
  - DEBIAN_FRONTEND=noninteractive apt-get install -y nvke-cni-ovn-images-<stream> nvke-cni-ovn-charts-<stream>
```

Replace `<k8s-version>` with the version matching `cluster.k8sVersion` in your site spec, and `<stream>` with the CNI stream for your Kubernetes version. If you skip this step, bootstrap runs its full 30-minute timeout and fails on `NodePreflight` / `check_cloud_init_status` — the error does not mention kubeadm.

**Nodes not built from the NVKE OS image** — follow the steps below to install kubeadm and all required runtime dependencies from scratch.

> **Warning:** If you skip or partially complete the steps below, `nvkectl bootstrap site` will reach `cluster state status=Provisioning` and then sit silently for the full `--timeout` (default 30 minutes) before failing. The log will say nothing useful. All of the steps below are required.

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

`nvke-agent` will install from the NVKE apt repository. The commands below are for Ubuntu 24.04 (Noble); on 26.04 (Resolute) use the `nvke-apt-resolute` prefix and `nvke-resolute` suite.

> **Not published yet — the commands below do not work.**
> `packages.nvidia.com/nvke` serves neither the key nor the suite: the `curl`
> returns HTTP 404 (`NoSuchKey`), so `apt-get install nvke-agent` fails after it.
> Until the repository exists there is no apt install. To prepare a node in
> the meantime, build the agent from this repository: `make -C agent package`
> cross-compiles it to `agent/bin/linux_<arch>/nvke-agent` and bundles it with
> `agent/scripts/agent-installer.sh`, which lays down the binary, the systemd
> unit and `/etc/default/nvke-agent`.
> [`controller/hack/capd-kindest-node/`](https://github.com/dsx-ai-factory/nvke/tree/main/controller/hack/capd-kindest-node)
> installs the agent that way for the bootstrap e2e's nodes and is the closest
> worked example.
> **Known limitation at preview:** the apt repository and its signing key are
> not published. From `agent/v0.3.0-rc.2`, each `agent/` release at
> https://github.com/dsx-ai-factory/nvke/releases also attaches
> `nvke-agent_<version>_<arch>.deb`, which `apt-get install` can install from a
> local path.

```bash
# Configure the NVKE apt repository
sudo curl -fsSL -o /etc/apt/keyrings/nke.asc \
  https://packages.nvidia.com/nvke/nvke-apt-public.key

echo 'deb [signed-by=/etc/apt/keyrings/nke.asc] https://packages.nvidia.com/nvke/apt nvke-noble main' | \
  sudo tee /etc/apt/sources.list.d/nke-apt.list

sudo apt-get update
sudo apt-get install -y nvke-agent

# Nodes prepared this way run no spire-agent, so the agent speaks plaintext gRPC.
# nvkectl bootstrap site must then be run with --disable-mtls too (Step 2).
echo 'AGENT_ARGS="--disable-mtls"' | sudo tee /etc/default/nvke-agent
sudo systemctl restart nvke-agent

# Verify agent is running
sudo systemctl status nvke-agent
```

> **Warning:** Use `nvke-agent` **0.1.12 or later**. Older agents do not implement kubeadm preflight checks, so configuration problems only surface mid-bootstrap.

> **Plaintext mode.** `--disable-mtls` on the agent turns SPIFFE off, so
> `nvkectl bootstrap site` has to be run with `--disable-mtls` as well — the controller
> otherwise dials the agents over mTLS and never connects. `nvkectl` labels this
> mode development only. For the attested path — `spire-agent` on every node, a
> TPM EK thumbprint per host in the CSV, and a SPIRE CA from
> `NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE` — follow
> [Your First Cluster, Real Site](../first-cluster.md#real-site).

### Install Open vSwitch (OVN-Kubernetes only)

If `cluster.cniName` is `ovn-kubernetes`, install Open vSwitch on every node:

```bash
sudo apt-get install -y openvswitch-switch
```

## Step 2: Run Bootstrap on Init Node

On the **init node only**:

1. [Install `nvkectl`](installation.md).
2. Generate your [site spec file](configuration.md). List your hosts in a CSV and let `nvkectl` write the spec:

```bash
cat > hosts.csv <<'EOF'
name,ip,tpm_ek_thumbprint
cp-01,10.10.50.2,
cp-02,10.10.50.3,
cp-03,10.10.50.4,
EOF

nvkectl generate sitespec my-tier0-site --nodes hosts.csv > sitespec.yaml
```

The result needs no editing. Leave `tpm_ek_thumbprint` empty for the plaintext
mode this guide uses; `--disable-mtls` ignores it. Use `--set` for anything the
CSV does not carry — for example `--set cluster.podCidr=10.244.0.0/16`. See
[configuration.md](configuration.md) for every field, and check the template's
`systemReservedMemory` default of `16Gi` against your hardware: kubelet subtracts
it from capacity, so on a smaller node nothing is allocatable and the node never
reaches `Ready`.

3. Run bootstrap:

```bash
# Validate the site spec first (no nodes or clusters touched)
nvkectl bootstrap site -f sitespec.yaml --dry-run

# Run bootstrap. --disable-mtls matches the plaintext agents from Step 1.
NVKECTL_NVKE_CONTROLLER_IMAGE="nvcr.io/<org>/nvke-controller:edge" \
NVKECTL_CAPI_PROVISIONED_IMAGE="nvcr.io/<org>/cluster-api-provider-provisioned:edge" \
  nvkectl bootstrap site -f sitespec.yaml --disable-mtls
```

The two images are loaded into the Kind cluster from containerd's image store on
the init node, never pulled from inside the cluster. An image with a registry host
is pulled with `nerdctl pull` first when it is not already in the store, so log in
to your registry on the init node (`nerdctl login nvcr.io`) before running. The
local build from `make -C cli images` needs no login.

`nvkectl` creates a temporary Kind cluster on the init node, deploys Postgres,
Temporal and the Cluster API providers onto it — and SPIRE, unless `--disable-mtls`
is set — then the controller, waits for it to be healthy, and dispatches the kubeadm init and
join work to your nodes through Cluster API and `nvke-agent` gRPC. No SSH required.

The ephemeral Kind cluster is torn down when bootstrap succeeds. It is **kept** if you
pass `--keep-bootstrap-cluster`, or if the run fails after the site cluster was
created — in that case `nvkectl` prints the `kind delete cluster` command to run once
you are done with it.

Useful flags:

| Flag | Default | What it does |
|---|---|---|
| `--timeout` | `30m` | How long to wait for the cluster to finish provisioning. A three-node site usually finishes in 10–15 minutes; raise it for larger sites. |
| `--diagnostics-dir` | `cluster-diagnostics` | Where a failed run writes cluster diagnostics before tearing down. |
| `--keep-bootstrap-cluster` | off | Leave the ephemeral Kind cluster running for debugging. |
| `--kubeconfig-out` | `<clusterName>.kubeconfig` | Where to write the finished cluster's breakglass admin kubeconfig. |
| `--encryption-key-out` | `<clusterName>-etcd-encryption-key.json` | Where to write the cluster's etcd encryption key, as a Secret manifest. |
| `--disable-mtls` | off | Stand the site up with no SPIRE, the controller dialling agents in plaintext. **Development only.** This guide uses it because the agents in Step 1 run with `--disable-mtls`; the two sides must agree. Drop it only when every node runs `spire-agent` and has a thumbprint in the CSV. |

When bootstrap finishes it writes two files into the working directory: `<clusterName>.kubeconfig`, a breakglass admin kubeconfig, and `<clusterName>-etcd-encryption-key.json`, the key that encrypts the cluster's etcd data. Both are secrets. The cluster does not depend on the key file: every control-plane node holds the key at `/etc/kubernetes/kube-api-encryption/encryption-config.yaml`, and that is what kube-apiserver reads. The file is for two things the nodes cannot do. It is what a controller installed later is given, so nobody has to pull the key off a node by hand, and it is the copy that restores etcd onto new hosts when no control-plane node survives. **Move it off this host** — this host is one of the nodes it protects — and keep it with the cluster's other credentials. Use `--kubeconfig-out` and `--encryption-key-out` to write the files elsewhere.

**Duration**: Typically **10–15 minutes** for a three-node site

> **Do NOT run bootstrap on join nodes** — they are joined automatically via gRPC from the init node.

## Step 3: Verify the Cluster

On success `nvkectl` writes the breakglass admin kubeconfig to
`my-tier0-site.kubeconfig` in the working directory (`--kubeconfig-out` changes the
path) and prints the command to use it:

```bash
kubectl --kubeconfig ./my-tier0-site.kubeconfig get nodes
```

All nodes should show `Ready` status.

## What Happens Next

**Self-managed sites:**

1. Ensure your NICo site is set up and healthy before proceeding. See the [NICo documentation](https://docs.nvidia.com/infra-controller/documentation/home) for site setup and networking requirements.
2. Install your environment SBOM so NVKE can drive day-2 operations through NICo.
3. Wait until the SBOM is fully deployed and healthy.
4. Complete NVKE and NICo host ingestion with the NVKE team. Keep the etcd encryption key file until this step succeeds: ingestion applies it to the controller, which needs it to join control-plane nodes and rotate the key. Known limitation at preview: no ingestion runbook is published.
5. Operate the site through NVKE day-2 workflows.

**Unmanaged sites:** Deploy workloads with whatever tooling fits your environment. No NVKE site-controller installation is required. Note that without NICo, no NVKE day-2 operations (tenant cluster creation, node add/remove, upgrades) are available.

## Important Notes

- **Join nodes don't need `nvkectl`** — only the init node runs it
- **`nvke-agent` must be running on all nodes** — verify `systemctl status nvke-agent` before starting bootstrap
- **Network connectivity** — the controller must reach each node on **15005** (nvke-agent gRPC) and **6443** (API server); nodes with a TPM EK thumbprint must reach the init node on **30081** (SPIRE) for attestation
- **Hostnames and IPs** — use actual hostnames and IPs that nodes use to communicate with each other
- **User permissions** — commands must run with `sudo` or as root

## Troubleshooting

For failures of the `nvkectl bootstrap site` command itself, where its logs and
diagnostics are, and when it is safe to re-run, see
[Troubleshooting a Site Bootstrap](../../cli/docs/bootstrap-troubleshooting.md).
The entries below are node-side.

**Node-agent not starting:**

```bash
sudo systemctl status nvke-agent
sudo cat /var/log/nvke-agent.log
sudo systemctl restart nvke-agent
```

**Bootstrap refuses to start: "bootstrap cluster … from an earlier run is still running":**

The ephemeral Kind cluster from a previous bootstrap is still up — kept by `--keep-bootstrap-cluster`, or retained because that run failed after building the site cluster. In the second case it may hold the site cluster's etcd encryption key. Save the key before deleting it:

```bash
umask 077   # both files below are credentials; keep them owner-readable
export KIND_EXPERIMENTAL_PROVIDER=nerdctl
kind get kubeconfig --name nvkectl-bootstrap > bootstrap.kubeconfig
# All namespaces: nvke-controller, or cloud-workflow-engine if an older nvkectl created the cluster.
kubectl --kubeconfig bootstrap.kubeconfig get secrets -A | grep etcd-encryption-key-
# save each that appears with `kubectl ... -n <namespace> get secret <name> -o json > <file>`, then:
kind delete cluster --name nvkectl-bootstrap
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

The cluster is built and encrypted, and its kubeconfig is written; only the last step, saving the key to disk, failed. The bootstrap Kind cluster is kept because it holds the key. Do not delete it and do not re-run bootstrap. Run the command the error prints: it saves the Secret exactly as the bootstrap cluster holds it, owner-readable only, to the same path a successful run would have used. That file carries a `metadata.namespace` of `nvke-controller`; remove that line before applying it anywhere else. Then delete the Kind cluster:

```bash
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nvkectl-bootstrap
```

**A control-plane node added by hand never starts its API server:**

`kubeadm join --control-plane` times out with "waiting for the kubelet to boot up the control plane", and the kubelet log shows kube-apiserver failing on a missing `/etc/kubernetes/kube-api-encryption/encryption-config.yaml`. The cluster's kubeadm configuration points every API server at that file, and a node joined outside NVKE has not been given it. Copy it from a working control-plane node before running `kubeadm join`:

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

Check `nvke-agent` on the join node and confirm `--disable-mtls` is set in `/etc/default/nvke-agent` — and that `nvkectl bootstrap site` was run with `--disable-mtls` too. One side in plaintext and the other in mTLS never connects.
