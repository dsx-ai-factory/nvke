# Quickstart: evaluate locally with mock-NICo

> **Preview:** NVKE is available as a preview. APIs, configuration and behavior can change between releases, and we do not recommend NVKE for production workloads. See [Preview status and known limitations](../overview/preview-status.md).

This page describes how to run NVIDIA Kubernetes Engine (NVKE) on a Linux machine and create a workload cluster through the NVKE API. The setup needs no GPU hardware. mock-NICo simulates the NVIDIA Infra Controller (NICo) API and starts each node of the workload cluster as a Docker container.

The setup has four parts:

- A kind cluster named `controller-capi`. It is the management cluster. Cluster API runs in it.
- The NVKE management plane. Tilt deploys it into the management cluster. It listens on `http://localhost:8080`.
- CAPNICo, the Cluster API infrastructure provider for NICo. Tilt installs it from its published Helm chart.
- mock-NICo, the `nico-sim` process. CAPNICo calls it in place of NICo.

**Note: The setup is for local evaluation and is not intended for production use.**

## Before you begin

- A Linux machine
- At least 6 CPUs, 12 GiB of free memory, and 100 GiB of free disk space
- `sudo` access to install Linux kernel module `tpm_vtpm_proxy`
- `git`, GNU Make, Go 1.27.3 or later, a C compiler such as `gcc`, `kind` v0.32.0, `kubectl`, Helm 4, `tilt`, `jq`, `curl`, `nc`, `dpkg-deb`, and `dpkg-scanpackages`
- Free ports: 179, 5432, 7233, 8080, 8081, 10350, 18080, 30440, and 30443

## Prepare the machine

1. Clone the repository and go into it.

   ```bash
   git clone https://github.com/dsx-ai-factory/nvke.git
   cd nvke
   ```

2. Set the inotify instance limit to at least 1024. Tilt needs this limit for its file watcher. The setting lasts until the machine restarts.

   ```bash
   [ "$(sysctl -n fs.inotify.max_user_instances)" -ge 1024 ] || sudo sysctl -w fs.inotify.max_user_instances=1024
   ```

3. Load the `tpm_vtpm_proxy` kernel module. The simulated nodes use it for their TPMs.

   ```bash
   make -C controller host-up
   ```   

4. Build `nvkectl` and add it to your `PATH`.

   ```bash
   make -C cli build
   export PATH="$PWD/cli/bin:$PATH"
   ```

## Start the management cluster

1. Write the Tilt configuration. It enables the NICo provider and runs mock-NICo with container nodes. Tilt installs the latest stable release of the published CAPNICo Helm chart.

   ```bash
   jq -n '{
     providers: ["capi_core", "bgp-containers", "nico"],
     nico_sim_backends: ["container"]
   }' > controller/tilt_config.json
   ```

2. Create the Tilt environment file. The example file contains example passwords for local use.

   ```bash
   cp controller/.env.tilt.example controller/.env.tilt
   ```

3. Create the management cluster. The command creates the kind cluster `controller-capi`, installs Cluster API, and builds the node image for the simulated nodes. 

   ```bash
   make -C controller capd-up
   ```

4. Open a new terminal in the `nvke` directory and start Tilt. Leave Tilt running in this terminal.

   ```bash
   cd controller && tilt up
   ```

   Tilt prints the address of its web UI. Open the UI and wait until all resources are green. `nico-sim` is mock-NICo, `capnico-provider` installs CAPNICo, and `nvke-controller` is the management plane.

## Create a workload cluster

1. Write the management cluster kubeconfig to a file. The file contains only the `kind-controller-capi` context and its admin credentials.

   ```bash
   kubectl config view --raw --minify --context kind-controller-capi > local-mgmt.kubeconfig 
   chmod 600 local-mgmt.kubeconfig
   ```

2. Point `nvkectl` at the management plane. The `--kubeconfig` option is the absolute path of the management cluster kubeconfig. The command saves the site `local` and makes it the current site.

   ```bash
   nvkectl config set-site --name local --url http://localhost:8080 --kubeconfig "$PWD/local-mgmt.kubeconfig"
   ```

3. Check that CAPNICo is ready. The `nico` row should show `CONFIGURED`.

   ```bash
   nvkectl get providers
   ```

4. Create the cluster. The file `cli/examples/nico-mock-cluster.yaml` describes the cluster `my-cluster`. It has Kubernetes 1.34.8, one control-plane node, and a node pool named `worker` with one node. The cluster uses the site that mock-NICo seeds. Its `controlPlaneVIP` is `172.31.2.1`. The local Border Gateway Protocol (BGP) peer that Tilt starts routes this address on your machine.

   ```bash
   nvkectl create cluster -f cli/examples/nico-mock-cluster.yaml
   ```

5. Check the cluster status. Repeat the command until `STATUS` is `Provisioned`.

   ```bash
   nvkectl get clusters
   ```

   The output is:

   ```text
   NAME         KUBERNETES VERSION   CNI              STATUS        ID             AGE
   my-cluster   1.34.8               ovn-kubernetes   Provisioned   <CLUSTER_ID>   <AGE>
   ```

## Access the workload cluster

1. Write the admin kubeconfig of the workload cluster to a file.

   ```bash
   nvkectl get kubeconfig my-cluster --output-file my-cluster.kubeconfig
   ```

2. Watch the nodes. The cluster has two nodes: one with the `control-plane` role and one worker. Both nodes show `Ready` after a few minutes. Press Ctrl+C to stop the watch.

   ```bash
   kubectl --kubeconfig my-cluster.kubeconfig get nodes --watch
   ```

## Clean up

1. Delete the workload cluster. Tilt must run while NVKE deletes the cluster, because mock-NICo removes the node containers.

   ```bash
   nvkectl delete cluster my-cluster
   ```

2. Check the cluster list. Repeat the command until `my-cluster` is not in the list.

   ```bash
   nvkectl get clusters
   ```

3. Stop Tilt. Press Ctrl+C in the terminal that runs Tilt.

4. Remove the kind cluster, the BGP peer, the registry caches, and the `kind` Docker network.

   ```bash
   make -C controller kind-down bgp-containers-down
   docker rm -f registry-mirror registry-k8s-mirror
   docker network rm kind
   ```

5. Remove the file that loads the `tpm_vtpm_proxy` module at boot.

   ```bash
   sudo rm -f /etc/modules-load.d/99-capi-vtpm.conf
   ```

6. Remove the `local` site from the `nvkectl` configuration. Delete the `local` entry under `sites`, with its `url` and `kubeconfig` lines. Delete the line `current-site: local`. Then check the result. The list of sites does not contain `local`.

   ```bash
   vi $XDG_CONFIG_HOME/nvkectl/config.yaml
   nvkectl config view
   ```

## Next steps

- [What is NVKE](../overview/what-is-nvke.md)
- [Plan your installation](../operate/plan-installation.md)
