# Install management-plane services

This page describes how to install the NVIDIA Kubernetes Engine (NVKE) management plane on the management cluster: Cluster API, the Cluster API infrastructure provider for NVIDIA Infra Controller (NICo), called CAPNICo, SPIRE, NVIDIA Kubernetes Telemetry Pipeline (NKTP), and the NVKE API and controllers. You do this after [Bootstrap the management cluster](bootstrap-management-cluster.md) and before you configure the infrastructure provider and API authentication.

`nkectl` has no command for this step. You run `clusterctl init` for Cluster API, then the repository's install script, `deploy/nke-deploy.sh`, with the `deploy/nico` deploy path. The script installs each component with Helm or `kubectl apply --server-side`, in the order the deploy path lists, and stops at the first component that fails.

## Before you begin

- You have the outputs of [Build node images with NVKR](build-node-images.md): the controller image (the `cwe` output) in your registry, the profile content image and its digest, and the OS profile resources in `example-releases`.
- You have completed [Bootstrap the management cluster](bootstrap-management-cluster.md), and you have its kubeconfig, `<CLUSTER_NAME>.kubeconfig`. Every node is `Ready` and no node carries the control-plane `NoSchedule` taint.
- You have the other bootstrap output files in secure storage. This procedure does not use `<CLUSTER_NAME>-etcd-encryption-key.json`, `clusterstaticentry-crd.yaml` or `clusterstaticentries.yaml`.
- The management cluster has a default StorageClass. `kubectl get storageclass` lists one marked `(default)`. The bootstrap installs none, and the management plane's database claims a 40 GiB volume that stays `Pending` without one. Which provisioner to use depends on your storage hardware.
- You have the NICo API endpoint, your organization ID, an API token and the API name. [Configure an infrastructure provider: NICo](provider-nico.md) explains each value and how to rotate the token.
- You have a GitHub user name and a token with `read:packages`. CAPNICo's image is on `ghcr.io`, and the install creates a pull Secret for it from these credentials.
- You run the commands on a Linux host that reaches the management cluster's Kubernetes API, such as the bootstrap host. It has `helm`, `kubectl`, `clusterctl` `v1.12.3`, `python3` and `git` on `PATH`, and the `nke/` checkout from [Bootstrap the management cluster](bootstrap-management-cluster.md) at the same release.
- The host and the management cluster have outbound access to the chart repositories and registries the install pulls from: `registry-1.docker.io`, `spiffe.github.io`, `go.temporal.io`, `open-telemetry.github.io`, `prometheus-community.github.io`, `kubernetes-sigs.github.io`, `github.com`, `docker.io`, `ghcr.io`, `quay.io` and `registry.k8s.io`.

Run every command on this page from the directory that holds the kubeconfig and the `nke/` checkout, with `KUBECONFIG` set.

```bash
export KUBECONFIG=$PWD/<CLUSTER_NAME>.kubeconfig
```

## Install Cluster API

1. Install Cluster API core and the kubeadm providers at the version NVKE pins.

   ```bash
   clusterctl init \
     --core cluster-api:v1.12.3 \
     --bootstrap kubeadm:v1.12.3 \
     --control-plane kubeadm:v1.12.3 \
     --wait-providers
   ```

   The command also installs cert-manager, which the install script needs. It returns when every provider is running. If it times out on cert-manager, check that the control-plane taint is gone.

## Write the install configuration

The deploy path's own values are the same on every site. You supply what is specific to your site in one JSON file and four values files. They hold credentials: keep them outside the `nke/` checkout, readable only by you.

1. Write the site configuration as `install-config.json`.

   ```json
   {
     "ClusterName": "<CLUSTER_NAME>",
     "Nico": {
       "Endpoint": "<NICO_ENDPOINT>",
       "OrgID": "<NICO_ORG_ID>",
       "Token": "<NICO_TOKEN>",
       "APIName": "<NICO_API_NAME>"
     },
     "Registry": {
       "Server": "ghcr.io",
       "Username": "<GITHUB_USER>",
       "Token": "<GITHUB_TOKEN>"
     },
     "Tenant": {
       "User": "<NODE_USER>",
       "Password": "<NODE_PASSWORD>"
     }
   }
   ```

   - `<CLUSTER_NAME>`: the management cluster's name. NKTP labels the management cluster's telemetry with it.
   - `<NICO_ENDPOINT>`, `<NICO_ORG_ID>`, `<NICO_TOKEN>` and `<NICO_API_NAME>`: your NICo API details. The install writes them to the `nico-credentials` Secret in `capnico-system`.
   - `<GITHUB_USER>` and `<GITHUB_TOKEN>`: the credentials for CAPNICo's image. The install renders them into the pull Secret `ghcr-imagepull` in `capnico-system`, which CAPNICo's Deployment names, and refuses to run with either field empty. Supply a token with `read:packages` even if you can pull the image without one.
   - `<NODE_USER>` and `<NODE_PASSWORD>`: the login account NVKE writes into the cloud-init user data of every workload-cluster node. Without it, every workload cluster fails when NVKE renders the node user data.

   Every field is required. The install refuses to start a component whose template refers to an empty or missing field.

2. Write the management plane's values as `controller.yaml`.

   ```yaml
   image:
     repository: <CONTROLLER_IMAGE_REPOSITORY>
     tag: <CONTROLLER_IMAGE_TAG>
     digest: <CONTROLLER_IMAGE_DIGEST>
   profiles:
     path: /profiles
     image:
       enabled: true
       repository: <REGISTRY_HOST>/<NAMESPACE>/nvke-profiles
       digest: <PROFILES_DIGEST>
   appDb:
     postgresPassword: <DB_PASSWORD>
   spiffe:
     trustDomain: <TRUST_DOMAIN>
     spire:
       serverAddress: <MANAGEMENT_ADDRESS>
   ```

   - `<CONTROLLER_IMAGE_REPOSITORY>`, `<CONTROLLER_IMAGE_DIGEST>` and `<CONTROLLER_IMAGE_TAG>`: the controller image in your registry, from the `Derived artifact reference:` of the `cwe` output that `nkdctl release browse` lists in [Build node images with NVKR](build-node-images.md). That reference is `<CONTROLLER_IMAGE_REPOSITORY>@<CONTROLLER_IMAGE_DIGEST>`, pinned by digest with no tag. The digest selects the image, so `<CONTROLLER_IMAGE_TAG>` is any label you choose, such as `v1`. The deploy path's default names an image you cannot pull, so set all three.
   - `profiles`: the values you recorded in [Build node images with NVKR](build-node-images.md). `<REGISTRY_HOST>/<NAMESPACE>/nvke-profiles` is the profile content image and `<PROFILES_DIGEST>` its `sha256:` digest. The image carries your config profiles and hook scripts. The chart refuses to render without a digest or a tag.
   - `<DB_PASSWORD>`: a password you choose for the management plane's database. You enter the same value in two more files below.
   - `<TRUST_DOMAIN>`: the SPIFFE trust domain from your site spec, `spire.trustDomain`.
   - `<MANAGEMENT_ADDRESS>`: the address workload-cluster nodes use to reach the management cluster: its API virtual IP address (VIP), or the node IP of a single-node cluster. Nodes reach SPIRE there on port 30440.

3. If your registry requires credentials, create the management plane's namespace and a pull Secret in it, then name the Secret in `controller.yaml`.

   ```bash
   kubectl create namespace nke-controller
   ```

   ```bash
   kubectl create secret docker-registry registry-pull -n nke-controller \
     --docker-server=<REGISTRY_HOST> --docker-username=<REGISTRY_USER> --docker-password=<REGISTRY_TOKEN>
   ```

   ```yaml
   imagePullSecrets:
     - name: registry-pull
   ```

   The same Secret applies to the controller image and the profile content image, so both must be in a registry it can read.

4. Write the database values as `postgresql.yaml`, with the same `<DB_PASSWORD>`.

   ```yaml
   auth:
     postgresPassword: <DB_PASSWORD>
   ```

5. Write the workflow-service values as `temporal.yaml`, with the same `<DB_PASSWORD>` in both places.

   ```yaml
   server:
     config:
       persistence:
         default:
           sql:
             password: <DB_PASSWORD>
         visibility:
           sql:
             password: <DB_PASSWORD>
   ```

   If the password is missing from either one, the `temporal` component's schema job fails with `password authentication failed` and the install stops.

6. Write the SPIRE values as `spire.yaml`.

   ```yaml
   global:
     spire:
       trustDomain: <TRUST_DOMAIN>
       clusterName: <CLUSTER_NAME>
   spire-server:
     adminIDs:
       - spiffe://<TRUST_DOMAIN>/nke-service/nke-controller
     nodeAttestor:
       tpmDirect:
         checksum: <TPM_ATTESTOR_CHECKSUM>
   ```

   `<TRUST_DOMAIN>` must be the same in both places and in `controller.yaml`. If they differ, the management plane has no admin access to SPIRE and no error says so. `<TPM_ATTESTOR_CHECKSUM>` is the checksum of SPIRE's TPM attestor plugin for the management nodes' architecture:

   | Architecture | `<TPM_ATTESTOR_CHECKSUM>` |
   |---|---|
   | x86_64 | `46d0caad8c25a027dd11c93e18b58a8bc6fbd9f1fe2e36fa2a0dd440986de4dc` |
   | aarch64 | `b69d0153c8241383f24da138bc85429a620d9f962a2f10abb314aa0e39046d16` |

   This is the SPIRE server plugin's checksum. It is not the `TPM_PLUGIN_CHECKSUM` that nodes carry for the agent plugin.

NKTP sends telemetry to a bundled development backend that keeps data in memory and has no authentication. To send it to your own backend, write a values file as described in [Getting started with NKTP](../nktp/getting-started.md) and add `--values-for nktp=<FILE>` to the install command.

## Run the install

1. Print every command the install would run, without changing the cluster.

   ```bash
   bash nke/deploy/nke-deploy.sh install --deploy-path nke/deploy/nico \
     --config install-config.json \
     --values-for controller=controller.yaml \
     --values-for postgresql=postgresql.yaml \
     --values-for temporal=temporal.yaml \
     --values-for spire=spire.yaml \
     --dry-run
   ```

   The output lists the `kubectl apply` or `helm upgrade --install` command for each component, the `helm` commands that fetch NKTP's chart dependencies, and a `kubectl wait` for the OIDC CA certificate. The dry run also renders the templates against `install-config.json`, so a missing or empty field fails here.

2. Run the same command without `--dry-run`.

   The script installs the components in this order and waits for each Helm release to become ready, for up to 15 minutes each (`--timeout` changes it):

   | Component | Namespace | What it installs |
   |---|---|---|
   | `nke-base-config` | several | Namespaces, the NICo credentials and image pull Secrets, the node login Secret, the OpenID Connect (OIDC) certificate authority (CA) chain, and the cluster name for NKTP. |
   | `spire-tpm-pubhashes`, `spire-server-nodeport`, `spire-oidc-provider-nodeport` | `spire-server` | The TPM allowlist that NVKE fills as nodes are created, and NodePorts 30440 (SPIRE server) and 30443 (OIDC discovery). |
   | `capnico` | `capnico-system` | CAPNICo `v0.0.45`, with the reboot and repair annotations NVKE uses. |
   | `nktp` | `nktp` | NKTP telemetry and the monitoring custom resource definitions. |
   | `postgresql`, `spire-crds`, `temporal`, `spire` | `postgres`, `spire-server`, `temporal`, `spire-server` and `spire-system` | The management plane's database, workflow service and identity services. |
   | `controller` | `nke-controller` | The NVKE API and controllers, release `nke-controller`, with the API on NodePort 30080. |

3. Give the Cluster API controllers a SPIFFE identity.

   ```bash
   bash nke/controller/hack/apply-capi-spiffe-exec-auth.sh
   ```

   The script patches the Cluster API controller Deployments so that they reach workload clusters with a SPIFFE token rather than a stored admin kubeconfig. Run it after the install, because it waits for the patched controllers only once SPIRE's CSI driver exists. It prints `skip capd-system/capd-controller-manager (not installed)`, which is expected.

4. Apply the OS profile resources that you generated in [Build node images with NVKR](build-node-images.md).

   ```bash
   kubectl apply --server-side -f example-releases
   ```

   `example-releases` holds a `MachineImage`, a `ClusterRelease` and a `NodePoolRelease` for each OS profile. The controller chart installed their custom resource definitions. NVKE resolves the `osProfile` of a workload cluster or node pool from them, so you cannot create a cluster until they exist.

   ```bash
   kubectl get machineimages,clusterreleases,nodepoolreleases
   ```

   Each profile has all three resources, with the same name.

## If the install fails

The script stops at the component that failed and prints the `helm` or `kubectl` error. Fix the cause and run the same command again: completed components are upgraded in place and manifests are re-applied.

- **A volume claim stays `Pending`.** The cluster has no default StorageClass.
- **The `controller` component times out with the pod in `ImagePullBackOff`.** The image reference or the pull Secret in `controller.yaml` is wrong.
- **`spire-server` restarts with `checksums did not match`.** `<TPM_ATTESTOR_CHECKSUM>` does not match the nodes' architecture.

Install on a cluster that has never had an earlier revision of this deploy path. The install does not take over or migrate an earlier installation. For deeper diagnosis, see [Troubleshoot](troubleshoot.md).

## Verify

1. Check that the management plane is running.

   ```bash
   kubectl rollout status deployment/nke-controller -n nke-controller
   ```

   The output ends with `successfully rolled out`.

2. Check that the NVKE API answers and that its dependencies are reachable.

   ```bash
   curl http://<MANAGEMENT_ADDRESS>:30080/readyz
   ```

   The response is `{"status":"ok","checks":{"database":"ok","temporal":"ok"}}`.

3. Point `nkectl` at the NVKE API and list clusters.

   ```bash
   nkectl config set-site --name <CLUSTER_NAME> --url http://<MANAGEMENT_ADDRESS>:30080 --kubeconfig <CLUSTER_NAME>.kubeconfig
   ```

   ```bash
   nkectl get clusters
   ```

   The command prints the column headings `NAME`, `KUBERNETES VERSION`, `CNI`, `STATUS`, `ID` and `AGE`, and no clusters.

The NVKE API now serves plain HTTP on port 30080 of every management node, with API authentication off. Any caller that reaches the port can call every endpoint until you complete [Secure the NVKE API](api-authentication.md).

## Next steps

- [Configure an infrastructure provider: NICo](provider-nico.md).
- [Secure the NVKE API](api-authentication.md).
