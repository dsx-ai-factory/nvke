# Run NVKE end to end on one machine

This page describes how to run the NVIDIA Kubernetes Engine (NVKE) operator flow on one machine: `nvkectl` bootstraps a one-node management cluster on a container that stands in for a management node, you install the management plane on that cluster, and you create a workload cluster on simulated NVIDIA Infra Controller (NICo) machines. One setup command stands in for the data center, the build and the lab's simulator; you then type the commands an operator types. Do it to evaluate NVKE or to rehearse an installation.

The setup has three parts on your machine:

- A container with a software TPM and the node agent, which `nvkectl bootstrap site` turns into the management cluster.
- A workspace, `~/nvke-local-site`, with what an operator is handed: the node's `hosts.csv` and SPIRE certificate authority (CA), the stack config naming the controller and profile images built from this checkout, the release set, a tenant request, and `env.sh` with the flow's environment and credentials.
- mock-NICo (the `nico-sim` process), which answers the NICo API and starts each workload cluster node as a Docker container.

The example credentials, the simulated NICo and the single-node site are for local evaluation only. Do not reuse this setup for a real installation. [Plan your installation](../operate/plan-installation.md) starts the real one; each step below links the page an operator follows there.

## Before you begin

- A Linux host, as root, with at least 10 CPUs, 24 GiB of free memory and 100 GiB of free disk; or macOS, set up in [On macOS](#on-macos).
- The Linux kernel module `tpm_vtpm_proxy`, which the container node's TPM uses. `make -C controller host-up` loads it.
- A standalone containerd with `nerdctl` and `buildkitd` for the node, which `cli/hack/bootstrap-e2e-host.sh install-runtime` installs with the version, checksums and socket paths exported as the two `export` lines in [On macOS](#on-macos) show, and Docker for the workload cluster nodes.
- These tools on `PATH`: `git`, GNU Make 4, Go 1.27.2 or later, `gcc`, `kind` v0.32.0, `kubectl`, Helm `v4.2.4`, `clusterctl` v1.13.6, `oras`, `curl`, `jq`, `python3`, `openssl` and `iptables`.
- A GitHub token in `GITHUB_TOKEN`: `clusterctl init` reads the Cluster API releases through the GitHub API, which allows anonymous callers 60 requests an hour, and this page runs it twice.
- Less than 70 percent of the disk in use. The node, the management plane and the workload cluster nodes share it, and the kubelet evicts pods above that.
- Outbound internet access to GitHub, the Go module proxy and public container registries and chart repositories.

### On macOS

Run everything inside a Colima virtual machine with the Docker runtime. `make -C controller host-up` creates it, loads `tpm_vtpm_proxy` in its kernel and sizes it from three variables; it refuses to resize a machine that exists, so set the sizes before the first run.

```bash
COLIMA_CPUS=10 COLIMA_MEMORY_GIB=28 COLIMA_DISK_GIB=100 make -C controller host-up
```

Then open a root shell in the machine, clone the repository inside it, and install the node's container runtime beside Colima's Docker. The installer reads the pinned version and checksums, and the runtime's socket paths, from the environment; the first line below takes the pins from the checkout:

```bash
colima ssh
sudo -i
git clone https://github.com/dsx-ai-factory/nvke.git nvke && cd nvke
export NERDCTL_VERSION="$(make --no-print-directory print-NERDCTL_VERSION)" NERDCTL_FULL_SHA256_AMD64="$(make --no-print-directory print-NERDCTL_FULL_SHA256_AMD64)" NERDCTL_FULL_SHA256_ARM64="$(make --no-print-directory print-NERDCTL_FULL_SHA256_ARM64)"
export CONTAINERD_ADDRESS=/run/containerd-nerdctl/containerd.sock BUILDKIT_HOST=unix:///run/buildkit-nerdctl/buildkitd.sock CONTAINERD_NAMESPACE=default
cli/hack/bootstrap-e2e-host.sh install-runtime
```

Install the other tools in the machine with its package manager and from each tool's release page. The rest of this page runs in that shell; `make local-site-setup` finds the runtime's sockets itself, and `env.sh` exports them for the flow.

## Set up the machine

1. Clone the repository and change into it, unless you did so in the machine.

   ```bash
   git clone https://github.com/dsx-ai-factory/nvke.git && cd nvke
   ```

2. Prepare the local site.

   ```bash
   make local-site-setup
   ```

   The command builds the bootstrap images, starts the container node and records its `hosts.csv` and CA, connects the networks the bootstrap's seed cluster and the node use, builds the controller and profile images from the checkout and stages them into the node, writes the workspace, and starts mock-NICo. The first run takes about ten minutes, most of it the node image; a rerun skips what is there. It ends with the first two commands of the flow and a list of what the workspace holds.

3. Load the workspace into your shell. Every command below runs from the workspace with its environment.

   ```bash
   source ~/nvke-local-site/env.sh
   ```

## Bootstrap the management cluster

1. Generate the site spec for the one node.

   ```bash
   nvkectl generate sitespec local-site --nodes hosts.csv --set cluster.podCidr=10.244.0.0/16 --set cluster.kubeletConfig.systemReservedMemory=512Mi --set spire.upstreamCA=true > sitespec.yaml
   ```

   One node and no virtual IP address: the node's own address is the API endpoint. The pod network moves off `192.168.0.0/16`, which the lab's DNS sits in; `512Mi` is the system reservation for a container node. Real hardware: [Bootstrap the management cluster](../operate/bootstrap-management-cluster.md).

2. Bootstrap the cluster.

   ```bash
   nvkectl bootstrap site -f sitespec.yaml --timeout 25m
   ```

   The command takes about seven minutes and writes `local-site.kubeconfig` and `local-site-etcd-encryption-key.json` into the workspace.

## Install the management plane

1. Install Cluster API at the version NVKE pins.

   ```bash
   clusterctl init --kubeconfig local-site.kubeconfig --core cluster-api:v1.13.6 --bootstrap kubeadm:v1.13.6 --control-plane kubeadm:v1.13.6 --wait-providers
   ```

2. Install the stack.

   ```bash
   "$NVKE_REPO"/deploy/nvke-deploy-local.sh install
   ```

   `NVKE_REPO` is the checkout, exported by `env.sh`. The command installs the lab StorageClass, runs the install a site runs, `deploy/nvke-deploy.sh install` on `deploy/nico` with the site spec and the stack config from the workspace, and patches the Cluster API controllers with the SPIRE exec-auth plugin. It takes about ten minutes.

3. Admit the lab's networks to the NVKE API and point `nvkectl` at it.

   ```bash
   kubectl --kubeconfig local-site.kubeconfig apply -f nvke-api-clients.yaml
   ```

   ```bash
   nvkectl config set-site --name local-site --url http://172.26.0.10:30080 --kubeconfig "$PWD/local-site.kubeconfig"
   ```

   `172.26.0.10` is the node's address, the one `hosts.csv` lists.

4. Publish the release set the workload cluster nodes boot from.

   ```bash
   nvkectl create machineimage -f releases/machineimage.yaml
   ```

   ```bash
   nvkectl create clusterrelease -f releases/clusterrelease.yaml
   ```

   ```bash
   nvkectl create nodepoolrelease -f releases/nodepoolrelease.yaml
   ```

   The set is for your machine's architecture and boots mock-NICo's container nodes. Real hardware: [Write the release manifests](../operate/build-node-images.md#write-the-release-manifests).

## Create a workload cluster

1. Create the cluster.

   ```bash
   nvkectl create cluster -f tenant.yaml
   ```

   The request asks for one control-plane node and one worker on mock-NICo's seeded site. Real hardware: [Create a workload cluster](../workload-clusters/create.md), with your NICo identifiers.

2. Wait for it.

   ```bash
   nvkectl get clusters
   ```

   The status moves from `Provisioning` to `Provisioned`, in about eight minutes. `docker ps` shows the two node containers as they come up.

## Access the workload cluster

```bash
nvkectl get kubeconfig tenant-1 --output-file tenant-1.kubeconfig
```

```bash
kubectl --kubeconfig tenant-1.kubeconfig get nodes
```

Two nodes are listed, one with the `control-plane` role, both `Ready`. [Access a workload cluster](../workload-clusters/access.md) describes the kubeconfig and how a customer uses it.

## Clean up

```bash
make -C "$NVKE_REPO" local-site-down
```

The command deletes the workload clusters through the NVKE API, stops mock-NICo and removes its node containers, undoes the network connections the setup made, removes the node, and deletes the workspace. The container images that the setup built stay in containerd and Docker. On macOS, `colima stop` keeps the machine for the next run and `colima delete` removes it.

## Next steps

- [Create a workload cluster](../workload-clusters/create.md) with your own request.
- [Observe NVKE and its clusters](../operate/observe.md).
- [Plan your installation](../operate/plan-installation.md) for real hardware.
