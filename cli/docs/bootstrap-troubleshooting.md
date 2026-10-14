# Troubleshooting a Site Bootstrap

When `nvkectl bootstrap site` fails, start with the final error and the
saved diagnostics. Before retrying, check
[When it is safe to run bootstrap again](#when-it-is-safe-to-run-bootstrap-again):
a failed command may have left a working or partially provisioned site cluster.

## Where the evidence is

### The terminal output

Keep the command output and the `Trace ID:` printed with it. Add `-v` for
debug detail or `--json-logs` for structured logs.

```bash
nvkectl bootstrap site -f sitespec.yaml 2>&1 | tee bootstrap.log
```

#### The trace ID

Each attempt has its own trace ID. Include it when reporting a failure. It
also finds the controller's records of the site cluster request: the API call,
the cluster and node pool workflows, and their steps. Bootstrap does not
configure span export for the controller it installs, so these logs are the
record.

Search the controller logs for it, in the diagnostics directory or in a
[retained bootstrap cluster](#the-retained-bootstrap-cluster):

```bash
grep -F '<trace-id>' bootstrap.log                                          # nvkectl's log
grep -rF '<trace-id>' cluster-diagnostics/nke-controller/                   # diagnostics directory
kubectl -n nke-controller logs deploy/nke-controller | grep -F '<trace-id>' # retained cluster
```

This works with text and JSON logs. Each node is provisioned by a workflow with
its own trace ID, so the run's ID does not reach a node's steps. To follow a
node, run the same searches in two steps:

1. Search for the cluster ID, which is in the `cluster created` log line and in
   most errors, and `ProvisionNode workflow` (`JoinNode workflow` for a
   control-plane join). There is one line per node, with its `node_name` and
   `workflow_id`.
2. Search for that `workflow_id`. The matches are the node's workflow and
   activity steps, including the one that failed.

### The diagnostics directory

Bootstrap attempts to collect diagnostics before cleaning up a failed
bootstrap cluster. The default is `cluster-diagnostics/`; change it with
`--diagnostics-dir` or `NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR`. Collection may be
incomplete if the cluster is unreachable. A retry writes into the same
directory, replacing files of the same name, so move it first or pass a new
`--diagnostics-dir`.

No diagnostics are collected when the failure comes after the site cluster and
its outputs are saved, as in
[making the control-plane nodes schedulable](#finish). The bootstrap cluster is
retained, so read its controller logs and the nodes' journals instead.

| Path | Use it to check |
|---|---|
| `bootstrap-cluster-create-failure.txt` | Kind creation failures: container output, kubelet journal and static pod logs |
| `all-resources.txt` | Pod readiness and events across namespaces |
| `pods-described.txt` | Image pulls, scheduling, restarts and probe failures |
| `<namespace>/<pod>/logs.txt` | Pod logs |
| `<namespace>/*.json` | Resource state captured by `kubectl cluster-info dump` |
| `helm-releases.txt` | Installed Helm releases and their status |

For a deployment failure, start with `all-resources.txt`, then check the
failing pod in `pods-described.txt` and its `logs.txt`. Key namespaces are
`postgres`, `temporal`, `spire-server`, `capi-system`, `provisioned-system`
and `nke-controller`.

### The retained bootstrap cluster

Bootstrap normally deletes its Kind cluster. It keeps it when you pass
`--keep-bootstrap-cluster` or when a failure occurs after the controller
accepts the site cluster request and before the outputs are saved. A failed
cleanup can also leave it behind. The logs report retention and, when
available, the kubeconfig path.

Use that kubeconfig to inspect the live cluster:

```bash
export KUBECONFIG=<bootstrap-kubeconfig>
kubectl get pods -A
kubectl -n nke-controller logs deploy/nke-controller --tail=500
```

To pick out one attempt or one node, see [The trace ID](#the-trace-id).

If no kubeconfig path was printed, retrieve it:

```bash
# Creates a private file; noclobber refuses to overwrite an existing one.
( umask 077; set -o noclobber
  KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get kubeconfig --name nvkectl-bootstrap > bootstrap.kubeconfig )
```

Keep this cluster until recovery is complete: it may hold active workflows
and the only saved copy of the etcd encryption key. See
[The encryption key was not saved](#the-encryption-key-was-not-saved).
When it is safe to delete it, use the same provider that created it:

```bash
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind delete cluster --name nvkectl-bootstrap
```

### The nodes

Once the controller accepts the site cluster request, inspect each affected
control-plane node:

```bash
sudo journalctl -u nvke-agent --since "1 hour ago"
sudo cat /var/log/nvke-agent-audit.log
sudo journalctl -u kubelet --since "1 hour ago"
sudo ls /etc/kubernetes/manifests
```

For agent setup and node prerequisites, see
[Pre-Provisioned Nodes](../../docs/site-bootstrap/pre-provisioned-nodes.md#troubleshooting).

## How to read the error

The final `Error:` identifies the phase and the failed step. If it names a
command such as `helm ...: exit status 1`, read the command output that
follows for the cause, such as a chart download error or a timeout.

| Failure phase | Where to look first | Before retrying |
|---|---|---|
| Preflight | The error and the site spec; nothing has been created | Fix the reported problem |
| Kind creation | `bootstrap-cluster-create-failure.txt` | Fix the cause and delete any leftover bootstrap cluster |
| Controller deploy | Pod events and logs in the diagnostics directory; site nodes are untouched | Fix the cause and delete any leftover bootstrap cluster |
| Site provisioning | Controller logs, then the affected node's agent and kubelet journals | Inspect the site and the retained bootstrap cluster; abandon the attempt only as described [below](#when-it-is-safe-to-run-bootstrap-again) |
| Saving outputs or making nodes schedulable | The error's recovery instructions; the site cluster already exists | Recover the missing outputs or run the final command; do not bootstrap the existing cluster again |

## Common failures

### Bootstrap refuses to start

**`nerdctl is required to bootstrap a site but was not found on PATH`**

The bootstrap cluster runs on nerdctl and containerd. Docker is not
supported. Install both on the machine running `nvkectl`; see
[Installation](../../docs/site-bootstrap/installation.md#additional-prerequisites-for-bootstrap).

**`image <image> is not in the local image store ... and names no registry to pull it from`**

A required image is missing from containerd's image store. Supply a pullable
registry reference with `--controller-image` or `--provisioned-provider-image`
(or `NVKECTL_CONTROLLER_IMAGE` / `NVKECTL_CAPI_PROVISIONED_IMAGE`). If building
from source, `make -C cli images` builds the default local images.

**`bootstrap cluster "nvkectl-bootstrap" from an earlier run is still running`**

Inspect the retained cluster before deleting it. If the earlier run created
a site cluster, follow the [recovery guidance](#when-it-is-safe-to-run-bootstrap-again)
first. Bootstrap does not reuse an existing Kind cluster.

**`port 6443, the Kubernetes API port, is accepting connections on: <node> (<ip>)`**

A host in the site spec already has something listening on the Kubernetes API
port. Confirm the host belongs to this bootstrap and inspect
`/etc/kubernetes/manifests` and `/var/log/nvke-agent-audit.log` on it. Do not
reset a host belonging to another cluster. For remnants of a failed attempt,
follow the [recovery guidance](#when-it-is-safe-to-run-bootstrap-again).

**`<path> already exists and holds the etcd encryption key of an earlier bootstrap`**

The selected key output path already exists. Preserve the old key, then
choose another path with `--encryption-key-out` or move the file before
bootstrapping a new cluster.

### Bootstrap cluster

**`kind create cluster nvkectl-bootstrap: exit status 1`**

Read the command output and `bootstrap-cluster-create-failure.txt`. Check
containerd is running, your user can reach its socket, and required host
ports are free. For Docker Hub pull failures, configure
`NVKECTL_BOOTSTRAP_DOCKERHUB_MIRROR`. Fix the cause and remove any partial
bootstrap cluster before retrying.

**`nerdctl pull <image>: exit status 1`** with an authentication error

Check the image reference and registry credentials. Registry login is per
user: authenticate and test the pull in the same environment that runs
bootstrap. If bootstrap runs in a root login shell, test there:

```bash
sudo -n -i nerdctl pull <image>
```

**`kind load image-archive ...: exit status 1`** or **`want two sha256 digests`**

The image exists locally but could not be exported into the Kind cluster. Pull
it again with the platform of the bootstrap host and re-run. A multi-platform
image that was only partially pulled is the usual cause.

### Controller deploy

**`installing postgres` / `temporal` / `SPIRE CRDs` / `SPIRE` / `cwe`: `helm ... exit status 1`**

The error names the release; the Helm output gives the cause:

- *Chart fetch failed*, such as `failed to fetch ... 500` or a timeout. The
  chart repository is unreachable from the bootstrap host. Retry, or point at
  a mirror or local copy with `NVKECTL_CONTROLLER_TEMPORAL_CHART`,
  `NVKECTL_CONTROLLER_SPIRE_CHART` and `NVKECTL_CONTROLLER_SPIRE_CRDS_CHART`. Nodes are
  untouched.
- *Image pull failed inside the cluster*, visible as `ErrImagePull` or
  `ImagePullBackOff`. Check the pod events for an incorrect image reference,
  denied access or a network failure. For Docker Hub connectivity problems,
  use `NVKECTL_BOOTSTRAP_DOCKERHUB_MIRROR`; also check that the controller
  image was loaded into Kind.
- *`--wait` timed out* with pods still `Pending` or `CrashLoopBackOff`. Open
  pod events in `pods-described.txt` for scheduling failures, and `logs.txt`
  for crashes. For database or Temporal connection errors, check the
  dependency's readiness and credentials; for migration errors, inspect the
  named migration or schema job.

`NVKECTL_CONTROLLER_HELM_TIMEOUT` raises the per-release wait from its default of
five minutes on a slow network.

**`the controller did not report healthy on <url> within 5m0s: last error=... last response=...`**

Check the controller pod's state and the last error. If `/readyz` responded,
it identifies the failing dependency: inspect `postgres/*/logs.txt` for
`database`, or `temporal/*/logs.txt` for `temporal`. Check credentials,
connectivity and schema jobs. `NVKECTL_CONTROLLER_READY_TIMEOUT` raises the wait.

**`waiting for the published cwe api: http://127.0.0.1:30080/readyz did not become ready`**

The controller passed its in-cluster check, but the host check failed. Verify
it is still healthy, then inspect the Kind port mapping and NodePort Service.
Check for a conflict on port 30080, or change it with `NVKECTL_CONTROLLER_PORT`.

### Site cluster

After the create request is accepted, the controller may be changing your
nodes. Keep the retained bootstrap cluster while investigating.

**`creating cluster "<name>": 400 Bad Request: <message>`**

The controller rejected the request. Fix the validation error in the site
spec, then retry; this request did not create a site cluster.

**`cluster <id> did not report provisioned within 30m0s (last status="Provisioning", last error=<nil>)`**

The initial control plane has not reported ready. This can fail before,
during or after `kubeadm init`, including CNI setup.

1. Find the last failure in the controller logs, following the node as
   described under [The trace ID](#the-trace-id).
2. On the affected node, check `nvke-agent` is running and listening on the
   site spec's `nodeAgentPort`. Check routing and firewalls from the controller.
3. For TLS errors, confirm the controller and agents agree on mTLS settings.
   Check SPIRE and agent logs; for attestation errors, verify the site's TPM
   endorsement key thumbprint against the node.
4. If `kubeadm` ran, inspect the kubelet journal and static pod logs for image,
   runtime or configuration errors. See
   [node preparation](../../docs/site-bootstrap/pre-provisioned-nodes.md#step-1-prepare-all-nodes).

`--timeout` or `NVKECTL_BOOTSTRAP_CLUSTER_TIMEOUT` raises the budget for slow
networks. The same budget also covers the join wait below.

**`only N of M control plane nodes of cluster <id> finished provisioning within ... (outstanding: <names>)`**

The initial control plane is up, but the listed nodes have not finished
provisioning. They may still be joining or running post-join steps. Apply the
checks above to each outstanding node. For a node that joined but is not Ready,
check CNI health. For a control-plane join timeout, check its API connectivity
and kubelet logs, including errors about missing encryption configuration.

**Bootstrap reports the cluster provisioned, but `kubectl` through the control-plane VIP times out**

Check API access directly on a control-plane node to distinguish an API
failure from a VIP routing problem. Check `cluster.controlPlaneVIP` is in the
nodes' subnet, unused by other hosts and reachable from your client. Inspect
the `kube-vip` pod logs under `/var/log/pods` for leader-election or network
errors.

**`cluster provisioning failed: cluster "<name>" reports updateFailed: <message>`**

Bootstrap stopped waiting because the controller reported a failure. Read
the controller logs for the cause and check whether work is still running
before choosing recovery or a fresh bootstrap.

**`cluster provisioning failed: cluster "<name>" started deleting while it was being provisioned`**

Something deleted the cluster through the controller's API while bootstrap was
waiting. Check for another operator or automation using the same bootstrap
cluster.

### Finish

**`tenant kubeconfig for "<name>" was not published within 2m0s`**

The site cluster is provisioned, but bootstrap could not retrieve its
kubeconfig. Check the last error and controller logs: the Secret may be
missing, unreadable or empty. Once it is available, save it:

```bash
# Creates a private file; noclobber refuses to overwrite an existing one.
( umask 077; set -o noclobber
  kubectl --kubeconfig <bootstrap-kubeconfig> -n nke-system get secret <name>-kubeconfig -o jsonpath='{.data.value}' | base64 -d > <name>.kubeconfig )
```

Verify the file works before deleting anything. The encryption key and SPIRE
bootstrap resources have not yet been saved; keep the bootstrap cluster until
you have recovered the required outputs.

**`writing SPIRE bootstrap resources: ...`**

Keep the bootstrap cluster. Check the error for a failed Kubernetes read or
file write. Recovery needs `clusterstaticentry-crd.yaml` and
`clusterstaticentries.yaml` alongside the site kubeconfig when mTLS is enabled.

**`making the control-plane nodes schedulable: ...`**

The site cluster and its outputs are ready. Run the manual `kubectl taint`
command printed in the error using the saved site kubeconfig. Do not run
bootstrap again for this failure.

### The encryption key was not saved

**`cluster "<name>" is up and its etcd data is encrypted, but its encryption key was not saved to <path>`**

Keep the bootstrap cluster and run the recovery command printed in the error.
If an earlier failure prevented key capture, locate and save the key Secret:

```bash
kubectl --kubeconfig <bootstrap-kubeconfig> -n nke-controller get secrets | grep etcd-encryption-key-
# Creates a private file; noclobber refuses to overwrite an existing one.
( umask 077; set -o noclobber
  kubectl --kubeconfig <bootstrap-kubeconfig> -n nke-controller get secret <secret-name> -o json > <secret-name>.json )
```

Confirm the saved file contains the expected Secret, and keep it with the
cluster's credentials before deleting the bootstrap cluster or resetting nodes.
For backup and restore usage, see
[Pre-Provisioned Nodes](../../docs/site-bootstrap/pre-provisioned-nodes.md#troubleshooting).

## When it is safe to run bootstrap again

A timeout or cancellation stops the CLI's wait; workflows in a retained
bootstrap cluster can continue. Inspect their progress before deciding to
abandon the attempt. Bootstrap starts fresh; it does not resume an existing site.

What to do per failure phase is in the table under [How to read the error](#how-to-read-the-error).

To abandon a failed site-provisioning attempt:

1. Save the logs, diagnostics and any [encryption key](#the-encryption-key-was-not-saved).
2. Delete the bootstrap cluster to stop further workflow dispatch.
3. Confirm no provisioning commands are still running on the affected hosts.
   Reset only nodes belonging to this failed attempt, using `sudo kubeadm reset -f`
   where kubeadm left state. Resetting a control-plane node removes its local
   etcd member.
4. Check the node prerequisites and fix the original cause before retrying.

The existing-cluster and port-6443 preflight checks catch common leftovers;
they do not prove a node is clean or safe to reset.

## See also

- [Pre-Provisioned Nodes](../../docs/site-bootstrap/pre-provisioned-nodes.md): preparing hosts, and
  node-side troubleshooting
- [Configuration](../../docs/site-bootstrap/configuration.md): the site spec fields the checks above
  refer to
- [Your First Cluster](../../docs/first-cluster.md#4-bootstrap): the full bootstrap
  walkthrough
