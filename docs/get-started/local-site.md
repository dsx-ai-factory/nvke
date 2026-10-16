# Run NVKE end to end on one machine

This page describes how to run the NVIDIA Kubernetes Engine (NVKE) operator flow on one machine: bootstrap a one-node management cluster on a container node with `nvkectl`, install the management plane from artifacts built from the checkout, and create a workload cluster on simulated NICo machines. Each step links the page an operator follows on real hardware and names what the lab substitutes. Do it to evaluate NVKE or to rehearse an installation.

The machine runs three things: a container that stands in for a management node, with a software TPM and the node agent, from the bootstrap end-to-end substrate; the management plane on the cluster `nvkectl` forms there; and `nico-sim`, a simulator that answers the NVIDIA Infra Controller (NICo) API and starts each workload cluster node as a container. `deploy/examples/local-site/` holds the inputs this page uses.

## Before you begin

- A Linux host, as root, with at least 10 CPUs, 24 GiB of free memory and 100 GiB of free disk, or macOS with Colima, set up in [On macOS](#on-macos) below.
- `/dev/vtpmx` present: the `tpm_vtpm_proxy` kernel module, which the container node's software TPM uses.
- These tools on `PATH`: `git`, GNU Make 4, Go 1.27.2 or later, `gcc`, `nerdctl` with a containerd and a buildkitd it reaches, `kind` v0.32.0, `kubectl`, Helm `v4.2.4`, `clusterctl` v1.13.6, `oras`, `curl`, `jq`, `python3` and Docker, which `nico-sim` uses to run the workload cluster nodes. `cli/hack/bootstrap-e2e-host.sh install-runtime` installs the pinned `nerdctl`, containerd and buildkitd the way the bootstrap end-to-end lane does, and prints the `CONTAINERD_ADDRESS`, `BUILDKIT_HOST` and `CONTAINERD_NAMESPACE` values to export.
- A GitHub token in `GITHUB_TOKEN`. `clusterctl init` reads the Cluster API releases through the GitHub API, which allows anonymous callers 60 requests an hour; this page runs it twice.
- A checkout of `dsx-ai-factory/nvke` in `nvke/`, on the machine the commands run on. Run every command on this page from its root.
- Less than 70 percent of the disk in use: the node container, the management plane and the workload cluster nodes share it, and the kubelet evicts pods above that.

### On macOS

Run everything inside a Colima virtual machine with the Docker runtime. `make -C controller host-up` creates it, loads `tpm_vtpm_proxy` in its kernel and sizes it from `COLIMA_CPUS`, `COLIMA_MEMORY_GIB` and `COLIMA_DISK_GIB`; it refuses to resize a machine that exists, so set the sizes before the first run.

```bash
COLIMA_CPUS=10 COLIMA_MEMORY_GIB=28 COLIMA_DISK_GIB=100 make -C controller host-up
```

Then open a root shell in the machine, clone the repository inside it, and install the standalone container runtime beside Colima's Docker:

```bash
colima ssh
sudo -i
git clone https://github.com/dsx-ai-factory/nvke.git nvke && cd nvke
cli/hack/bootstrap-e2e-host.sh install-runtime
export CONTAINERD_ADDRESS=/run/containerd-nerdctl/containerd.sock BUILDKIT_HOST=unix:///run/buildkit-nerdctl/buildkitd.sock CONTAINERD_NAMESPACE=default
```

The runtime comes with `nerdctl`; install the rest of the tools with the machine's package manager and from each tool's release page. `nico-sim` keeps using Colima's Docker. The management cluster's architecture is the machine's: `arm64` on Apple silicon.

## Bootstrap the management cluster

1. Build the images the bootstrap side-loads into its seed cluster.

   ```bash
   make -C cli images
   ```

   Real hardware: [Bootstrap the management cluster](../operate/bootstrap-management-cluster.md), *Choose the NVKE images*.

2. Start the container node and write its host file.

   ```bash
   export NVKECTL_E2E_NODES=1
   make -C cli nodes-up
   eval "$(make -C cli --no-print-directory nodes-hosts)"
   ```

   The first run builds the node image, which takes several minutes. `nodes-hosts` prints the path of a `hosts.csv` with the node's name, address and TPM thumbprint as `NVKECTL_E2E_HOSTS_CSV`, and the SPIRE certificate authority (CA) the node's agent trusts as `NVKECTL_E2E_SPIRE_CA_CERT` and `NVKECTL_E2E_SPIRE_CA_KEY`. Real hardware: [Prepare nodes](../operate/prepare-nodes.md), where the data center hands you the same three things.

3. Generate the site spec for one node.

   ```bash
   export NVKECTL_SPIRE_CA_CERT_FILE="$NVKECTL_E2E_SPIRE_CA_CERT" NVKECTL_SPIRE_CA_KEY_FILE="$NVKECTL_E2E_SPIRE_CA_KEY"
   nvkectl generate sitespec local-site --nodes "$NVKECTL_E2E_HOSTS_CSV" \
     --set cluster.podCidr=10.244.0.0/16 \
     --set cluster.kubeletConfig.systemReservedMemory=512Mi \
     --set spire.upstreamCA=true > sitespec.yaml
   ```

   One node and no `cluster.controlPlaneVIP`: the node's own address is the API endpoint, so no kube-vip and no BGP peer are needed. The pod network moves off the default `192.168.0.0/16`, which Colima's DNS server sits in and OVN-Kubernetes would route into the overlay. `512Mi` is the system reservation for a container node. Real hardware: [Bootstrap the management cluster](../operate/bootstrap-management-cluster.md), *Write the site spec*.

4. Bootstrap the cluster.

   ```bash
   nvkectl bootstrap site -f sitespec.yaml --timeout 25m
   ```

   The command writes `local-site.kubeconfig` and `local-site-etcd-encryption-key.json`. It takes about seven minutes. Real hardware: the same command.

## Install the management plane

5. Install the lab StorageClass. Real hardware brings the CSI driver for its storage; the lab uses Rancher's local-path provisioner at the version `make/tool-versions.mk` pins.

   ```bash
   kubectl --kubeconfig local-site.kubeconfig apply -f "https://raw.githubusercontent.com/rancher/local-path-provisioner/$(make --no-print-directory print-LOCAL_PATH_PROVISIONER_VERSION)/deploy/local-path-storage.yaml"
   kubectl --kubeconfig local-site.kubeconfig annotate storageclass local-path storageclass.kubernetes.io/is-default-class=true --overwrite
   ```

6. Export the install's credentials. Nothing here reaches a real NICo, so the token and the node login are placeholders; the database password is yours.

   ```bash
   export NVKECTL_NICO_TOKEN=example-token NVKECTL_NODE_USER=nvke NVKECTL_NODE_PASSWORD=example-password
   export NVKECTL_DATABASE_PASSWORD="$(openssl rand -hex 16)"
   ```

   Real hardware: [Install management-plane services](../operate/install-management-plane-services.md), *Write the stack config*, with the NICo token and a password from your secret store.

7. Build the stack artifacts from the checkout.

   ```bash
   make local-stack-artifacts CONTAINER_CLI=nerdctl
   ```

   The target writes `dist/local-site/stack-artifacts.json` and one OCI layout per artifact under `dist/local-site/images/`. Real hardware: [Get the NVKE artifacts](../operate/get-artifacts.md), the same command or a file naming images in your registry.

8. Check the site. The stack config is the local site's, which names `nico-sim` on the node network's gateway.

   ```bash
   export CTR="nerdctl exec -i nvkectl-e2e-node1 ctr"
   deploy/check-site.sh --sitespec sitespec.yaml --stack-config deploy/examples/local-site/stack-config.json --stack-artifacts dist/local-site/stack-artifacts.json --kubeconfig local-site.kubeconfig
   ```

   `CTR` is how the install imports the layouts into the node's containerd: the node is a container, so the import goes through `nerdctl exec`. On real hardware the install runs on the node and `CTR` is `ctr`. The check passes and prints the next two commands.

9. Install Cluster API.

   ```bash
   clusterctl init --kubeconfig local-site.kubeconfig --core "cluster-api:$(make --no-print-directory print-CAPI_VERSION)" --bootstrap "kubeadm:$(make --no-print-directory print-CAPI_VERSION)" --control-plane "kubeadm:$(make --no-print-directory print-CAPI_VERSION)" --wait-providers
   ```

10. Install the stack.

    ```bash
    deploy/nvke-deploy.sh install --deploy-path deploy/nico --sitespec sitespec.yaml --stack-config deploy/examples/local-site/stack-config.json --stack-artifacts dist/local-site/stack-artifacts.json --kubeconfig local-site.kubeconfig
    ```

    The install ends with the path of its record under `$XDG_STATE_HOME/nvke/installs/`.

11. Give the Cluster API controllers a SPIFFE identity, admit the lab networks to the NVKE API, and point `nvkectl` at it. The two networks are the node network the host is on and the Docker `kind` network the workload cluster nodes come up on.

    ```bash
    controller/hack/apply-capi-spiffe-exec-auth.sh --kubeconfig local-site.kubeconfig
    HOST_CIDR="$(nerdctl network inspect nvkectl-e2e-nodes --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}')"
    TENANT_CIDR="$(docker network inspect kind --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | awk '{print $1}')"
    sed -e "s|REPLACE_HOST_CIDR|$HOST_CIDR|" -e "s|REPLACE_TENANT_CIDR|$TENANT_CIDR|" deploy/examples/local-site/nvke-api-clients.yaml | kubectl --kubeconfig local-site.kubeconfig apply -f -
    nvkectl config set-site --name local-site --url http://172.26.0.10:30080 --kubeconfig "$PWD/local-site.kubeconfig"
    ```

    `172.26.0.10` is the node's address, the one `hosts.csv` lists. Real hardware: [Install management-plane services](../operate/install-management-plane-services.md), steps 4 to 6, with your operator networks.

12. Publish the release set for your architecture, `amd64` or `arm64`.

    ```bash
    ARCH="$(go env GOARCH)"
    nvkectl create machineimage -f "deploy/examples/local-site/releases/machineimage-$ARCH.yaml"
    nvkectl create clusterrelease -f "deploy/examples/local-site/releases/clusterrelease-$ARCH.yaml"
    nvkectl create nodepoolrelease -f "deploy/examples/local-site/releases/nodepoolrelease-$ARCH.yaml"
    ```

    The set boots `nico-sim`'s container nodes; the iPXE script is a placeholder. Real hardware: [Write release manifests](../operate/write-release-manifests.md).

## Create a workload cluster

13. Start `nico-sim` with its container backend, listening on the node network's gateway, where the stack config points.

    ```bash
    CGO_ENABLED=1 make -C controller/nico-sim build
    NICO_SIM_LISTEN_ADDR=0.0.0.0:18080 NICO_SIM_SEED_PATH=controller/nico-sim/internal/seed/default-seed.yaml \
      NICO_VMS_VM_BACKEND=mock NICO_VMS_BGP_ENABLED=false \
      NICO_CONTAINERS_ENABLED=true NICO_CONTAINERS_REQUIRE_HEALTHY=true NICO_CONTAINERS_NODE_NETWORK=kind \
      NICO_CONTAINERS_NODE_IMAGE=cwe-capd-kindest-node:local \
      controller/nico-sim/bin/nico-sim &
    ```

    `cwe-capd-kindest-node:local` is the node image step 2 built; `docker image inspect cwe-capd-kindest-node:local` confirms Docker has it, and `docker network inspect kind` the network. Real hardware: [Configure an infrastructure provider: NICo](../operate/provider-nico.md), where NICo is yours.

14. Create the cluster and wait for it.

    ```bash
    nvkectl create cluster -f "deploy/examples/local-site/tenant-$ARCH.yaml"
    nvkectl get clusters
    ```

    The status moves from `Provisioning` to `Provisioned`, in about eight minutes. Real hardware: [Create a workload cluster](../workload-clusters/create.md), with your NICo identifiers.

## Verify

```bash
nvkectl get kubeconfig tenant-1 --output-file tenant-1.kubeconfig
kubectl --kubeconfig tenant-1.kubeconfig get nodes
```

Two nodes, one control plane and one worker, are `Ready`. `curl http://172.26.0.10:30080/readyz` answers `{"status":"ok",...}`, and `nvkectl get providers` lists CAPNICo.

## Clean up

```bash
nvkectl delete cluster tenant-1
kill %1
docker rm -f $(docker ps -aq --filter label=nvidia.com/nico-containers.managed=true)
make -C cli nodes-down
```

On macOS, `colima stop` keeps the machine for the next run and `colima delete` removes it.

## Next steps

- [Plan your installation](../operate/plan-installation.md) for real hardware.
- [Observe NVKE and its clusters](../operate/observe.md).
