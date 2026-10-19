# Kubernetes certificate renewal

CWE renews kubeadm-managed Kubernetes leaf certificates on provisioned clusters.
It does not rotate Kubernetes certificate authorities (CAs).

## Scheduled renewal

The certificate-renewal controller is enabled by
`certRenewal.controllerEnabled` in `nke-controller-config` (default `true`). The chart
renders `certRenewal.controllerEnabled`, `certRenewal.interval` and
`certRenewal.thresholdDays` from `scheduledMaintenance.certRenewal.enabled`,
`.interval` and `.thresholdDays`. It evaluates each cluster at
`nextCertRenewalDue` and starts the `RenewK8sCertsV2` Temporal workflow with the
configured `certRenewal.thresholdDays` (default `30`). The original
`RenewK8sCerts` type remains registered so existing v1 typed-activity histories
can replay during rollout.

The first time the controller sees a cluster without `nextCertRenewalDue`, it
only sets that field one renewal interval ahead; no cycle starts. A new cluster
has nothing to renew, and the cycle holds the cluster mutation lock, which would
reject upgrades and node changes requested while the cluster is still settling.
When that first check is due, the cycle additionally waits until all
control-plane nodes are present and ready.

A cycle acquires the cluster's mutually exclusive update lock, snapshots the
control-plane nodes, and then runs. Acceptance refuses unfinished control-plane
membership work (joins, provisioning/upgrades, accepted deletes, teardown, and
reboots). While the lock is held, or retained by a failed cycle, API requests
to delete a control-plane node or pool are rejected with HTTP 409 and nothing is
recorded, so they must be resubmitted after the lock clears. (Patching a
control-plane pool is always rejected with HTTP 400, lock or not; configure the
control plane through the cluster's `controlPlaneConfig`.) Controller-driven membership work
(creation, remediation, upgrades, joins, and deletes accepted earlier) waits
and resumes once the lock clears; worker-only changes and health observations
continue. Cluster deletion is the exception: it is always accepted, teardown is
not fenced, and the cycle stops at its next health check:

```text
cluster health → node renewal → cluster health → next node
```

The health gate requires stable control-plane membership, joined and Ready
nodes, reachable node agents, for multi-node clusters enough etcd voters to
tolerate losing one, every local kube-apiserver `/readyz` check needed
for quorum-plus-one safety, and healthy static mirror pods after each node. The
typed node-agent operation also verifies that all local control-plane
components are running, etcd has the exact expected voting membership, its
local endpoint is healthy, and the local kube-apiserver is ready immediately
before and after renewal.

For nonforced cycles, an embedded read-only runbook checks each node's leaf
certificate expiry before any drain or renewal dispatch. It also reads either
executor's persisted renewal state (the runbook's phase file and the node agent's
`k8sCertRenewalState`): a renewal left `renewing` or `restart_pending` by an
earlier attempt counts as due even with fresh certificates, so the next attempt
resumes and finishes its restarts. A node with nothing inside
`certRenewal.thresholdDays` and no pending renewal records `unchanged` without
either operation; check failures, including a corrupt state file, stop the cycle
without dispatch. Forced cycles bypass this check.

With more than one control-plane node, a selected node's renewal is bracketed by the
rollout framework's kube-vip drain/restore node guard, as in the
etcd-encryption and OIDC rollouts: the
guard removes kube-vip from the node and waits for VIP connections to drain
before the apiserver restarts, so worker kubelets pinned to that node fail over
instead of going NotReady. The restore runs after success, failure and
cancellation on a disconnected context before the node's postflight. A drain
failure stops the cycle before any renewal is dispatched. A restore failure
fails the cycle for that node even if renewal succeeded, and the message
records it; it does not retain the cluster mutation lock, which only tracks
unconfirmed renewal work. Nodes without kube-vip skip the drain. A cluster with
a single control-plane node is renewed without the drain, since there is no
other node for workers to fail over to. An agent that predates the drain RPCs
(`Unimplemented`) fails the cycle, as it does for the etcd-encryption and OIDC
rollouts, so upgrade such agents before renewal.

Renewal supports clusters with one or with three or more control-plane nodes.
Two etcd voters cannot lose one, so a cluster with exactly two control-plane
nodes is never renewed. Acceptance refuses it and records the cycle as `failed`
with `phase` `two-control-plane-voters`, without the mutation lock. The
controller does not retry that cycle while `controlPlaneConfig.desiredCount`
stays at 2; changing it to 3 or more lets the automatic retry resume. Scaling
down to one node is not possible: the controller only adds control-plane nodes,
and it refuses to delete a healthy node that would leave fewer than three.
Scale such a cluster to three or more nodes before its leaf certificates expire.

A single-control-plane cluster has no peer quorum to protect, so the
lose-one-voter requirement does not apply to it; every other gate, including
exactly one voting etcd member and no learners, still does. Renewing its only
node restarts its only etcd and kube-apiserver, so the Kubernetes API is briefly
unavailable during the cycle. That interruption is inherent and accepted.

The node-agent operation persists its phase and restarts the four kubeadm
control-plane static pods one at a time in the runbook's order (etcd,
kube-apiserver, kube-controller-manager, kube-scheduler). For each pod it moves
only that manifest to a staging directory on the same filesystem, waits for the
old container to stop, restores the manifest, and waits for a replacement
container and full local control-plane health before touching the next pod.
Unrelated manifests such as kube-vip are never moved. Every failure path
restores the active manifest, and an interrupted operation resumes by
restarting all four pods again. A Temporal activity retry reattaches to the
stable operation ID instead of starting a second renewal. The hardened workflow calls the capability-distinct V2 start RPC, so
agents that implement only the V1 operation return `Unimplemented` before
accepting node-side work. The controller then uses the embedded runbook, whose
persistent state, bounded lock, and CA fingerprint checks add mixed-fleet
safety. Both executors restart the same pods one at a time in the same order,
gated on local control-plane health; they differ in resume granularity (the
runbook resumes at the persisted component, the node agent restarts all four
pods again) and in the runbook's additional lock and CA fingerprint checks.

`nextCertRenewalDue` advances by `certRenewal.interval` only after the workflow
succeeds. A failed scheduled cycle leaves the old due time intact and therefore
remains overdue until a fresh cycle succeeds. If the failure released the
mutation lock, the controller retries with a new workflow ID once both
`nextCertRenewalDue` and a fixed one-hour backoff after
`certificateRenewal.completedAt` have passed. A failure does not move
`nextCertRenewalDue`, so a failed cycle that started before its due time, such
as a forced one, is not retried until that due time arrives, up to
`certRenewal.interval` later. A retained `certificate_renewal` lock prevents automatic retry.
When acceptance refuses a cycle because of cluster state (unfinished
control-plane membership, upgrade or recycling work, a node-agent installation,
or two control-plane nodes), the cycle is recorded as `failed` with `phase`
`admission` (or `two-control-plane-voters`) and the lock is not taken, so the
same backoff applies. Such a record replaces the previous cycle's status.
Refusals because another operation holds the lock, or because the cluster is
being deleted, record nothing; the controller waits for the lock to clear.

## Forced renewal

Forced renewal is intentionally an operator-started Temporal workflow; there is
no REST or CLI trigger. Use a new workflow ID for every forced run, and do not
reuse the controller's `renew-k8s-certs-...` IDs. Start it in the namespace and
task queue the controller's workers poll: `temporal.namespace` and
`temporal.taskQueue` in `nke-controller-config` (chart values
`temporal.cloudWorkflow.namespace` and `temporal.cloudWorkflow.queue`; defaults
`cloud-workflow-engine` and `cloud-workflow-engine-tasks`). A different namespace
or queue is rejected or never runs. Also pass the Temporal CLI's address and TLS
flags for the site's Temporal frontend (`temporal.hostPort`).

```bash
TEMPORAL_NAMESPACE=cloud-workflow-engine         # temporal.namespace
TEMPORAL_TASK_QUEUE=cloud-workflow-engine-tasks  # temporal.taskQueue

temporal workflow start \
  --namespace "$TEMPORAL_NAMESPACE" \
  --task-queue "$TEMPORAL_TASK_QUEUE" \
  --type RenewK8sCertsV2 \
  --workflow-id "operator-k8s-cert-renewal-$(date -u +%Y%m%dT%H%M%SZ)-$(uuidgen)" \
  --input '{"clusterID":"<cluster-uuid>","thresholdDays":30,"force":true}'
```

A successful forced cycle also resets `nextCertRenewalDue`, preventing an
immediate duplicate scheduled run.

## Status and failure handling

`GET /api/clusters/{id}` exposes:

- `nextCertRenewalDue`: a past value is overdue unless a cycle is accepted;
- `conditions.updateInProgress.type=certificate_renewal`: the cluster mutation
  lock held by an accepted cycle, or retained by a failed cycle whose node work
  has not been confirmed terminal;
- `conditions.certificateRenewal.cycleID`: the owning Temporal workflow ID;
- `conditions.certificateRenewal.state`: `accepted`, `succeeded`, or `failed`;
- successful `outcome`: `renewed` or `unchanged`;
- failure `failedNodeID`, `phase`, and a bounded operator-facing `message`.

A node operation, fallback runbook, or postflight failure exhausts its bounded
Temporal activity retries, records the failing node and phase, and stops before
another node is scheduled. After a node-renewal failure or cancellation, cleanup
has a separate five-minute budget to confirm termination of the same operation
ID on the executor already selected by the workflow. Typed cleanup only polls
status, retrying transport, `Unavailable` and `DeadlineExceeded` errors until
that budget ends; legacy cleanup requires a matching durable terminal receipt under the
runbook's flock, which also excludes an active EXIT restoration. Neither cleanup
starts or resumes renewal. Healthy diagnostic postflight alone is not proof of
termination.

The V2 runbook stores immutable, operation-ID-keyed receipts in
`/var/lib/nvke/k8s-cert-renewal/completed/`. Delayed attempts check their own receipt
before reading or resuming the shared phase file, so they cannot restart an old
operation or interfere with a newer one. Receipts preserve the original outcome
and must not be deleted while delayed attempts could still arrive. Only newly
started V2 operations use the receipt-aware phase format: old five-line state
remains readable/resumable but is never upgraded into fencing evidence.
Normal V2 legacy success also requires this receipt verification; a successful
outcome from a receipt-unaware override fails without retrying the override.

For a legacy operation that ended without a receipt, the status-only check,
holding the renewal lock, records an `abandoned` receipt for that operation ID.
Delayed attempts with that ID then fail without changing the node, the cycle is
recorded as failed, and the lock is released; a half-finished renewal is
resumed by the next operation. Phase files written without receipt provenance
are never abandoned this way.

If termination cannot be confirmed (including mismatched, unfinished,
malformed, or unreachable status), the cycle is recorded as failed with a
`cluster mutation lock retained` diagnostic. `updateInProgress` remains set to
block overlapping cluster mutations. The due time is not advanced. Legacy status
checks always use the embedded status-only implementation, not a configured
runbook override that might ignore the status-only mode.

The controller never reopens the same failed cycle ID. Released failures retry
automatically with a fresh ID once both `nextCertRenewalDue` and the one-hour
backoff have passed; retained-lock failures stay halted.
For a retained lock, first establish that the reported operation and all its
attempts have stopped, including any legacy manifest restoration, and repair the
reported node/health problem. An operator must then clear only that cycle's
`certificate_renewal` mutation lock with an ownership-checked database transaction
on the cluster's row in the `clusters` table: under the row lock, check that the
`conditions` jsonb column has `updateInProgress.type` = `certificate_renewal`,
`certificateRenewal.cycleID` = the failed cycle ID and `certificateRenewal.state`
= `failed`, then remove only the `updateInProgress` key. There is no automatic
retained-lock release or dedicated recovery API. Do not clear the lock based
only on cluster health or workflow completion.

Clearing the lock leaves a released failure, and the controller then starts
its own non-forced `-retry-` cycle once `nextCertRenewalDue` has passed and
`certificateRenewal.completedAt` is more than an hour old. If the due time is
still in the future, that retry waits for it, up to `certRenewal.interval`. That retry takes the lock, so
a forced workflow started meanwhile is refused. Wait for the automatic retry and
inspect its result; start a uniquely named forced workflow only when forced
semantics are needed, after that retry has finished.

## Validation evidence

For a representative three-control-plane cluster, capture before and after:

```bash
sudo kubeadm certs check-expiration
sudo sha256sum \
  /etc/kubernetes/pki/ca.crt \
  /etc/kubernetes/pki/etcd/ca.crt \
  /etc/kubernetes/pki/front-proxy-ca.crt
kubectl get nodes
kubectl -n kube-system get pods \
  -l component=kube-apiserver -o wide
kubectl get --raw '/readyz?verbose'
```

Run continuous API traffic for the duration of scheduled and forced validation.
On each control-plane node, validate etcd from the local kubeadm etcd container
with the same TLS settings the renewal runbook uses:

```bash
sudo crictl exec "$(sudo crictl ps --label io.kubernetes.container.name=etcd -q)" etcdctl \
  --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/server.crt \
  --key=/etc/kubernetes/pki/etcd/server.key \
  --dial-timeout=10s --command-timeout=30s \
  endpoint health --cluster
```

Record the source revision, workflow ID, Temporal result, certificate details,
unchanged CA fingerprints, static-pod container IDs, API probe results, and any
known validation limitations.
