# Quickstart: evaluate locally with mock-NICo

This page describes how to run NVIDIA Kubernetes Engine (NVKE) on one Linux machine and create a workload cluster through the NVKE API, with no GPU hardware and no NVIDIA account. In place of NVIDIA Infra Controller (NICo), the machine runs mock-NICo, a simulator that answers the NICo API and starts each workload cluster node as a Docker container.

The setup has four parts on your machine:

- A kind cluster named `cwe-capi` that serves as the management cluster, with Cluster API installed.
- The NVKE management plane, deployed into that cluster by Tilt and served on `http://localhost:8080`.
- CAPNICo, the Cluster API infrastructure provider for NICo, built from source.
- mock-NICo (the `nico-sim` process), which CAPNICo calls as if it were NICo.

The example passwords, the simulated NICo and the Tilt-managed management plane are for local evaluation only. Do not reuse this setup for a real installation.

## Before you begin

- A Linux machine on x86-64 (amd64) or arm64. macOS is not covered by this page.
- At least 6 CPUs, 12 GiB of free memory and 100 GiB of free disk. These are the sizes the repository gives its own development VM for this stack.
- The Linux kernel module `tpm_vtpm_proxy`, which the simulated nodes use for their TPMs. On Debian and Ubuntu, the setup step below installs it if it is missing. You need `sudo` for that step.
- Docker Engine with BuildKit, usable by your user without `sudo`.
- These tools on `PATH`: `git`, GNU Make, Go 1.27.2 or later, a C compiler such as `gcc`, `kind` v0.32.0, `kubectl`, Helm 4, `tilt`, `jq`, `curl`, `nc`, `dpkg-deb` and `dpkg-scanpackages`. On Debian and Ubuntu, `dpkg-scanpackages` is in the `dpkg-dev` package. An older Go downloads 1.27.2 automatically unless `GOTOOLCHAIN=local` is set. You do not need `clusterctl`: the setup installs the pinned version into the repository.
- Nothing else listening on ports 179, 5432, 7233, 8080, 8081, 10350, 18080, 30440 or 30443.
- Outbound internet access to GitHub, the Go module proxy and public container registries.

## Prepare the machine

1. Clone the repository and change into it.

   ```bash
   git clone https://github.com/dsx-ai-factory/nvke.git
   ```

   ```bash
   cd nke
   ```

   Run every command on this page from the `nke` directory unless a step says otherwise.

2. Load the virtual TPM module.

   ```bash
   make -C controller host-up
   ```

   The output includes `ensure-vtpm-proxy: /dev/vtpmx ready`, or `ensure-vtpm-proxy: /dev/vtpmx already present` if the module was already loaded.

3. Build `nvkectl` and put it on your `PATH`.

   ```bash
   make -C cli build
   ```

   ```bash
   export PATH="$PWD/cli/bin:$PATH"
   ```

## Get the CAPNICo source

CAPNICo is public at [dsx-ai-factory/cluster-api-provider-nico](https://github.com/dsx-ai-factory/cluster-api-provider-nico). Use the version this repository pins, so that CAPNICo matches the management plane you run.

1. Download the pinned version with Go and print where Go stored it.

   ```bash
   CAPNICO_MODULE_DIR="$(go mod download -json github.com/dsx-ai-factory/cluster-api-provider-nico | jq -r .Dir)"
   ```

   ```bash
   echo "$CAPNICO_MODULE_DIR"
   ```

   The path ends in `cluster-api-provider-nico@` followed by the pinned version.

2. Copy it next to the `nke` directory and make the copy writable.

   ```bash
   mkdir -p ../cluster-api-provider-nico
   ```

   ```bash
   cp -R "$CAPNICO_MODULE_DIR/." ../cluster-api-provider-nico/
   ```

   ```bash
   chmod -R u+w ../cluster-api-provider-nico
   ```

   The copy contains `chart/Chart.yaml`. Tilt checks for that file before it builds CAPNICo.

## Start the management cluster

1. Write the Tilt configuration. It selects the NICo provider, runs mock-NICo with container nodes, and builds CAPNICo from the copy you made.

   ```bash
   cat > controller/tilt_config.json <<'EOF'
   {
     "providers": ["capi_core", "bgp-containers", "nico"],
     "nico_sim_backends": ["container"],
     "capnico_source_dir": "../../cluster-api-provider-nico"
   }
   EOF
   ```

   Tilt resolves `capnico_source_dir` relative to the `controller` directory.

2. Create the Tilt environment file.

   ```bash
   cp controller/.env.tilt.example controller/.env.tilt
   ```

   Open `controller/.env.tilt` and give each of the five empty values at the top a value. Tilt refuses to start while any of them is empty. Any values work for this local stack:

   ```text
   DGXC_TENANT_USER=example-user
   DGXC_TENANT_PASSWORD=example-password
   CNPG_SUPERUSER_PASSWORD=example-password
   CNPG_CLOUD_PASSWORD=example-password
   CNPG_TEMPORAL_PASSWORD=example-password
   ```

   Leave the rest of the file as it is.

3. Create the kind cluster, install Cluster API, and build the node image the simulated nodes run.

   ```bash
   make -C controller capd-up
   ```

   The command also downloads `clusterctl` into `controller/.local/bin` and starts two local registry caches. Near the end, the output includes:

   ```text
   ✅ CAPD management cluster is ready
   Next step: tilt up
   ```

   Your current `kubectl` context is now `kind-cwe-capi`.

4. Start Tilt from the `controller` directory and leave it running.

   ```bash
   cd controller && tilt up
   ```

   Tilt prints the address of its web UI. Open it and wait until every resource is green. If `helm`, `docker`, `go` or `kind` is missing, the `check-management-cluster` resource fails and its log names the tool. `nico-sim` is mock-NICo, `capnico-provider` builds and installs CAPNICo, and `cwe` is the management plane.

Open a second terminal in the `nke` directory for the rest of this page, and run `export PATH="$PWD/cli/bin:$PATH"` in it.

## Create a workload cluster

1. Point `nvkectl` at the management plane.

   ```bash
   nvkectl config set-site --name local --url http://localhost:8080 --kubeconfig ~/.kube/config
   ```

   The output is `Site "local" configured (url: http://localhost:8080)`. `--kubeconfig` is the management cluster's kubeconfig, which `kind` wrote to `~/.kube/config`. Tilt turns on NVKE API authentication, and `nvkectl` uses this kubeconfig to get an API token for each request, so you do not handle tokens yourself.

2. Check that CAPNICo is ready.

   ```bash
   nvkectl get providers
   ```

   The `nico` row shows `CONFIGURED`. Other rows show `NOT-INSTALLED` and do not affect this page. If `nico` shows `NOT-CONFIGURED` or `DEGRADED`, wait for the Tilt resources to finish and run the command again.

3. Create the cluster from the example request.

   ```bash
   nvkectl create cluster -f cli/examples/nico-mock-cluster.yaml
   ```

   The request creates `my-cluster`: Kubernetes 1.34.2, one control-plane node and a node pool named `worker` with one node, on mock-NICo's seeded site. Its `controlPlaneVIP`, `172.31.2.1`, is routed on your machine by the local Border Gateway Protocol (BGP) peer that Tilt started. The output is:

   ```text
   Cluster "my-cluster" created (id: <CLUSTER_ID>)
   Provisioning is async — run 'nvkectl get clusters' to check status.
   ```

   `<CLUSTER_ID>` is the ID NVKE assigned to the cluster.

4. Check the cluster's status, and run the command again until `STATUS` is `Provisioned`.

   ```bash
   nvkectl get clusters
   ```

   ```text
   NAME         KUBERNETES VERSION   CNI              STATUS        ID             AGE
   my-cluster   1.34.2               ovn-kubernetes   Provisioned   <CLUSTER_ID>   <AGE>
   ```

   The status is `Provisioning` while mock-NICo starts the node containers and NVKE forms the cluster. If it becomes `ProvisioningFailed`, check the `cwe` and `nico-sim` logs in the Tilt UI, and the CAPNICo logs with `kubectl -n capnico-system logs deployment/controller-manager`.

## Access the workload cluster

1. Write the cluster's admin kubeconfig to a file.

   ```bash
   nvkectl get kubeconfig my-cluster --output-file my-cluster.kubeconfig
   ```

   The output is `kubeconfig written to my-cluster.kubeconfig`. The file is an admin credential for the cluster.

2. List the cluster's nodes.

   ```bash
   kubectl --kubeconfig my-cluster.kubeconfig get nodes
   ```

   Two nodes are listed, one with the `control-plane` role, both with status `Ready`.

## Clean up

1. Delete the workload cluster.

   ```bash
   nvkectl delete cluster my-cluster
   ```

   The output starts with `Cluster "my-cluster" deletion started`. Run `nvkectl get clusters` until `my-cluster` is no longer listed. Delete the cluster before you stop Tilt, because mock-NICo removes the node containers.

2. Stop Tilt with Ctrl+C in the first terminal.

3. Remove the kind cluster, the BGP peer, the registry caches and the `kind` Docker network.

   ```bash
   make -C controller kind-down
   ```

   ```bash
   make -C controller bgp-containers-down
   ```

   ```bash
   docker rm -f registry-mirror registry-k8s-mirror
   ```

   ```bash
   docker network rm kind
   ```

   `kind-down` ends with `✅ Kind management cluster deleted`.

4. Delete the repository, the CAPNICo copy, the mock-NICo binary and the `nvkectl` configuration.

   ```bash
   cd ..
   ```

   ```bash
   rm -rf nke cluster-api-provider-nico
   ```

   ```bash
   rm -f /tmp/cwe-nico-sim ~/.cache/cwe-nico-sim
   ```

   ```bash
   rm -rf ~/.config/nvkectl ~/.cache/nvkectl
   ```

   The `nke` directory holds the other files this page created, including `my-cluster.kubeconfig`, `controller/.env.tilt` and the build output in `controller/.local`.

The container images that the setup built or pulled stay in Docker, and `docker image ls` lists them. If `host-up` loaded `tpm_vtpm_proxy`, it also wrote `/etc/modules-load.d/99-capi-vtpm.conf`, which loads the module at boot. Delete that file with `sudo` if you do not want the module loaded after a reboot.

## Next steps

- [What is NVKE](../overview/what-is-nvke.md)
- [Plan your installation](../operate/plan-installation.md)
