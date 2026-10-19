# Install management-plane services

This page describes how to install the NVIDIA Kubernetes Engine (NVKE) management plane on the management cluster: Cluster API, the Cluster API infrastructure provider for NVIDIA Infra Controller (NICo), called CAPNICo, PostgreSQL, Temporal, SPIRE, NVKE telemetry pipelines (NKTP), and the NVKE API and controllers. You do this after [Bootstrap the management cluster](bootstrap-management-cluster.md) and before you configure the infrastructure provider and API authentication.

The install is a short list of commands that you run in order. Each one stands on its own, and nothing orchestrates them. The first command checks the cluster and your host and tells you what is still missing. The install reads four inputs:

| Input | What it says |
|---|---|
| The site spec | What the bootstrap built and the management plane must agree with: the site name, the control-plane VIP and the SPIFFE trust domain. It is the file you wrote for the bootstrap, read as it is. |
| The stack config | What only the management plane needs: the NICo API. You write it in [Write the stack config](#write-the-stack-config). |
| The stack artifacts | Which images and charts to install, each by OCI reference and digest: the `stack-artifacts.json` file from [Get the NVKE artifacts](get-artifacts.md). |
| The deploy path, `deploy/nico` | What to install, in which order, with which values. It is in the `nvke/` checkout. |

## Before you begin

- You have completed [Bootstrap the management cluster](bootstrap-management-cluster.md). You have its kubeconfig, `<CLUSTER_NAME>.kubeconfig`, and its site spec, `sitespec.yaml`, in your working directory. The cluster has an odd number of nodes, all control-plane and all of one CPU architecture, and none carries the control-plane `NoSchedule` taint.
- The management cluster has exactly one default StorageClass. `kubectl get storageclass` lists one marked `(default)`. The bootstrap installs none, and PostgreSQL and SPIRE need persistent volumes. Install the Container Storage Interface (CSI) driver for your storage hardware and annotate its class `storageclass.kubernetes.io/is-default-class=true`. A lab can use the rancher local-path-provisioner.
- You have the NICo API endpoint, your organization ID, an API token and the API name. [Configure an infrastructure provider: NICo](provider-nico.md) explains each value.
- You have the stack artifacts file from [Get the NVKE artifacts](get-artifacts.md), and the `releases` directory from [Write release manifests](write-release-manifests.md).
- You run the commands on a Linux host that reaches the management cluster's Kubernetes API, such as the bootstrap host. It has the `nvke/` checkout from the bootstrap, at the same release as `nvkectl`, and these tools on `PATH`:
  - `nvkectl`. The install reads the deploy path and renders it with it. `NVKECTL` names another binary.
  - `helm` `v4.2.4`. Helm 3 cannot install these charts.
  - `clusterctl` `v1.13.6`, `kubectl`, `curl` and `python3`.

  The versions are the ones `make/tool-versions.mk` pins.
- The management cluster's nodes reach the registries the stack artifacts name for the artifacts without a local layout, and the images the third-party charts, the CAPNICo manifest and Cluster API run. The host reaches `github.com` for `clusterctl init`, the registries of the charts the file names, and `kubernetes-sigs.github.io`, `open-telemetry.github.io` and `prometheus-community.github.io` for NKTP's chart dependencies when the NKTP chart is not in the file.

Run every command on this page from the root of the `nvke/` checkout. The site spec, the stack config, the kubeconfig and the stack artifacts stay in the directory above it.

```bash
cd nvke
```

## Write the stack config

The install reads what only the management plane needs from `stack-config.json`, a JSON file you write beside the site spec, and its secrets from the environment. The site spec stays as the bootstrap left it.

1. Export the install's secrets. The install reads them from the environment, so you can keep the site spec in version control and use it from any machine.

   ```bash
   read -rs NVKECTL_NICO_TOKEN && export NVKECTL_NICO_TOKEN
   export NVKECTL_NODE_USER=<NODE_USER>
   read -rs NVKECTL_NODE_PASSWORD && export NVKECTL_NODE_PASSWORD
   read -rs NVKECTL_DATABASE_PASSWORD && export NVKECTL_DATABASE_PASSWORD
   export NVKECTL_SPIRE_CA_CERT_FILE=<CA_CERT_PATH> NVKECTL_SPIRE_CA_KEY_FILE=<CA_KEY_PATH>
   ```

   `read -s` keeps each value out of your terminal and shell history. Type the NICo API token, the node login password and the PostgreSQL password in turn.

   - `NVKECTL_NICO_TOKEN`: the NICo API token. The install writes it, with the NICo details below, to the `nico-credentials` Secret that CAPNICo reads.
   - `NVKECTL_NODE_USER` and `NVKECTL_NODE_PASSWORD`: the login account NVKE writes into the cloud-init user data of every workload-cluster node.
   - `NVKECTL_DATABASE_PASSWORD`: the PostgreSQL password the management plane and Temporal share. The install writes it to the `nvke-database` Secret in the `postgres`, `temporal` and `nke-controller` namespaces; the charts read it from there. Choose it once and store it in your secret manager: every later install of this site needs the same value.
   - `NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE`: the SPIRE CA certificate and key you gave the bootstrap, as absolute paths. A relative path resolves from the current directory, which is now the `nvke/` checkout. Every install of a site with `spire.upstreamCA: true` requires them. Leave both unset if the bootstrap ran without a CA.

2. Copy the example stack config.

   ```bash
   cp deploy/examples/stack-config.json ../stack-config.json
   ```

3. Fill in every `REPLACE_*` value in `../stack-config.json`.

   ```json
   {
     "nico": {
       "endpoint": "<NICO_ENDPOINT>",
       "orgID": "<NICO_ORG_ID>",
       "apiName": "<NICO_API_NAME>"
     }
   }
   ```

   - `<NICO_ENDPOINT>`, `<NICO_ORG_ID>` and `<NICO_API_NAME>`: your NICo API details. The API name is, for example, `forge`. The install writes them, with the token from step 1, to the `nico-credentials` Secret that CAPNICo reads.

   The site name, the SPIFFE trust domain and the control-plane VIP come from the site spec, the SPIRE certificate authority from `NVKECTL_SPIRE_CA_CERT_FILE` and `NVKECTL_SPIRE_CA_KEY_FILE` (step 1), the CPU architecture of the management nodes from the cluster, and every image and chart from the stack artifacts, so none of them is repeated here.

   NKTP sends telemetry to a bundled development backend that keeps data in memory and has no authentication. To send it to your own backend, write a values file as described in [Getting started with NKTP](../nktp/getting-started.md) and pass `--values-for nktp=<FILE>` to every install run.

   The file and the variables are checked in step 1 of [Install](#install), which names every key that is still a placeholder, empty or unknown, and every variable that is not set. To rotate the NICo API token later, export the new value and run the install again.

## Produce the stack artifacts

The install takes every image and chart from `stack-artifacts.json`, which [Get the NVKE artifacts](get-artifacts.md) describes how to produce: built from the checkout with `make local-stack-artifacts`, or written from the example file with the artifacts in your registry. A chart the file carries is installed by digest, from its layout or its registry; a chart it does not carry comes from the checkout or its public repository. An artifact with a local layout is imported into the node's containerd by the install, through the `CTR` command, which defaults to `ctr` on the node; on a cluster with more than one node, name the artifacts in a registry instead. The install refuses a file that lacks an image the deploy path names, for the management nodes' CPU architecture: the controller image `cwe`, the profile content image `controller-profiles` or the CAPNICo image `capnico-controller`. The commands below take the file as `../stack-artifacts.json`.

## Install

1. Check the site.

   ```bash
   deploy/check-site.sh --sitespec ../sitespec.yaml --stack-config ../stack-config.json --stack-artifacts ../stack-artifacts.json --kubeconfig ../<CLUSTER_NAME>.kubeconfig
   ```

   The check changes nothing. It starts with an `INFO` line naming the site spec, the stack config and the stack artifacts it reads, with the site name, the NICo endpoint and the artifact count, so a wrong file is seen first. Then it prints one line per finding, `PASS`, `WARN`, `SKIP` or `FAIL`, and every `FAIL` names its fix. It covers the tools and the `helm` pin, the checkout, the site spec, the stack config and the `NVKECTL_*` variables, the stack artifacts, their layouts and what the deploy path binds for the nodes' architecture, the cluster's API, the node count, the nodes' CPU architecture, the control-plane taint, the default StorageClass, the Cluster API version and that its kubeadm providers are present, egress and disk space. It exits non-zero on any `FAIL`. When everything passes, it prints the commands that remain with your paths filled in.

2. Install Cluster API at the version NVKE pins.

   ```bash
   clusterctl init --kubeconfig ../<CLUSTER_NAME>.kubeconfig \
     --core cluster-api:v1.13.6 \
     --bootstrap kubeadm:v1.13.6 \
     --control-plane kubeadm:v1.13.6 \
     --wait-providers
   ```

   The command also installs cert-manager. It returns when every provider is running. `clusterctl init` does not upgrade an existing installation: a Cluster API at another version is a finding of step 1, not something to install over.

3. Install the stack.

   ```bash
   deploy/nvke-deploy.sh install --deploy-path deploy/nico \
     --sitespec ../sitespec.yaml \
     --stack-config ../stack-config.json \
     --stack-artifacts ../stack-artifacts.json \
     --kubeconfig ../<CLUSTER_NAME>.kubeconfig
   ```

   The script reads the management nodes' CPU architecture from the cluster, renders `deploy/nico` for your site and against the stack artifacts into a private scratch directory, imports the artifacts that have a local layout into the node's containerd, applies the result one component at a time, waits for each, and removes the scratch directory, so no rendered credential is left on the host. Add `--dry-run` first to print every `kubectl` and `helm` call without changing the cluster. A successful install writes a record of what it installed, `images.txt`, `stack-artifacts.json`, `render.log` and `status`, under `$XDG_STATE_HOME/nvke/installs/<TIME>.<ID>/`, and prints the path. It installs the components in this order:

   | Component | Namespace | What it installs |
   |---|---|---|
   | `nvke-base-config` | several | Namespaces, the NICo credentials Secret, the node login Secret, the OpenID Connect (OIDC) certificate authority (CA) chain, and the cluster name for NKTP. |
   | `site-credentials` | `postgres`, `temporal`, `nke-controller`, `spire-server` | The `nvke-database` Secret with the PostgreSQL password and the management plane's connection string, and the `spire-upstream-ca` Secret when the site uses a CA. The render adds this item; the charts below read these Secrets. |
   | `spire-tpm-pubhashes`, `spire-server-nodeport`, `spire-oidc-provider-nodeport` | `spire-server` | The TPM allowlist that NVKE fills as nodes are created, and NodePorts 30440 (SPIRE server) and 30443 (OIDC discovery). |
   | `capnico` | `capnico-system` | CAPNICo, with the reboot and repair annotations NVKE uses. |
   | `nktp` | `nktp` | NKTP telemetry and the monitoring custom resource definitions. |
   | `postgresql`, `spire-crds`, `temporal`, `spire` | `postgres`, `spire-server`, `temporal`, `spire-server` and `spire-system` | The management plane's database, workflow service and identity services. |
   | `controller` | `nke-controller` | The NVKE API and controllers, release `nke-controller`, with the API on NodePort 30080. |

   The SPIFFE trust domain and the SPIRE server address come from the site spec, and the TPM attestor checksum from the architecture the management nodes report; you do not set them by hand.

4. Give the Cluster API controllers a SPIFFE identity.

   ```bash
   controller/hack/apply-capi-spiffe-exec-auth.sh --kubeconfig ../<CLUSTER_NAME>.kubeconfig
   ```

   The script patches the Cluster API controller Deployments so that they reach workload clusters with a SPIFFE token rather than a stored admin kubeconfig. Run it after step 3, because it checks the rollout only once SPIRE's CSI driver is registered. `clusterctl upgrade` removes the patch; run the script again after an upgrade.

5. Admit your client networks to the NVKE API. The chart admits the API only from pods in the cluster. Save this NetworkPolicy as `../nvke-api-clients.yaml`, with one `ipBlock` for each network your operators and automation call the API from:

   ```yaml
   apiVersion: networking.k8s.io/v1
   kind: NetworkPolicy
   metadata:
     name: nvke-api-clients
     namespace: nke-controller
   spec:
     podSelector:
       matchLabels:
         app.kubernetes.io/name: nke-controller
     policyTypes:
       - Ingress
     ingress:
       - from:
           - ipBlock:
               cidr: <CLIENT_CIDR>
         ports:
           - port: 8080
             protocol: TCP
   ```

   ```bash
   kubectl --kubeconfig ../<CLUSTER_NAME>.kubeconfig apply -f ../nvke-api-clients.yaml
   ```

   Which networks may reach the API is your decision. Until you complete [Secure the NVKE API](api-authentication.md), any caller you admit can call every endpoint.

6. Point `nvkectl` at the NVKE API.

   ```bash
   nvkectl config set-site --name <CLUSTER_NAME> --url http://<MANAGEMENT_ADDRESS>:30080 --kubeconfig "$PWD/../<CLUSTER_NAME>.kubeconfig"
   ```

   `<MANAGEMENT_ADDRESS>` is the management cluster's API virtual IP address (VIP), or the node IP of a single-node cluster.

7. Publish your OS release resources, the machine image first because both releases reference it.

   ```bash
   nvkectl create machineimage -f ../releases/machineimage.yaml
   ```

   ```bash
   nvkectl create clusterrelease -f ../releases/clusterrelease.yaml
   ```

   ```bash
   nvkectl create nodepoolrelease -f ../releases/nodepoolrelease.yaml
   ```

   The files are the ones you wrote in [Write release manifests](write-release-manifests.md). `nvkectl` writes the resources to the management cluster through the kubeconfig of the previous step and supplies their API version and kind; the controller chart installed their custom resource definitions in step 3. `nvkectl get machineimages` lists what you published.

## Run the install again

Other pages, such as [Secure the NVKE API](api-authentication.md), have you run the install command again to change the site's values. Each run renders the site from scratch, so give it the same inputs as the first:

- the `NVKECTL_*` variables from step 1 of [Write the stack config](#write-the-stack-config), exported in the shell you run it from, with the same values. A site spec with `spire.upstreamCA: true` refuses to render without the SPIRE CA variables.
- the same three files, `--sitespec`, `--stack-config` and `--stack-artifacts`, and the same flags, including every `--values-for` file you passed before, such as `--values-for nktp=<FILE>`. A file you leave out stops applying: without the `nktp` file, NKTP goes back to the bundled development backend.

## If the install fails

The script stops at the component that failed and prints the `helm` or `kubectl` error.

1. Collect what the failure left in the cluster.

   ```bash
   deploy/check-site.sh --kubeconfig ../<CLUSTER_NAME>.kubeconfig --diagnostics
   ```

   The command writes the nodes, pods, events, Helm releases, controller logs and the last successful install's record under `$XDG_STATE_HOME/nvke/diagnostics/`.

2. If a Helm release failed, uninstall it before you run the install again.

   ```bash
   helm --kubeconfig ../<CLUSTER_NAME>.kubeconfig uninstall <RELEASE> -n <NAMESPACE>
   ```

3. Fix the cause and run step 3 again. Completed components are upgraded in place and manifests are re-applied.

Common causes:

- **A volume claim stays `Pending`.** The cluster has no default StorageClass.
- **A pod stays in `ImagePullBackOff`.** The nodes cannot reach the registry the stack artifacts name for that image, or the image was imported from a layout into another node's containerd than the one the pod landed on: `nvke.local/...` is never pulled, only imported through `CTR`.
- **The render refuses the stack artifacts.** The file lacks an image the deploy path names for the nodes' architecture; the error lists every missing one.

Install on a cluster that has never had an earlier revision of this deploy path. The install does not take over or migrate an earlier installation. For deeper diagnosis, see [Troubleshoot](troubleshoot.md).

## Verify

1. Check that the management plane is running.

   ```bash
   kubectl --kubeconfig ../<CLUSTER_NAME>.kubeconfig rollout status deployment/nke-controller -n nke-controller
   ```

   The output ends with `successfully rolled out`.

2. From a machine in a network you admitted, check that the NVKE API answers and that its dependencies are reachable.

   ```bash
   curl http://<MANAGEMENT_ADDRESS>:30080/readyz
   ```

   The response is `{"status":"ok","checks":{"database":"ok","temporal":"ok"}}`.

3. List the infrastructure providers.

   ```bash
   nvkectl get providers
   ```

   CAPNICo is listed. [Configure an infrastructure provider: NICo](provider-nico.md) describes its states.

The NVKE API serves plain HTTP on port 30080, with API authentication off. Any caller in a network you admitted can call every endpoint until you complete [Secure the NVKE API](api-authentication.md).

## Next steps

- [Configure an infrastructure provider: NICo](provider-nico.md).
- [Secure the NVKE API](api-authentication.md).
