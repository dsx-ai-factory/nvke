# Run NVKE end to end on one machine

This page describes how to run the NVIDIA Kubernetes Engine (NVKE) operator flow on one machine: `nvkectl` bootstraps a one-node management cluster on a container that stands in for a management node, you install the management plane on that cluster, and you create a workload cluster on simulated NVIDIA Infra Controller (NICo) machines. One setup command stands in for the data center, the build and the lab's simulator; you then type the commands an operator types. Do it to evaluate NVKE or to rehearse an installation.

The setup has three parts on your machine:

- A container with a software TPM and the node agent, which `nvkectl bootstrap site` turns into the management cluster.
- A workspace, `~/nvke-local-site`, with what an operator is handed: the node's `hosts.csv` and SPIRE certificate authority (CA), the management plane's images and charts built from this checkout, the stack config, the release set, a tenant request, and `env.sh` with the credentials.
- mock-NICo (the `nico-sim` process), which answers the NICo API and starts each workload cluster node as a Docker container.

The example credentials, the simulated NICo and the single-node site are for local evaluation only. Do not reuse this setup for a real installation. [Plan your installation](../operate/plan-installation.md) starts the real one; each step below links the page an operator follows there.

## Before you begin

**Validation status:** the October 18, 2026 ARM64 Colima test of revision `bdbe146745702dcb76422cee52b05a88e562935f` completed runtime installation and `make local-site-setup`, but management-cluster bootstrap was blocked by local network connectivity. The management-plane install and workload-cluster steps below remain unverified in that run. See [Runtime test evidence](#runtime-test-evidence) before following this draft walkthrough.

- A Linux host, as root, with at least 10 CPUs, 24 GiB of free memory and 100 GiB of free disk; or macOS, set up in [On macOS](#on-macos).
- The Linux kernel module `tpm_vtpm_proxy`, which the container node's TPM uses. `make -C controller host-up` loads it.
- A standalone containerd with `nerdctl` and `buildkitd` for the node, which `cli/hack/bootstrap-e2e-host.sh install-runtime` installs, and Docker for the workload cluster nodes.
- These tools on `PATH`: `git`, GNU Make 4, Go 1.27.2 or later, `gcc`, `kind` v0.32.0, `kubectl`, Helm `v4.2.4`, `clusterctl` v1.13.6, `oras`, `curl`, `jq`, `python3` and `openssl`.
- A GitHub token in `GITHUB_TOKEN`: `clusterctl init` reads the Cluster API releases through the GitHub API, which allows anonymous callers 60 requests an hour, and this page runs it twice.
- Less than 70 percent of the disk in use. The node, the management plane and the workload cluster nodes share it, and the kubelet evicts pods above that.
- Outbound internet access to GitHub, the Go module proxy and public container registries and chart repositories.

### On macOS

Run everything inside a Colima virtual machine with the Docker runtime. `make -C controller host-up` creates it, loads `tpm_vtpm_proxy` in its kernel and sizes it from three variables; it refuses to resize a machine that exists, so set the sizes before the first run.

```bash
COLIMA_CPUS=10 COLIMA_MEMORY_GIB=28 COLIMA_DISK_GIB=100 make -C controller host-up
```

Then open a root shell in the machine and clone the repository inside it:

```bash
colima ssh
sudo -i
git clone https://github.com/dsx-ai-factory/nvke.git nvke && cd nvke
```

Install the prerequisite tools in the machine with its package manager and from each tool's release page. The runtime installation below supplies `nerdctl`, containerd and BuildKit. The rest of this page runs in that root shell.

## Set up the machine

1. Clone the repository and change into it, unless you did so in the machine.

   ```bash
   git clone https://github.com/dsx-ai-factory/nvke.git && cd nvke
   ```

2. Install the standalone container runtime, in the root shell. Read its version and checksums from the checkout and select its sockets before running the installer.

   ```bash
   export NERDCTL_VERSION="$(make --no-print-directory print-NERDCTL_VERSION)"
   export NERDCTL_FULL_SHA256_AMD64="$(make --no-print-directory print-NERDCTL_FULL_SHA256_AMD64)"
   export NERDCTL_FULL_SHA256_ARM64="$(make --no-print-directory print-NERDCTL_FULL_SHA256_ARM64)"
   export CONTAINERD_ADDRESS=/run/containerd-nerdctl/containerd.sock
   export BUILDKIT_HOST=unix:///run/buildkit-nerdctl/buildkitd.sock
   export CONTAINERD_NAMESPACE=default
   ```

   ```bash
   cli/hack/bootstrap-e2e-host.sh install-runtime
   ```

   The installer verifies the archive for your architecture and starts a separate containerd and BuildKit, alongside Docker. Keep these exports in the shell used for setup; a new root shell does not inherit them.

   ```bash
   nerdctl info
   ```

   The command reports the standalone containerd server. If installation reports `NERDCTL_VERSION: parameter null or not set`, run the exports above in the same shell before retrying.

3. Prepare the local site.

   ```bash
   make local-site-setup
   ```

   The command builds the bootstrap images, starts the container node and records its `hosts.csv` and CA, builds the management plane's images and charts from the checkout, writes the workspace, and starts mock-NICo. The first run takes about ten minutes, most of it the node image; a rerun skips what is there. It ends with the first two commands of the flow and a list of what the workspace holds.

4. Load the workspace into your shell. Every command below runs from the workspace with its environment.

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
   ../nvke/deploy/nvke-deploy-local.sh install
   ```

   The command installs the lab StorageClass, runs the install a site runs, `deploy/nvke-deploy.sh install` on `deploy/nico` with the site spec, the stack config and the artifacts from the workspace, and patches the Cluster API controllers with the SPIRE exec-auth plugin. It takes about ten minutes and ends with the path of the install record. Real hardware: [Install management-plane services](../operate/install-management-plane-services.md), where the storage is the site's and each input is yours.

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

   The set is for your machine's architecture and boots mock-NICo's container nodes. Real hardware: [Write release manifests](../operate/write-release-manifests.md).

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
make -C ../nvke local-site-down
```

The command deletes the workload clusters through the NVKE API, stops mock-NICo and removes its node containers, removes the node, and deletes the workspace. The container images that the setup built stay in containerd and Docker. On macOS, `colima stop` keeps the machine for the next run and `colima delete` removes it.

## Next steps

- [Create a workload cluster](../workload-clusters/create.md) with your own request.
- [Observe NVKE and its clusters](../operate/observe.md).
- [Plan your installation](../operate/plan-installation.md) for real hardware.

## Runtime test evidence

The October 18, 2026 test used an isolated Colima 0.10.3 / Lima 2.2.1 machine with the Docker runtime, Apple Virtualization, Ubuntu 24.04.4 ARM64, 10 CPUs, 28 GiB RAM, and separate 150 GiB root and data disks. The source revision was `bdbe146745702dcb76422cee52b05a88e562935f`, the tested head of [#1725](https://github.com/dsx-ai-factory/nvke/pull/1725). No executable source was changed.

The machine was created separately with these resources; this run did not validate the macOS `host-up` creation command or the minimum disk size above. Tools were installed at the checkout's pinned versions. No `GITHUB_TOKEN` was supplied; bootstrap's Cluster API installation succeeded without one in this run.

| Step | Observed result |
| --- | --- |
| Original runtime-install command, without exports | Failed with `NERDCTL_VERSION: parameter null or not set`. |
| Runtime install with the exports documented above | Passed archive checksum verification; containerd and BuildKit started; `nerdctl info` succeeded alongside Docker. |
| `make local-site-setup` | Passed: built the images and stack artifacts, started the management-node container and simulator, and wrote the workspace. |
| Site-spec generation | Passed. |
| `nvkectl bootstrap site -f sitespec.yaml --timeout 25m` | Created the seed cluster and reported a healthy controller, but management-cluster provisioning stalled. The run was interrupted after diagnosing the network failure; no `local-site.kubeconfig` was produced. |
| Management-plane installation and workload-cluster creation | Not run: require the management cluster's kubeconfig. |

The controller's `NodePreflight` retries reported `dial tcp 172.26.0.10:15005: i/o timeout`. The management node's SPIRE agent reported `dial tcp 172.26.0.2:30081: connect: no route to host`. Its service was running, but it had not attested and the node agent was not listening on port 15005.

The seed cluster used nerdctl's `kind` bridge. Setup's `bridge_networks` function in `hack/local-site.sh` connected the management-node bridge to Docker's `kind` bridge instead; the SPIRE address was also absent from the management-node bridge. The separate bootstrap test in `cli/e2e/bootstrap/run.sh` supplies those missing steps through `attach_bootstrap_to_node_network`. That test's network preparation is not invoked by this walkthrough. Resolve this prerequisite and repeat bootstrap before treating the remaining steps as validated.
