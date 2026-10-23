# Troubleshoot

This page lists the failures you are most likely to meet when you install NVIDIA Kubernetes Engine (NVKE) and run workload clusters, in the order of the installation path. Each entry gives the symptom, the cause and the fix. Start with [Collect diagnostics](#collect-diagnostics).

## Collect diagnostics

### Bootstrap diagnostics

When `nvkectl bootstrap site` fails, it writes diagnostics from the seed cluster to `cluster-diagnostics/`, or to the directory `--diagnostics-dir` names: one directory per namespace with object state and every pod's log (the management plane's under `nvke-controller/`), plus `all-resources.txt`, `pods-described.txt` and `helm-releases.txt`.

If the run failed after it requested the cluster, `nvkectl` keeps the seed cluster, and its NVKE API answers on the bootstrap host without authentication. List the management cluster's nodes and the step each one is on:

```bash
curl -s http://127.0.0.1:30080/api/nodes | jq -r '.nodes[] | [.name, .conditions.provisioned.status, .conditions.provisioning.message] | @tsv'
```

To use `kubectl` against the seed cluster, write its kubeconfig:

```bash
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get kubeconfig --name nvkectl-bootstrap > seed.kubeconfig
```

### Management-plane logs

Show the warnings and errors from the last hour:

```bash
kubectl logs deployment/nvke-controller -n nvke-controller --since=1h \
  | jq -cR 'fromjson? | select(.level == "WARN" or .level == "ERROR")'
```

[Observe NVKE and its clusters](observe.md) describes how to raise the log level.

### Inside the management-plane pod

The `nvke-controller` image has a busybox shell, so `kubectl exec` works for quick checks. It has `sh`, `wget`, `nc`, `ip`, `netstat`, `ps` and the usual file tools, but no `bash`, `curl`, `nslookup` or `dig`. Read the metrics endpoint from inside the pod:

```bash
kubectl exec -n nvke-controller deploy/nvke-controller -- wget -qO- http://127.0.0.1:9090/metrics
```

Busybox `wget` does not verify TLS certificates, so do not use it to check an HTTPS endpoint.

For anything the image does not carry, attach a debug container to the running pod. It shares the pod's network and, with `--target`, can see the management-plane process.

1. Find the pod name:

   ```bash
   kubectl get pod -n nvke-controller -l app.kubernetes.io/name=nvke-controller
   ```

2. Start a shell in a debug container:

   ```bash
   kubectl debug -it -n nvke-controller <POD> --profile=restricted --target=nvke-controller --image=<DEBUG_IMAGE> -- sh
   ```

   Replace `<POD>` with the name from the previous step, and `<DEBUG_IMAGE>` with an image that has the tools you need, such as `curl` and `dig`. `--profile=restricted` runs the debug container without privileges, as the pod runs; without it, a namespace that enforces the `restricted` Pod Security Standard rejects the container.

   The debug container runs as the pod's user, uid 65532, with no Linux capabilities, whatever user `<DEBUG_IMAGE>` declares. Choose an image whose tools work as an unprivileged user: `curl`, `dig` and `nslookup` do, while `tcpdump` and anything else that needs root or raw sockets does not.

The debug container stays in the pod's spec, stopped, until the pod is replaced. Starting one needs permission to patch `pods/ephemeralcontainers` in the `nvke-controller` namespace.

### Cluster and node state

1. Read the conditions of a cluster that is not `Provisioned`. The `message` of each condition usually names the cause.

   ```bash
   nvkectl get clusters <WORKLOAD_CLUSTER> -o yaml
   ```

   `<WORKLOAD_CLUSTER>` is the workload cluster's name.

2. List the cluster's nodes with the step each one is on.

   ```bash
   curl -s "http://<MANAGEMENT_ADDRESS>:30080/api/nodes?clusterName=<WORKLOAD_CLUSTER>" \
     | jq -r '.nodes[] | [.name, .conditions.pending.message, .conditions.provisioning.message, .conditions.provisioned.status] | @tsv'
   ```

   `<MANAGEMENT_ADDRESS>` is the management cluster's API virtual IP address (VIP), or the node IP of a single-node management cluster. If API authentication is on, add `-H "Authorization: Bearer <TOKEN>"` to each `curl` command that calls `/api/`, where `<TOKEN>` is a JSON Web Token (JWT) from your identity provider, as described in [Secure the NVKE API](api-authentication.md). While a step fails and retries, its message ends with the number of failed attempts and `-- last error:` followed by the error.

### Cluster API objects

NVKE creates each workload cluster's Cluster API objects in the `nvke-system` namespace of the management cluster, named after the cluster. The last command shows the logs of CAPNICo, the Cluster API infrastructure provider for NVIDIA Infra Controller (NICo).

```bash
clusterctl describe cluster <WORKLOAD_CLUSTER> -n nvke-system
```

```bash
kubectl logs deployment/controller-manager -n capnico-system --since=1h
```

### Node logs

On a node, as root:

```bash
journalctl -u nvke-agent -n 100
```

```bash
journalctl -u spire-agent -n 100
```

```bash
cloud-init status --long
```

### Versions

Record these before you report a problem.

```bash
nvkectl version
```

```bash
curl -s http://<MANAGEMENT_ADDRESS>:30080/api/version
```

```bash
kubectl get deployment nvke-controller -n nvke-controller -o jsonpath='{.spec.template.spec.containers[0].image}'
```

## Building artifacts

### nvkr does not build

- **Symptom:** `make build` in the NVIDIA Kubernetes Recipes (NVKR) repository prints `Go go<VERSION> does not match pinned go1.27.2`.
- **Fix:** NVKR builds only with the exact Go version in its `.go-version` file. Install Go 1.27.2, or let Go fetch it:

  ```bash
  GOTOOLCHAIN=go1.27.2 make build
  ```

### The bootstrap fails at clusterctl init

- **Symptom:** The bootstrap fails with `controllerdeploy: clusterctl init:` and `clusterctl dev must be on PATH`, and `nvkectl version` prints `dev`.
- **Cause:** `nvkectl` was built with `go build`. Only the Makefile stamps the version and the Cluster API version into the binary.
- **Fix:** Build with `make -C nvke/cli build`. If the message names a real version, `clusterctl` is missing from `PATH` or the host cannot reach `github.com`.

## Preparing nodes

### The SPIRE agent configuration is missing

- **Symptom:** `/etc/spire-agent/agent.conf` does not exist, and `journalctl -u nvke-spire-configure` shows `nvke-spire-configure: missing /etc/nke/spire.env`, `nvke-spire-configure: missing /etc/nke/trust-bundle.pem`, or a line ending in `is required`.
- **Cause:** The node's user data did not write the file or value named, so the SPIRE agent never starts.
- **Fix:** Correct the user data and boot the node again, as described in [Prepare nodes](prepare-nodes.md).

## Bootstrap

[Bootstrap the management cluster](bootstrap-management-cluster.md#if-the-bootstrap-fails) gives the retry steps that the entries below refer to.

### A seed cluster from an earlier run is still running

- **Symptom:** `bootstrap cluster "nvkectl-bootstrap" from an earlier run is still running`.
- **Cause:** An earlier run kept its seed cluster, which can hold the only copy of that cluster's etcd encryption key.
- **Fix:** Save every `etcd-encryption-key-*` Secret the message lists, then delete the seed cluster as in the retry steps. Keep the `KIND_EXPERIMENTAL_PROVIDER=nerdctl` prefix, or `kind` deletes nothing and the next run stops on the same message.

### The node CNI directory holds nerdctl network configs

- **Symptom:** `/etc/cni/net.d already holds nerdctl network config(s), and this node's own containerd reads that directory for pod networking`, followed by the list of `nerdctl-*.conflist` files.
- **Cause:** nerdctl ran as root on this host without `cni_netconfpath` set, so it wrote its network configs to `/etc/cni/net.d`. A node's containerd reads that directory for pod networking, and until the cluster's CNI plugin writes its own configuration, the first pods, CoreDNS among them, would get addresses on nerdctl's bridge.
- **Fix:** List the bridges the files name with `grep -h '"bridge":' /etc/cni/net.d/nerdctl-*.conflist` before you remove them. Remove the files, delete each of those bridges with `ip link delete <BRIDGE>`, and set `cni_netconfpath = "/etc/containerd-nerdctl/net.d"` in `/etc/nerdctl/nerdctl.toml` so nerdctl keeps its networks out of that directory. Then run the bootstrap again.

### An etcd encryption key file already exists

- **Symptom:** `<CLUSTER_NAME>-etcd-encryption-key.json already exists and holds the etcd encryption key of an earlier bootstrap of this cluster name`.
- **Cause:** The bootstrap never overwrites a key file.
- **Fix:** If you reset that cluster's nodes, move the file to secure storage or delete it. Otherwise pass `--encryption-key-out` with a new path.

### A node already serves the Kubernetes API

- **Symptom:** `port 6443, the Kubernetes API port, is accepting connections on: <NODE_NAME> (<NODE_IP>)`.
- **Cause:** An earlier attempt left a control plane on the node, or the site spec names a node of a running cluster.
- **Fix:** Reset kubeadm on the node as in the retry steps, or correct the site spec.

### An NVKE image is missing or cannot be pulled

- **Symptom:** `image <IMAGE> is not in the local image store`, or a `nerdctl pull` error that ends with `if this is an authentication failure, log in to the image's registry`.
- **Cause:** The `--nvke-controller-image` or `--provisioned-provider-image` reference has no registry host and was never built on this host, or the registry refused the pull.
- **Fix:** Build the images with `make -C nvke/cli images`, or pass full image references. For a refused pull, run `nerdctl login <REGISTRY_HOST>` first.

### A required tool is missing

- **Symptom:** `nerdctl is required to bootstrap a site but was not found on PATH`, or, after the seed cluster starts, an error that names `helm`, `kubectl` or `clusterctl` and ends in `executable file not found in $PATH`.
- **Cause:** The bootstrap checks only for `nerdctl` before it starts.
- **Fix:** Install rootful `nerdctl` with containerd, `kind`, `helm`, `kubectl` and `clusterctl`. Docker is not supported.

### The bootstrap times out waiting for the cluster

- **Symptom:** After `--timeout`, 30 minutes by default, the bootstrap fails with `did not report provisioned within` or `only <N> of <M> control plane nodes of cluster <ID> finished provisioning within`, and keeps the seed cluster.
- **Cause:** A node's SPIRE agent cannot attest, so `nvke-agent` never starts, or the bootstrap host cannot reach `nvke-agent`.
- **Fix:**
  1. List the nodes and their step with the `curl` command in [Bootstrap diagnostics](#bootstrap-diagnostics). `Waiting for node preflight to succeed` is followed by the last error from `nvke-agent`.
  2. On each node that is not ready, check that the SPIRE agent attested.

     ```bash
     journalctl -u spire-agent | grep 'Node attestation was successful'
     ```

     Until it attests, `nvke-agent` logs `timed out waiting for SPIFFE workload API socket`. If nothing prints, check that the node's TPM thumbprint in `hosts.csv` matches `get_tpm_pubhash` on the node, that `trust_domain` in `/etc/spire-agent/agent.conf` equals `spire.trustDomain` in the site spec, that `/etc/nke/trust-bundle.pem` holds the certificate you passed as `NVKECTL_SPIRE_CA_CERT_FILE`, and that the node reaches the bootstrap host on port 30081.
  3. From the bootstrap host, check that each node accepts connections on port 15005.

     ```bash
     nc -vz <NODE_IP> 15005
     ```

  4. Fix the cause, then follow the retry steps. For a larger cluster, also raise `--timeout`.

### cloud-init failed on a node

- **Symptom:** A node's step message contains `cloud-init error status`.
- **Cause:** The bootstrap requires cloud-init `status: done` on every node.
- **Fix:** Find the failing module with `cloud-init status --long`, correct the user data, boot the node again, and follow the retry steps.

### The etcd encryption key was not saved

- **Symptom:** `its encryption key was not saved to <PATH>`.
- **Cause:** The cluster is built and encrypted, but writing the key file failed. The seed cluster holds the key.
- **Fix:** Run the save command the message prints before you delete the seed cluster or run the bootstrap again.

### The control-plane taint was not removed

- **Symptom:** `making the control-plane nodes schedulable:`, followed by the command to finish by hand.
- **Cause:** Removing the control-plane `NoSchedule` taint failed. Until it is removed, nothing schedules on the management cluster, and `clusterctl init` in the next phase times out on cert-manager.
- **Fix:**

  ```bash
  kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule- --kubeconfig <CLUSTER_NAME>.kubeconfig
  ```

## Management-plane install

The install stops at the component that failed. Fix the cause and run the same command again, as described in [Run NVKE end to end on one machine](../get-started/local-site.md).

### The stack config is not ready

- **Symptom:** The install stops with `stack config <FILE>:` followed by one line per problem, such as `nico.endpoint: still a REPLACE_* placeholder; fill it in` or `environment: NVKECTL_NICO_TOKEN is not set`.
- **Cause:** A value in the stack config is still a `REPLACE_*` placeholder, a required key is empty, the file carries a key it must not, or one of the `NVKECTL_*` variables is not exported in the shell you run from.
- **Fix:** Edit the key or export the variable as described in [Run NVKE end to end on one machine](../get-started/local-site.md), then run the check again until the stack config passes.

### The install refuses the release

- **Symptom:** The install stops before it changes the cluster with `<ITEM>: <ROLE> <ARTIFACT> is not in the release`, for example for `controller-profiles` or `capnico-controller`.
- **Cause:** The release description does not carry an artifact that the deploy path names, for the management nodes' CPU architecture.
- **Fix:** Use a release that carries the artifact, or install without `--release` and name the images in the stack config's `images`, as described in [Run NVKE end to end on one machine](../get-started/local-site.md).

### A volume claim stays Pending

- **Symptom:** `kubectl get pvc -A` shows a claim in `Pending`, and the component that owns it times out.
- **Cause:** The management cluster has no default StorageClass. The bootstrap installs none.
- **Fix:** Install a storage provisioner, mark its StorageClass as the default, and run the install again.

### An image cannot be pulled

- **Symptom:** A component times out, and its pod is in `ImagePullBackOff`.
- **Cause:** The nodes cannot reach or authenticate to the registry that the release or the stack config's `images` names.
- **Fix:** Read the registry's error with `kubectl describe pod`, make the registry reachable from the nodes, and run the install again.

### spire-server restarts with checksums did not match

- **Symptom:** The `spire-server` pod restarts, and its log contains `checksums did not match`.
- **Cause:** The install rendered SPIRE's TPM attestor checksum for another CPU architecture than the node that runs `spire-server`. The install reads the architecture from the management nodes, so this happens when the nodes do not all report one, or when `--arch` was passed to `deploy/nvke-deploy.sh` with the wrong value. `deploy/check-site.sh` fails on nodes that report two architectures or lack the `kubernetes.io/arch` label.
- **Fix:** Give every management node the same architecture and the label, run the check, then run the install again without `--arch`.

## Infrastructure provider

### nvkectl get providers does not show CONFIGURED

- **Symptom:** The `nico` row of `nvkectl get providers` shows `NOT-INSTALLED`, `NOT-CONFIGURED`, `DEGRADED` or `UNKNOWN`. The `DETAIL` column says why.
- **Fix:** [Configure an infrastructure provider: NICo](provider-nico.md#confirm-that-capnico-is-running) gives the action for each state.

### nvkectl get providers shows CREDENTIALS other than HEALTHY

- **Symptom:** The `nico` row reads `CONFIGURED`, but `CREDENTIALS` is not `HEALTHY`. The `DETAIL` column says why.
- **Cause:** `UNSUPPORTED` is expected with the CAPNICo release the install deploys, which does not serve `NicoIdentity`. With a release that does, `NO-IDENTITY` means no `NicoIdentity` names `nico-credentials`, and `FAILING` or `UNKNOWN` carries the reason from CAPNICo's last check.
- **Fix:** [Infrastructure providers](https://github.com/dsx-ai-factory/nvke/blob/main/cli/README.md#infrastructure-providers) explains each value. For `FAILING`, correct the credentials as described in [Configure an infrastructure provider: NICo](provider-nico.md#manage-the-nico-credentials).

### Machines are not delivered

- **Symptom:** A node's `pending` message stays `Waiting for infrastructure allocation`, or its step stays `Waiting for CAPI machine to be ready`. `clusterctl describe cluster <WORKLOAD_CLUSTER> -n nvke-system` may show a machine with the reason `InstanceTypeUnavailable`.
- **Cause:** The NICo instance type has no allocation for your organization, or no free machine.
- **Fix:** Add an allocation in NICo, or use an instance type with free capacity. The control plane can list fallback types in `instanceTypePreference`, as in [Create a workload cluster](../workload-clusters/create.md).

### CAPNICo logs NICo API errors

- **Symptom:** Nodes stay on `Waiting for CAPI machine to be ready`, and the CAPNICo logs show failed calls to the NICo API.
- **Cause:** The endpoint, organization ID, token or API name in the `nico-credentials` Secret is wrong. `CONFIGURED` does not test them.
- **Fix:** Correct the credentials as described in [Configure an infrastructure provider: NICo](provider-nico.md#manage-the-nico-credentials).

## API access and authentication

[Secure the NVKE API](api-authentication.md) describes each of these cases.

| Symptom | Cause | Fix |
|---|---|---|
| `nvkectl` fails with `minting token for nke-clients/nke-client`. | `nvkectl` uses the management cluster's admin kubeconfig after authentication is configured. | Use an OpenID Connect (OIDC) kubeconfig, as in [With nvkectl](api-authentication.md#with-nvkectl). |
| `401`, or `403` with `Not admitted: the authenticated identity matches no admission rule.` | Enforcing mode rejected the token or the caller. | Search the management-plane log for `rejected request` in enforcing mode, or `authentication would have rejected this request` in permissive mode, then see [Find callers that enforcing mode would reject](api-authentication.md#find-callers-that-enforcing-mode-would-reject). |
| The `nvke-controller` pod does not become ready, and logs `Failed to set up API authentication`. | In enforcing mode NVKE cannot fetch an issuer's signing keys at startup. | Fix the issuer setting the error names, or return to permissive mode until the log shows `fetched issuer signing keys` for each issuer. |
| A change to `apiAuth.config` has no effect, or authentication is off after you re-run the install. | NVKE reads the configuration only at startup, and the install's values turn authentication off. | See [Switch to enforcing mode](api-authentication.md#switch-to-enforcing-mode). |

## Creating clusters

### nvkectl create cluster returns an error

- **Symptom:** `nvkectl create cluster` prints `creating cluster:` with a `400` and a message such as `controlPlaneConfig.nico.release is required for a nico cluster control plane`.
- **Cause:** The cluster spec is invalid. Nothing was created.
- **Fix:** Correct the field the message names. [Create a workload cluster](../workload-clusters/create.md#create-the-cluster) lists the common causes.

### The cluster stays in Provisioning

- **Symptom:** `nvkectl get clusters` shows `Provisioning` long after you created the cluster.
- **Cause:** A control-plane node has not finished provisioning.
- **Fix:** List the cluster's nodes with the command in [Cluster and node state](#cluster-and-node-state), and act on the step the stuck node is on:

  | Node message | What is wrong | Next check |
  |---|---|---|
  | `Waiting for infrastructure allocation` (`pending`) or `Waiting for CAPI machine to be ready` | NICo has not delivered or booted a machine. | [Machines are not delivered](#machines-are-not-delivered), then the CAPNICo logs. |
  | `Waiting for node preflight to succeed` | The machine booted, but `nvke-agent` is unreachable or cloud-init has not finished. | The `-- last error:` text, then the node logs. |
  | `Workflow failed:` | Provisioning of the node stopped with the error that follows. | The error text and the management-plane log. |

### Nodes do not progress because of the release

- **Symptom:** Nodes do not progress, and the management-plane log contains `resolving release "<NAME>"`.
- **Cause:** The control plane must name an existing cluster release and each node pool an existing node pool release.
- **Fix:** Check the names with `nvkectl get clusterreleases` and `nvkectl get nodepoolreleases`. Apply the missing releases as in [Run NVKE end to end on one machine](../get-started/local-site.md), or delete the cluster and create it again with names that exist.

### The cluster shows ProvisioningFailed

- **Symptom:** `STATUS` is `ProvisioningFailed`.
- **Cause:** Provisioning stopped. The `provisioning` condition's message, after `Workflow failed:`, gives the cause.
- **Fix:** Fix the cause, then delete the cluster and create it again.

### A cluster patch fails

- **Symptom:** `nvkectl patch cluster` fails with `409` and `cluster update rejected: <OPERATION> in progress since <TIME>`, or `STATUS` becomes `UpdateFailed`.
- **Cause:** Only one operation runs on a cluster at a time. `UpdateFailed` means the last day-2 operation failed; its condition message gives the cause.
- **Fix:** For `409`, wait for `Provisioned`. Then, or for `UpdateFailed`, send the same patch again, as in [Create a workload cluster](../workload-clusters/create.md#change-cluster-settings-after-creation).

### The cluster stays in Deleting

- **Symptom:** The cluster stays `Deleting`, and its `deleting` condition message starts with `Workflow failed:`.
- **Cause:** Teardown failed.
- **Fix:** [Report the problem](#get-help) with the cluster's YAML, the management-plane log and the Cluster API objects. Do not delete the cluster's objects in `nvke-system` by hand.

## Node pools and nodes

### A node was replaced after two hours

- **Symptom:** A provisioning worker node is replaced, and `nvke_controller_node_provisioning_giveups_total` increases.
- **Cause:** The node provisioned for longer than the timeout, 2 hours by default.
- **Fix:** Fix what the replacement node's step message waits on. To keep a stuck node for diagnosis, set `provisioningMonitorDisabled` on the pool, as described in [Manage node pools](../workload-clusters/node-pools.md#control-automatic-node-replacement).

## Workload cluster access

### nvkectl get kubeconfig fails

- **Symptom:** `admin kubeconfig secret nvke-system/<WORKLOAD_CLUSTER>-admin-kubeconfig not found`, or `not allowed to read secret nvke-system/<WORKLOAD_CLUSTER>-admin-kubeconfig`.
- **Cause:** For `not found`, the name is wrong or the cluster is not provisioned yet. For `not allowed`, the kubeconfig set with `nvkectl config set-site` lacks `get` on Secrets in `nvke-system`.
- **Fix:** Check the name with `nvkectl get clusters` and wait for `Provisioned`, or use a kubeconfig with that permission.
- **Note:** A management plane installed before the management namespace became configurable publishes into `nke-system`, not `nvke-system`. `nvkectl get kubeconfig` tries both, so the message naming only one means it found the Secret in neither. Pass `--namespace` if the site pins a name of its own.

### kubectl times out against the workload cluster

- **Symptom:** `kubectl --kubeconfig <WORKLOAD_CLUSTER>.kubeconfig get nodes` times out.
- **Cause:** Your machine cannot reach the API address in the kubeconfig on port 6443.
- **Fix:** See [How clients reach the Kubernetes API](../workload-clusters/access.md#how-clients-reach-the-kubernetes-api).

## Get help

Report bugs and documentation gaps in [GitHub Issues](https://github.com/dsx-ai-factory/nvke/issues), after searching the existing issues. Review every file and log before you attach it, and remove addresses, host names, tokens, passwords and other values specific to your site. Include:

- The output of the commands in [Versions](#versions).
- The command you ran and its full error.
- The diagnostics from [Collect diagnostics](#collect-diagnostics) that relate to the failure.
- What you changed from the steps in this documentation.
