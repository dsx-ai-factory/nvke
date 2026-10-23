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
the cluster and node pool workflows, and their steps. Without an OTLP endpoint
(below), the controller's logs are the record.

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

#### Send nvkectl's telemetry to a backend

nvkectl can send its spans and log records to an OpenTelemetry collector over
OTLP/gRPC. Each carries the run's trace ID, so you can search the backend for
the `Trace ID:` that nvkectl prints. What nvkectl writes to stderr does not
change.

##### With environment variables

| Variable | What it does |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | The collector's address, as `host:port` or an `http://` or `https://` URL, with no credentials and no path. With no scheme, nvkectl uses TLS. Every nvkectl command reads it. |
| `OTEL_EXPORTER_OTLP_INSECURE` | With an endpoint that has no scheme, `true` sends without TLS. An `http://` URL already does. |
| `OTEL_EXPORTER_OTLP_HEADERS` | Headers for the collector, such as a token, as `key=value` pairs separated by commas. nvkectl sends them, and `bootstrap site` also gives them to the temporary controller. |

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.com:4317
export OTEL_EXPORTER_OTLP_HEADERS='authorization=Bearer <TOKEN>'
nvkectl bootstrap site -f sitespec.yaml
```

`bootstrap site` also exports the temporary controller's spans to this endpoint,
with these headers, so the endpoint must be reachable from inside the Kind
cluster. A loopback endpoint (`localhost`, `127.0.0.1`, `::1`) is used by nvkectl
only: nvkectl prints a warning and the controller's spans are not exported.

If the endpoint is wrong, nvkectl prints one warning that names the variable
and says what to do, sends no telemetry, and runs the command.

##### With flags on `bootstrap site`

`--otlp-endpoint <host:port or URL>` sets the collector's address and
`--otlp-insecure` sends without TLS. When `--otlp-endpoint` is set, it is used
instead of `OTEL_EXPORTER_OTLP_ENDPOINT`:

```bash
nvkectl bootstrap site -f sitespec.yaml --otlp-endpoint 192.0.2.10:4317 --otlp-insecure
```

The temporary controller in the bootstrap cluster exports its spans to the same
endpoint, so the endpoint must be reachable from inside the Kind cluster. A
loopback address (`localhost`, `127.0.0.1`, `::1`, `0.0.0.0` or `::`) is
rejected; use the host's LAN address or a DNS name.

A wrong flag value is an error: the command stops before bootstrap starts and
the error names the flag.

Headers cannot be passed as a flag. Set `OTEL_EXPORTER_OTLP_HEADERS`. nvkectl
sends them to the endpoint you set with `--otlp-endpoint` and also gives them to
the temporary controller, so its spans reach the same backend. With an `http://`
endpoint or `--otlp-insecure`, the headers travel without TLS and nvkectl prints
a warning. The controller keeps them in its Deployment and Helm release until
the bootstrap cluster is deleted.

nvkectl verifies the collector's certificate against the host's trusted
certificate authorities, for spans and for logs. It does not use
`OTEL_EXPORTER_OTLP_CERTIFICATE` or the client certificate variables, so a
collector with a private certificate authority needs that authority in the
host's trust store, and a collector that requires client certificates is not
supported. Use the collector's gRPC address, usually `:4317`, not its HTTP
address such as `https://<COLLECTOR_HOST>:4318/v1/traces`.

If the collector cannot be reached, nvkectl prints one warning and the command
still succeeds.

Commands that use a site, such as `get clusters`, send to the endpoint stored
with `nvkectl config set-site --otlp-endpoint`; see
[Export a site's telemetry](user-flow.md#export-a-sites-telemetry).

### The diagnostics directory

Bootstrap attempts to collect diagnostics before cleaning up a failed
bootstrap cluster. The default is `cluster-diagnostics/`; change it with
`--diagnostics-dir` or `NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR`. Collection may be
incomplete if the cluster is unreachable. Pass `-v` to collect them from a
successful bootstrap as well, for a support bundle or to check what a healthy
run prints. A retry writes into the same
directory, replacing files of the same name, so move it first or pass a new
`--diagnostics-dir`.

Without `-v`, no diagnostics are collected when the failure comes after the site
cluster and its outputs are saved, as in
[making the control-plane nodes schedulable](#finish). The bootstrap cluster is
retained, so read its controller logs and the nodes' journals instead. With `-v`
they are collected in that case too.

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
(`capnico-system` with `infra.provider: nico`) and `nke-controller`.

### The retained bootstrap cluster

Bootstrap normally deletes its Kind cluster. It keeps it when you pass
`--keep-bootstrap-cluster`, when a failure occurs after the controller
accepts the site cluster request and before the outputs are saved, or, with
`infra.provider: nico`, when the create request got no answer, so the
controller may have created the site cluster. A failed
cleanup can also leave it behind. The logs report retention and, when
available, the kubeconfig path.

```text
level=WARN msg="bootstrap failed after site cluster creation; retaining the bootstrap kind cluster for recovery" cluster=nvkectl-bootstrap kubeconfig=/tmp/nvkectl-bootstrap-kubeconfig-.../config
```

If you set `OTEL_EXPORTER_OTLP_HEADERS`, the kept cluster also holds those
headers, typically a token, in the controller's Deployment and Helm release, so
delete it when you are done.

With `infra.provider: nico`, the kept cluster also holds the
NICo token and the tenant password as Secrets (`nico-credentials` in
`capnico-system`, `dgxc-tenant-credentials` in `nke-controller`), so delete it
when you are done. After a failure past site cluster creation but before every
control-plane node joined, it also holds the machines for the NICo instances
CAPNICo allocated, and those must be released before the cluster is deleted: see
[Release the NICo instances first](#release-the-nico-instances-first). After
every node joined the site cluster is up and working: recover what the run could
not write and delete only the bootstrap cluster.

Use that kubeconfig to look at the live state, which is more than the dump
captures:

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

#### Release the NICo instances first

With `infra.provider: nico`, after a failure past the point where the site
cluster was created and before every control-plane node joined, the retained
cluster also holds the Cluster API machines
for the NICo instances CAPNICo allocated. Deleting a Kind cluster runs no finalizers, so deleting it
now leaves those instances allocated and unmanaged, with the tenant login in
their user data, and nothing in NICo or in a re-run will find them. Release them
first, by deleting the site cluster through the retained cluster's controller,
which deletes each node's machine and so releases its instance:

```bash
nvkectl config set-site --name nvkectl-bootstrap --url http://127.0.0.1:30080 \
  --kubeconfig /tmp/nvkectl-bootstrap-kubeconfig-.../config   # the path in the log line above
nvkectl delete cluster <clusterName>                          # clusterName from your site spec
nvkectl get clusters                                          # repeat until it is no longer listed
```

The URL port is `NVKECTL_NVKE_CONTROLLER_PORT`, `30080` by default. `set-site` makes
`nvkectl-bootstrap` the active site; switch back with
`nvkectl config use-site <name>`. A cluster can be deleted in any state, and it
stays listed as `Deleting` until every machine is released. Only then delete the
Kind cluster. The failure message prints these steps.

The same applies when the create request got no answer: a timeout, an
interrupt, a dropped connection or a `5xx`. The controller commits the cluster
and starts provisioning it before it answers, so with `nico` the cluster may
exist and CAPNICo may be allocating its instances. The bootstrap cluster is
then kept, and the failure message says the create was not confirmed. Run
`set-site` as above, then `nvkectl get clusters`. If it lists `<clusterName>`,
delete it and wait as above. If it does not, nothing was allocated. Either way,
then delete the Kind cluster.

None of this applies once every control-plane node joined, whether the run then
failed writing the kubeconfig or saving the key, or succeeded and was kept with
`--keep-bootstrap-cluster`: the instances are then the new cluster's nodes, and
`nvkectl delete cluster` would destroy the working site cluster and its NICo
machines. Recover the kubeconfig and the
[encryption key](#the-encryption-key-was-not-saved) from the retained cluster,
then delete only the Kind cluster. The failure message says which case you are in.

#### Delete the cluster

When it is safe to delete it, which with `nico` is after the instances are
released, use the same provider that created it:

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
registry reference with `--nvke-controller-image` or `--provisioned-provider-image`
(or `NVKECTL_NVKE_CONTROLLER_IMAGE` / `NVKECTL_CAPI_PROVISIONED_IMAGE`). If building
from source, `make -C cli images` builds the default local images.

**`bootstrap cluster "nvkectl-bootstrap" from an earlier run is still running`**

Inspect the retained cluster before deleting it. If the earlier run created
a site cluster, follow the [recovery guidance](#when-it-is-safe-to-run-bootstrap-again)
first. With `infra.provider: nico`, if the earlier run failed before every
control-plane node joined, [release the NICo instances](#release-the-nico-instances-first)
it allocated before you delete the cluster: deleting it alone leaves them
allocated. If it succeeded, or failed after every node joined, the site cluster
is working: recover from the Kind cluster whatever that run did not write,
delete only the Kind cluster, and do not re-run, because a re-run allocates
another set of NICo machines. Bootstrap does not reuse an existing Kind cluster.

**`the control-plane VIP ... is accepting connections on port 6443`**

With `infra.provider: nico` there are no hosts to probe, so the bootstrap dials
`cluster.controlPlaneVIP` on port 6443 before it builds anything. An answer
means a site cluster for this spec is most likely running: a second bootstrap
would allocate another set of NICo machines and bring up a second control plane
advertising the same VIP. If that cluster is yours, do not re-run. If it is a
half-built one from a failed bootstrap, [release its NICo
instances](#release-the-nico-instances-first) and re-run once the VIP stops
answering. If something else holds the address, change `cluster.controlPlaneVIP`.

**`port 6443, the Kubernetes API port, is accepting connections on: <node> (<ip>)`**

A host in the site spec already has something listening on the Kubernetes API
port. This check applies to `provisioned` only: with `nico` the machines are
allocated, so there is no host to probe. Confirm the host belongs to this bootstrap and inspect
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

The image exists locally but could not be exported into the Kind cluster,
usually because the archive lists a platform whose content is not in the local
store. kind imports every platform an image index lists, so nvkectl pulls every
platform of an image pinned by an index digest. For a tag, pull it again for the
bootstrap host's platform and re-run. To load only one platform of a pinned
image, pin that platform's manifest digest instead:
`nerdctl image inspect --mode=native --format '{{.ManifestDesc.Digest}}' <image>`.

### Controller deploy

**`installing postgres` / `temporal` / `SPIRE CRDs` / `SPIRE` / `cwe`: `helm ... exit status 1`**

The error names the release; the Helm output gives the cause:

- *Chart fetch failed*, such as `failed to fetch ... 500` or a timeout. The
  chart repository is unreachable from the bootstrap host. Retry, or point at
  a mirror or local copy with `NVKECTL_NVKE_CONTROLLER_TEMPORAL_CHART`,
  `NVKECTL_NVKE_CONTROLLER_SPIRE_CHART` and `NVKECTL_NVKE_CONTROLLER_SPIRE_CRDS_CHART`. Nodes are
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

`NVKECTL_NVKE_CONTROLLER_HELM_TIMEOUT` raises the per-release wait from its default of
five minutes on a slow network.

**`the controller did not report healthy on <url> within 5m0s: last error=... last response=...`**

Check the controller pod's state and the last error. If `/readyz` responded,
it identifies the failing dependency: inspect `postgres/*/logs.txt` for
`database`, or `temporal/*/logs.txt` for `temporal`. Check credentials,
connectivity and schema jobs. `NVKECTL_NVKE_CONTROLLER_READY_TIMEOUT` raises the wait.

**`waiting for the published cwe api: http://127.0.0.1:30080/readyz did not become ready`**

The controller passed its in-cluster check, but the host check failed. Verify
it is still healthy, then inspect the Kind port mapping and NodePort Service.
Check for a conflict on port 30080, or change it with `NVKECTL_NVKE_CONTROLLER_PORT`.

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
  kubectl --kubeconfig <bootstrap-kubeconfig> -n nvke-system get secret <name>-kubeconfig -o jsonpath='{.data.value}' | base64 -d > <name>.kubeconfig )
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
2. Delete the bootstrap cluster to stop further workflow dispatch. With
   `infra.provider: nico` and a failure before every control-plane node joined,
   first [release the NICo instances](#release-the-nico-instances-first):
   deleting the Kind cluster runs no finalizers, so it leaves them allocated and
   unmanaged.
3. Confirm no provisioning commands are still running on the affected hosts.
   Reset only nodes belonging to this failed attempt, using `sudo kubeadm reset -f`
   where kubeadm left state. Resetting a control-plane node removes its local
   etcd member. With `nico` the release in step 2 resets and releases the nodes,
   so this step applies to `provisioned` only.
4. Check the node prerequisites and fix the original cause before retrying.

With the `provisioned` provider the existing-cluster and port-6443 preflight
checks catch common leftovers; they do not prove a node is clean or safe to
reset.

With `infra.provider: nico` the existing-cluster check applies, and a probe of
`cluster.controlPlaneVIP` on port 6443 when the VIP is set; a one-node spec
without one has nothing to probe. The nodes are allocated rather than
listed, so there is no host to probe, and the preflight cannot see the NICo
instances an earlier run allocated. It stops a re-run while the retained Kind
cluster exists, or while a site cluster answers on the VIP; once that cluster is
deleted and nothing answers on the VIP, nothing stops one, and each re-run allocates another
`infra.nico.controlPlane.count` machines. So release the instances before you
delete the Kind cluster, and check NICo for instances left by a cluster you
already deleted.

## See also

- [Pre-Provisioned Nodes](../../docs/site-bootstrap/pre-provisioned-nodes.md): preparing hosts, and
  node-side troubleshooting
- [Configuration](../../docs/site-bootstrap/configuration.md): the site spec fields the checks above
  refer to
- [Your First Cluster](../../docs/first-cluster.md#4-bootstrap): the full bootstrap
  walkthrough
