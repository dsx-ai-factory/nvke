# Install the management plane

This page describes how to install the NVIDIA Kubernetes Engine (NVKE) management plane on the management cluster that `nvkectl bootstrap site` formed on your hardware: Cluster API, CAPNICo, SPIRE, NVKE telemetry (NKTP), PostgreSQL, Temporal and the NVKE controller. You do it after [Bootstrap the management cluster](bootstrap-management-cluster.md) and before you register node images or create workload clusters.

The install is a short sequence of commands. `clusterctl init` installs Cluster API, and `deploy/nvke-deploy.sh install` renders the `deploy/nico` deploy path for your site and applies it item by item. You then patch the Cluster API controllers for SPIRE and admit your networks to the NVKE API.

## Before you begin

- You have read [Plan your installation](plan-installation.md) and completed [Bootstrap the management cluster](bootstrap-management-cluster.md). You have the site spec the bootstrap ran with and `<CLUSTER_NAME>.kubeconfig`.
- The management cluster has exactly one default StorageClass, from the Container Storage Interface (CSI) driver for your storage. The bootstrap installs none, and the management plane's database claims a 40 GiB volume.
- You run every command on the install host, from the root of an `nvke/` checkout at the release you install, with the site spec, `<CLUSTER_NAME>.kubeconfig` and, if you install from one, `release.json` in the directory above it. Use the bootstrap host as the install host: it holds the controller image the bootstrap ran with, and [Stage the controller images](#stage-the-controller-images-on-the-management-nodes) copies that image from it to the nodes.
- The install host has `nvkectl` on `PATH`, built as in [Install nvkectl](bootstrap-management-cluster.md#install-nvkectl), and Helm `v4.2.4`, `kubectl`, `clusterctl` `v1.13.6`, `curl` and `python3`.
- Unless you install from a release, you can log in to every management node with `ssh` as a user who can run `sudo` without a password. The node image accepts no password logins, so the user data you wrote in [Prepare the nodes](prepare-nodes.md#write-the-user-data) must have added that user and your SSH key.
- The install host and the management cluster reach the registries and chart repositories the deploy path pulls from: `ghcr.io`, `quay.io`, `registry.k8s.io`, Docker Hub (`registry-1.docker.io`), `go.temporal.io`, `spiffe.github.io` and the NKTP chart dependencies on `kubernetes-sigs.github.io`, `open-telemetry.github.io` and `prometheus-community.github.io`. `clusterctl` also reads the Cluster API releases from `github.com`, `api.github.com` and `objects.githubusercontent.com`. A `mirror` in the stack config replaces the upstream registries for external images. `ghcr.io` serves the NVKE controller image only to authenticated callers, so the nodes do not pull it; [Stage the controller images](#stage-the-controller-images-on-the-management-nodes) puts it on the nodes instead.
- You have the NVIDIA Infra Controller (NICo) API endpoint, organization ID, API name and API token, and your SPIRE certificate authority (CA) certificate and key files, as listed in [Fill in your values](plan-installation.md#fill-in-your-values).

## Set the install inputs

The install reads the site spec, a stack config file for what only the management plane needs, and credentials from `NVKECTL_*` environment variables. No file holds a credential.

1. Copy the example stack config next to the site spec.

   ```bash
   cp deploy/examples/stack-config.json ../stack-config.json
   ```

2. Fill in `../stack-config.json`.

   | Key | Value |
   |---|---|
   | `nico.endpoint` | The NICo API base URL, `http` or `https`, with no user or password. |
   | `nico.orgID` | Your NICo organization ID. |
   | `nico.apiName` | The API name in the NICo API path, such as `forge`. |
   | `images.profiles` | The profile content image reference, with a tag or digest. Without a release description, set it to the name you stage in [Stage the controller images](#stage-the-controller-images-on-the-management-nodes). With one, leave it empty to use the release's `controller-profiles` artifact. |
   | `images.controller` | The controller image reference, with a tag or digest. Without a release description, set it to the name you stage; the image the deploy path pins otherwise is in a registry the nodes cannot pull from without a credential. With a release, leave it empty to use the release's image. |
   | `mirror` | Optional. A registry to pull external images from instead of their upstream registries, as `host[:port][/prefix]`. |

   The install refuses a file that still holds a `REPLACE_*` value, or that has a key the example does not have.

3. Export the credentials in the shell you install from. Read each secret without echoing it.

   ```bash
   read -rs NVKECTL_NICO_TOKEN && export NVKECTL_NICO_TOKEN
   ```

   ```bash
   export NVKECTL_NODE_USER=nvke
   read -rs NVKECTL_NODE_PASSWORD && export NVKECTL_NODE_PASSWORD
   ```

   ```bash
   read -rs NVKECTL_DATABASE_PASSWORD && export NVKECTL_DATABASE_PASSWORD
   ```

   ```bash
   export NVKECTL_SPIRE_CA_CERT_FILE=<CA_CERT_PATH> NVKECTL_SPIRE_CA_KEY_FILE=<CA_KEY_PATH>
   ```

   - `NVKECTL_NICO_TOKEN`: the NICo API token CAPNICo uses.
   - `NVKECTL_NODE_USER` and `NVKECTL_NODE_PASSWORD`: the login account NVKE writes into every workload cluster node's user data. Neither can contain a quote, a backslash or a control character.
   - `NVKECTL_DATABASE_PASSWORD`: the PostgreSQL password the controller and Temporal share. Keep it in your secret manager: every later install of the same site needs the same value.
   - `<CA_CERT_PATH>` and `<CA_KEY_PATH>`: absolute paths to the SPIRE CA files you used for the bootstrap.

4. If you install from an NVIDIA Kubernetes Recipes (NVKR) release, put its description in `../release.json`, exported with `nvkr release export` from a release reference that `nvkr release verify` accepted. The install then pulls every chart and image the release carries by digest. Without it, the charts come from this checkout and their public repositories.

## Stage the controller images on the management nodes

Without a release description, the controller item runs two images that no registry serves without a credential: the NVKE controller and a profile content image. The deploy path installs them with `imagePullPolicy: IfNotPresent` and no pull secret, so they must already be in every management node's containerd, in the `k8s.io` namespace the kubelet reads. The bootstrap loaded the controller image only into its temporary seed cluster, which it deleted. Skip this section if you install from a release: the nodes then pull both images by digest from the registries the release names.

`<ARCH>` below is the nodes' CPU architecture, `amd64` or `arm64`. The controller image you tag must be built for it.

1. Tag the controller image the bootstrap ran with under the name the install uses. `<CONTROLLER_IMAGE>` is `nke-controller:latest` if you built it with `make -C nvke/cli images`, or the reference you passed as `--nvke-controller-image`.

   ```bash
   nerdctl tag <CONTROLLER_IMAGE> nvke.local/nke-controller:site
   ```

2. Build the profile content image. The example profiles in `controller/profiles/example` are a starting point; [controller/profiles/README.md](https://github.com/dsx-ai-factory/nvke/blob/main/controller/profiles/README.md) describes how to build an image from your own profiles.

   ```bash
   nerdctl build --platform linux/<ARCH> -t nvke.local/controller-profiles:site -f controller/profiles/example/Dockerfile controller/profiles/example
   ```

3. Import both images into every management node's containerd. Run the command once for each node. `<NODE_USER>` is the account on the node that can run `sudo`, and `<NODE_ADDRESS>` is the node's IP address. The controller pod can run on any node, so do not skip one.

   ```bash
   nerdctl save --platform linux/<ARCH> nvke.local/nke-controller:site nvke.local/controller-profiles:site | ssh <NODE_USER>@<NODE_ADDRESS> sudo ctr -n k8s.io images import -
   ```

4. Check that each node lists both images.

   ```bash
   ssh <NODE_USER>@<NODE_ADDRESS> sudo ctr -n k8s.io images ls -q | grep nvke.local/
   ```

   The output names `nvke.local/nke-controller:site` and `nvke.local/controller-profiles:site`. Use namespace `k8s.io`: an image imported into another containerd namespace is invisible to the kubelet, which reports it as an image pull failure.

5. Name the two images in `../stack-config.json`.

   ```json
   "images": {
     "controller": "nvke.local/nke-controller:site",
     "profiles": "nvke.local/controller-profiles:site"
   }
   ```

   The names have no registry host: the nodes never pull them. Keep a tag other than `latest`, which Kubernetes pulls on every start. To run a new build later, import it on every node under a new tag, name the new tag here and run the install again.

## Check the site

1. Run the site check.

   ```bash
   deploy/check-site.sh --sitespec ../sitespec.yaml --stack-config ../stack-config.json --kubeconfig ../<CLUSTER_NAME>.kubeconfig
   ```

   Add `--release ../release.json` if you install from a release. `<CLUSTER_NAME>` is the management cluster's name.

   The check changes nothing. It prints one line for each finding, `PASS`, `WARN`, `SKIP` or `FAIL`, and every `FAIL` names its fix. It covers the tools and their versions, the checkout, Cluster API, the site spec, the stack config and credentials, the release, the cluster's nodes, taint and default StorageClass, outbound access and free disk. It does not look inside the nodes' containerd, so it cannot tell whether the controller images are staged.

2. Fix each `FAIL` and run the check again until none remain. When everything passes, the check prints the commands that remain, with your paths.

## Install Cluster API

1. Install Cluster API at the version NVKE pins.

   ```bash
   clusterctl init --kubeconfig ../<CLUSTER_NAME>.kubeconfig --core cluster-api:v1.13.6 --bootstrap kubeadm:v1.13.6 --control-plane kubeadm:v1.13.6 --wait-providers
   ```

   `clusterctl init` also installs cert-manager. It does not upgrade an existing Cluster API, so the site check fails on a Cluster API of another version. It reads the Cluster API releases through the GitHub API, which allows anonymous callers 60 requests an hour; if it reports a rate limit, export a GitHub token in `GITHUB_TOKEN` and run it again.

2. Wait for the cert-manager webhook. The SPIRE item of the install creates a certificate that the webhook admits.

   ```bash
   kubectl --kubeconfig ../<CLUSTER_NAME>.kubeconfig -n cert-manager rollout status deployment/cert-manager-webhook --timeout=5m
   ```

## Install the management plane

1. Install the deploy path.

   ```bash
   deploy/nvke-deploy.sh install --deploy-path deploy/nico --sitespec ../sitespec.yaml --stack-config ../stack-config.json --kubeconfig ../<CLUSTER_NAME>.kubeconfig
   ```

   Add `--release ../release.json` if you install from a release. Add `--dry-run` first to print every `kubectl` and `helm` command without running them.

   The install reads the nodes' CPU architecture from the cluster, renders the deploy path for your site into a private scratch directory, and applies the items in the order `deploy/nico/nvke-deploy.yaml` lists. Each Helm item waits up to 15 minutes; `--timeout` changes that. The install removes the scratch directory, with the credentials rendered into it, when it exits.

2. Patch the Cluster API controllers so that they reach workload cluster nodes with a SPIRE identity.

   ```bash
   controller/hack/apply-capi-spiffe-exec-auth.sh --kubeconfig ../<CLUSTER_NAME>.kubeconfig
   ```

   The script patches the core, kubeadm control-plane and kubeadm bootstrap controllers and waits for each rollout. It prints `skip capd-system/capd-controller-manager (not installed)`, which is expected. Run it again after every `clusterctl init` or Cluster API upgrade.

## Admit your networks to the NVKE API

The controller chart admits the NVKE API only from pods in the management cluster. Add a NetworkPolicy for the networks that call the API.

1. Copy the example policy.

   ```bash
   cp deploy/examples/local-site/nvke-api-clients.yaml ../nvke-api-clients.yaml
   ```

2. In `../nvke-api-clients.yaml`, replace `REPLACE_HOST_CIDR` with the management cluster's node network, and add an `ipBlock` for each other network that calls the API, such as the network of the hosts you run `nvkectl` on. Replace `REPLACE_TENANT_CIDR` with the network your workload cluster nodes come up on. Admit only trusted networks: the API serves plain HTTP, and with authentication off it serves every caller it admits.

   A request can reach the controller from a management node's address rather than the caller's own, because the NVKE API's NodePort forwards it from the node that receives it. Admitting the node network covers that. Filter port 30080 at your network edge as well, since the policy alone may not tell callers apart.

3. Apply the policy.

   ```bash
   kubectl --kubeconfig ../<CLUSTER_NAME>.kubeconfig apply -f ../nvke-api-clients.yaml
   ```

## Point nvkectl at the management plane

1. Register the site with `nvkectl`.

   ```bash
   nvkectl config set-site --name <CLUSTER_NAME> --url http://<MANAGEMENT_ADDRESS>:30080 --kubeconfig "$(realpath ../<CLUSTER_NAME>.kubeconfig)"
   ```

   `<MANAGEMENT_ADDRESS>` is the management cluster's API virtual IP address (VIP), or the node IP of a single-node management cluster. The NVKE API listens on NodePort 30080 on every management node.

## Verify

1. Check that the NVKE API answers.

   ```bash
   curl -sS --max-time 10 http://<MANAGEMENT_ADDRESS>:30080/healthz
   ```

   Run it from a host you admitted, not from a management node. The output is `{"status":"ok"}`. If it times out:

   - Check that the policy admits the network the host calls from and the management node network.
   - If it still times out, the request reaches the controller from the OVN-Kubernetes join network, `100.64.0.0/16`, which NVKE does not change. Add an `ipBlock` for it and check again. The policy then admits every caller that can reach port 30080, so limit that port to your operator networks at your network edge or the hosts' firewalls.

2. Check that CAPNICo is configured.

   ```bash
   nvkectl get providers
   ```

   The `nico` row reads `CONFIGURED`.

3. Check that the API serves requests.

   ```bash
   nvkectl get clusters
   ```

   The command lists no clusters and prints no error.

## Run the install again

Changing a management plane setting later, such as API authentication, telemetry or the controller image, means running the same `deploy/nvke-deploy.sh install` command again. Each item is upgraded in place. To change the controller or profile image, stage the new build on every node under a new tag first, as in [Stage the controller images](#stage-the-controller-images-on-the-management-nodes), then name the new tag in the stack config.

- Export the same `NVKECTL_*` values, including the same `NVKECTL_DATABASE_PASSWORD`.
- Pass the same flags, and every `--values-for <ITEM>=<FILE>` you used before. A file you leave out stops applying, so a later install without `--values-for controller=<FILE>` turns off the settings that file carried.

If the install stops at an item, fix the cause and run the same command again. To collect what the failed step left in the cluster, run:

```bash
deploy/check-site.sh --kubeconfig ../<CLUSTER_NAME>.kubeconfig --diagnostics
```

It writes nodes, pods, events, Helm releases and the controller logs under `NVKE_STATE_DIR/diagnostics`. `NVKE_STATE_DIR` defaults to `$XDG_STATE_HOME/nvke`, or `~/.local/state/nvke` when `XDG_STATE_HOME` is unset. [Troubleshoot](troubleshoot.md) lists the common install failures.

## Next steps

- [Build and host node OS images](build-node-images.md#register-the-image-for-provisioning) to register the image and releases the workload cluster nodes boot from.
- [Configure an infrastructure provider: NICo](provider-nico.md) to collect the NICo identifiers each workload cluster needs.
- [Secure the NVKE API](api-authentication.md) before you give anyone else access to the API.
