# Troubleshooting a Site Bootstrap

What to do when `nvkectl bootstrap site` fails. This page covers where the
evidence is, how to read the error, the failures operators meet most often
with their fixes, and when it is safe to run the command again.

For failures on the nodes themselves, such as `nke-agent` not starting, see
[Pre-Provisioned Nodes](../../docs/site-bootstrap/pre-provisioned-nodes.md#troubleshooting).

## Where the evidence is

A bootstrap leaves evidence in up to four places. Which ones exist depends on
how far it got.

### The terminal output

`nvkectl` logs to standard error, one line per action, and prints the final
error last. Keep the output: redirect it to a file when you run the command,
or use `--json-logs` for output a tool can parse.

```bash
REGISTRY_API_KEY=... nvkectl bootstrap site -f sitespec.yaml 2>&1 | tee bootstrap.log
```

Every line names the component that wrote it. `bootstrap-site` is the command
itself, `bootstrapengine` drives the Kind cluster, `cwedeploy` installs the
controller and its charts. `-v` adds debug detail.

### The diagnostics directory

When a bootstrap fails after the Kind cluster exists, `nvkectl` dumps that
cluster's state before anything is torn down. The default location is
`cluster-diagnostics/` in the working directory; `--diagnostics-dir` or
`NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR` changes it. The log line
`bootstrap failed; collecting cluster diagnostics` names the directory used.

| Path | Contents |
|---|---|
| `<namespace>/<pod>/logs.txt` | Every pod's logs, from `kubectl cluster-info dump` |
| `<namespace>/*.json` | Every object in every namespace |
| `all-resources.txt` | `kubectl get all,events -A -o wide`: one view of what ran and what did not |
| `pods-described.txt` | `kubectl describe pods -A`: image pulls, scheduling, restarts, probe failures |
| `helm-releases.txt` | Which Helm releases landed and their status |

Start with `all-resources.txt` to see which pods never became ready, then open
that pod's `logs.txt` and its section in `pods-described.txt`. The namespaces
that matter are `postgres`, `temporal`, `spire-server`, `capi-system`,
`provisioned-system` and `nke-controller`.

### The retained bootstrap cluster

The Kind cluster is deleted on every exit path except two. It is kept when you
pass `--keep-bootstrap-cluster`, and it is kept automatically when the failure
happens after the site cluster was created, because it then holds the
workflows and credentials needed to recover. In both cases the log line names
the kubeconfig path:

```text
level=WARN msg="bootstrap failed after site cluster creation; retaining the bootstrap kind cluster for recovery" cluster=nvkectl-bootstrap kubeconfig=/tmp/nvkectl-bootstrap-kubeconfig-.../config
```

Use that kubeconfig to look at the live state, which is more than the dump
captures:

```bash
export KUBECONFIG=/tmp/nvkectl-bootstrap-kubeconfig-.../config
kubectl get pods -A
kubectl -n nke-controller logs deploy/nke-controller --tail=500
```

The controller's logs are the record of what it did to each node. Filter
them by the cluster or node name from your site spec.

Read [Bootstrap refuses to start](#bootstrap-refuses-to-start) and
[The encryption key was not saved](#the-encryption-key-was-not-saved) before
deleting a retained cluster. It may hold the only copy of a key.

When you are done, delete the cluster. The provider prefix is required. Without
it `kind` looks in Docker, finds nothing, and exits successfully having deleted
nothing.

```bash
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nvkectl-bootstrap
```

### The nodes

Once the site cluster has been created, the work happens on your hosts through
`nke-agent`. On each control plane node:

```bash
sudo journalctl -u nke-agent --since "1 hour ago"
sudo cat /var/log/nke-agent-audit.log      # one line per request the agent received
ls /etc/kubernetes/manifests               # present means kubeadm ran here
sudo journalctl -u kubelet --since "1 hour ago"
```

## How to read the error

The final `Error:` line is a chain. Each segment is added by the layer that
caught the failure, so it reads from the outermost phase on the left to the
root cause on the right. Take a generic chart download failure:

```text
Error: deploying cwe: cwedeploy: installing SPIRE CRDs: helm upgrade --install spire-crds spire-crds --repo https://spiffe.github.io/helm-charts-hardened/ --namespace spire-server --create-namespace --version 0.6.1 --wait --timeout 5m0s: exit status 1
Error: failed to fetch https://spiffe.github.io/helm-charts-hardened/releases/download/spire-crds-0.6.1/spire-crds-0.6.1.tgz : 500 Internal Server Error
```

- `deploying cwe` is the phase: installing the controller and its dependencies onto
  the Kind cluster.
- `installing SPIRE CRDs` is the step within it.
- `helm upgrade --install ...: exit status 1` is the command that failed, with
  its full arguments, so you can run it by hand.
- The lines that follow are the command's own output, and hold the cause: the
  chart repository returned a server error.

So this bootstrap failed before any of your nodes were touched, because a
public chart repository was unavailable. The fix is to retry, or to point at a
mirror. Nothing needs cleaning up on the nodes.

The phase tells you what state you are in. In order:

| Phase | Log markers | State of the system when it fails |
|---|---|---|
| Preflight | `loaded site spec`, then checks that run before anything is created | Nothing created. Safe to fix and re-run. |
| Bootstrap cluster | `creating ephemeral bootstrap kind cluster` to `bootstrap kind cluster ready`, then `loading image into bootstrap kind cluster` | A Kind cluster may exist. It is deleted on failure unless you kept it. |
| Controller deploy | `installing postgres` through `cwe deployment healthy` | Kind cluster exists with partial installs. Deleted on failure unless kept. Nodes untouched. |
| Site cluster | `creating the site cluster` through `all control plane nodes joined` | Nodes are being changed. The Kind cluster is retained. Read the recovery section before re-running. |
| Finish | `wrote the cluster kubeconfig`, `wrote the etcd encryption key`, `wrote SPIRE bootstrap resources` | The cluster is built. A failure here is about saving its outputs, not about the cluster. |

## Common failures

Each entry gives the text to look for, what it means, and what to do.

### Bootstrap refuses to start

**`nerdctl is required to bootstrap a site but was not found on PATH`**

The bootstrap cluster runs on nerdctl and containerd. Docker is not
supported. Install both on the machine running `nvkectl`; see
[Installation](../../docs/site-bootstrap/installation.md#additional-prerequisites-for-bootstrap).

**`image <image> is not in the local image store ... and names no registry to pull it from`**

Bootstrap side-loads two images that default to local builds,
`nke-controller:latest` and `cluster-api-provider-provisioned:latest`, and
neither is in containerd's image store. Build them with `make -C cli images`,
or name images your host can pull, with `--controller-image` and
`--provisioned-provider-image` or `NVKECTL_CONTROLLER_IMAGE` and
`NVKECTL_CAPI_PROVISIONED_IMAGE`. A reference with a registry, such as
`docker.io/<org>/<name>`, is pulled; if the pull is refused, see
[Bootstrap cluster](#bootstrap-cluster).

**`bootstrap cluster "nvkectl-bootstrap" from an earlier run is still running`**

A previous bootstrap left its Kind cluster behind, either because you passed
`--keep-bootstrap-cluster` or because that run failed after creating the site
cluster. If that earlier run built an encrypted site cluster and did not save
its key, the key is in this cluster. Save it before deleting anything:

```bash
umask 077
export KIND_EXPERIMENTAL_PROVIDER=nerdctl
kind get kubeconfig --name nvkectl-bootstrap > bootstrap.kubeconfig
kubectl --kubeconfig bootstrap.kubeconfig -n nke-controller get secrets | grep etcd-encryption-key-
# for each match:
kubectl --kubeconfig bootstrap.kubeconfig -n nke-controller get secret <name> -o json > <name>.json
kind delete cluster --name nvkectl-bootstrap
```

Then re-run. Bootstrap never deletes or reuses an existing cluster itself.

**`port 6443, the Kubernetes API port, is accepting connections on: <node> (<ip>)`**

Something on a site spec host already answers on the Kubernetes API port. The
check is a plain TCP connect, so it cannot tell a control plane from anything
else, but a control plane left by an earlier attempt is the usual cause.
Bootstrap does not provision over one, because encryption is configured
during `kubeadm init` and a second run cannot adopt a key the host already
holds. On each listed node, first check whether it is a control plane:

```bash
ls /etc/kubernetes/manifests    # kube-apiserver.yaml here means a control plane
```

Reset a node only once you have confirmed it was left by your failed bootstrap:
it is in your site spec, and `/var/log/nke-agent-audit.log` on it shows requests
from your earlier attempt. On a control plane node `kubeadm reset` also removes
its local etcd member, which would disrupt a different cluster running there.

```bash
sudo kubeadm reset -f
```

If a host answering on the port is not from your bootstrap, or nothing on it is
a control plane, do not reset it. Check the node list in the site spec.

**`<path> already exists and holds the etcd encryption key of an earlier bootstrap`**

The key file for this cluster name is already in the working directory.
Bootstrap will not overwrite a key. Move the old file away, or choose another
path with `--encryption-key-out`.

### Bootstrap cluster

**`kind create cluster nvkectl-bootstrap: exit status 1`**

The output after the error is kind's own. Common causes: containerd is not
running or the current user cannot reach its socket; the kindest/node image
could not be pulled, in which case set `NVKECTL_BOOTSTRAP_DOCKERHUB_MIRROR` to a
registry mirror; or a host port bootstrap needs is in use. Fix the cause and
re-run. If a half-created cluster remains, delete it with the command above.

**`nerdctl pull <image>: exit status 1`** with an authentication error

The token used in `nerdctl login` for the image's registry is incorrect, or the
image reference is wrong. Use the correct token for that registry. The login is
stored per user, so it must be in the environment bootstrap runs in: the root
login shell (`sudo -n -i`), not plain `sudo`. Confirm it works there before
re-running bootstrap:

```bash
sudo -n -i nerdctl pull <image>
```

**`kind load image-archive ...: exit status 1`** or **`want two sha256 digests`**

The image exists locally but could not be exported into the Kind cluster. Pull
it again with the platform of the bootstrap host and re-run. A multi-platform
image that was only partially pulled is the usual cause.

### Controller deploy

**`installing postgres` / `temporal` / `SPIRE CRDs` / `SPIRE` / `cwe`: `helm ... exit status 1`**

The chain names the release. The helm output below the error names the cause.
Three patterns account for most of them:

- *Chart fetch failed*, such as `failed to fetch ... 500` or a timeout. The
  chart repository is unreachable from the bootstrap host. Retry, or point at
  a mirror or local copy with `NVKECTL_CONTROLLER_TEMPORAL_CHART`,
  `NVKECTL_CONTROLLER_SPIRE_CHART` and `NVKECTL_CONTROLLER_SPIRE_CRDS_CHART`. Nodes are
  untouched.
- *Image pull failed inside the cluster*, visible in `pods-described.txt` as
  `ErrImagePull` or `ImagePullBackOff`. The Kind node cannot reach the image
  registry. Set `NVKECTL_BOOTSTRAP_DOCKERHUB_MIRROR` for Docker Hub images, and
  check that the controller image was loaded rather than pulled.
- *`--wait` timed out* with pods still `Pending` or `CrashLoopBackOff`. Open
  the pod's `logs.txt`. A Pending pod is usually resources on the bootstrap
  host. A crash-looping controller pod whose log ends in a database or Temporal
  connection error means Postgres or Temporal is not ready yet, or their
  shared password did not reach one side; a schema migration error names the
  migration that failed.

`NVKECTL_CONTROLLER_HELM_TIMEOUT` raises the per-release wait from its default of
five minutes on a slow network.

**`cwe did not report healthy on /readyz within 5m0s: last error=... last response=...`**

The controller pod is running but its readiness check is failing. The response
names which dependency: `database` means Postgres is not reachable with the
generated password, `temporal` means the Temporal frontend is not up yet or
its schema jobs failed. Open `temporal/*/logs.txt` and `postgres/*/logs.txt`
in the diagnostics directory. `NVKECTL_CONTROLLER_READY_TIMEOUT` raises the wait.

**`waiting for the published cwe api: http://127.0.0.1:30080/readyz did not become ready`**

The controller is healthy inside the cluster but not reachable from the host. The
Kind host port mapping or the NodePort Service did not come up. Check that
nothing else on the host holds port 30080, or change it with
`NVKECTL_CONTROLLER_PORT`.

### Site cluster

From here on, the controller is driving `kubeadm` on your nodes through
`nke-agent`. The bootstrap cluster is retained on failure, and the controller's
logs in it are the primary record.

**`creating cluster "<name>": 400 Bad Request: <message>`**

The controller rejected the cluster request before doing anything. The message is
the controller's validation error. The most common is a non-default pod or
service CIDR with a CNI that does not support it. Fix the site spec and
re-run; nothing was created.

**`cluster <id> did not report provisioned within 30m0s (last status="Provisioning", last error=<nil>)`**

The first control plane node never finished `kubeadm init`. In order:

1. In the controller logs, search for the cluster name and read the last activity
   it ran for the first node.
2. On that node, check `nke-agent` is running and its journal shows the controller
   connecting. No connection at all means the controller cannot reach the node's
   agent port: check the `nodeAgentPort` in the site spec and any firewall.
3. A refused connection happens before any TLS handshake, so it is not an
   attestation failure. Check that `nke-agent` is listening on the agent port
   and that nothing blocks the route to it. With SPIFFE enabled, a connection
   that is reset or fails during the TLS handshake means attestation failed.
   Check that the node's TPM endorsement key thumbprint in the site spec
   matches the hardware, and read `spire-server/*/logs.txt` for the rejected
   attestation.
4. If the agent ran `kubeadm init` and it failed, the kubelet journal on the
   node has the reason. Missing container images on the node and a
   misconfigured container runtime are the usual causes; see
   [Pre-Provisioned Nodes](../../docs/site-bootstrap/pre-provisioned-nodes.md#step-1-prepare-all-nodes).

`--timeout` or `NVKECTL_BOOTSTRAP_CLUSTER_TIMEOUT` raises the budget for slow
networks. The same budget also covers the join wait below.

**`only N of M control plane nodes of cluster <id> finished provisioning within ... (outstanding: <names>)`**

The first node initialised and the rest did not join. The outstanding names
tell you which. Apply the same checks as above to those nodes. A node that
joins the API server but never shows Ready is usually the CNI; a node whose
join times out with `waiting for the kubelet to boot up the control plane`
is usually missing the encryption configuration or cannot reach the first
node's API endpoint.

**Bootstrap reports the cluster provisioned, but `kubectl` through the control-plane VIP times out**

The control plane is up on the nodes but the virtual IP is not answering.
This shows up after bootstrap, when the written kubeconfig points at
`cluster.controlPlaneVIP` and the first `kubectl get nodes` hangs. Check that
the VIP is in the same subnet as the nodes and is not already in use, and that
the nodes can announce it on their network. The `kube-vip` pod logs under
`/var/log/pods` on the first control plane node show the leader election.
Reaching the API server directly on that node's IP confirms the cluster
itself is healthy.

**`cluster provisioning failed: cluster "<name>" reports updateFailed: <message>`**

The controller gave up on the cluster. The message is the controller's own summary;
its logs hold the detail. This is terminal for that attempt: follow the
recovery section, then re-run.

**`cluster provisioning failed: cluster "<name>" started deleting while it was being provisioned`**

Something deleted the cluster through the controller's API while bootstrap was
waiting. Check for another operator or automation using the same bootstrap
cluster.

### Finish

**`tenant kubeconfig for "<name>" was not published within 2m0s`**

The cluster is built but the controller has not yet written its admin kubeconfig
Secret. This is a timing problem on a slow control plane. The bootstrap
cluster is retained. Fetch the kubeconfig yourself:

```bash
kubectl --kubeconfig <bootstrap-kubeconfig> -n nke-system get secret <name>-kubeconfig -o jsonpath='{.data.value}' | base64 -d > <name>.kubeconfig
chmod 600 <name>.kubeconfig
```

Bootstrap saves the encryption key only after it has the kubeconfig, so it has
not saved it yet. If the site spec enables etcd encryption, save the
`etcd-encryption-key-*` Secret from `nke-controller` first, using the commands
in [Bootstrap refuses to start](#bootstrap-refuses-to-start). If you cannot
save it, keep the bootstrap cluster. Then delete the Kind cluster.

### The encryption key was not saved

**`cluster "<name>" is up and its etcd data is encrypted, but its encryption key was not saved to <path>`**

The cluster is complete and its kubeconfig is written; only saving the key to
disk failed. The bootstrap cluster is kept because it holds the key. Do not
delete it and do not re-run bootstrap until the key is saved. Run the command
the error prints, which writes the Secret exactly as the bootstrap cluster
holds it, owner-readable only. Then delete the Kind cluster.

What to do with the saved key, and how to use it when restoring etcd, is in
[Pre-Provisioned Nodes](../../docs/site-bootstrap/pre-provisioned-nodes.md#troubleshooting).

## When it is safe to run bootstrap again

| Where it failed | Re-run as is? | First |
|---|---|---|
| Preflight | Yes | Fix what the message names |
| Bootstrap cluster or controller deploy | Yes | Delete the Kind cluster if one was left behind |
| Site cluster, before any node initialised | Yes | Delete the retained Kind cluster |
| Site cluster, after a node initialised | No | Save any key Secret from the retained Kind cluster, `kubeadm reset -f` on every node that has `/etc/kubernetes/manifests`, delete the Kind cluster, then re-run |
| Finish | No | The cluster exists. Recover the missing output by hand as described above, then delete the Kind cluster |

The preflight checks enforce the third and fourth rows: bootstrap refuses to
start while the old Kind cluster exists or while a node answers on the API
port, so an unsafe re-run stops before it does anything.

## See also

- [Pre-Provisioned Nodes](../../docs/site-bootstrap/pre-provisioned-nodes.md): preparing hosts, and
  node-side troubleshooting
- [Configuration](../../docs/site-bootstrap/configuration.md): the site spec fields the checks above
  refer to
- [Your First Cluster](../../docs/first-cluster.md#4-bootstrap): the full bootstrap
  walkthrough
