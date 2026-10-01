# Kubernetes certificate renewal

CWE renews kubeadm-managed Kubernetes leaf certificates on provisioned clusters.
It does not rotate Kubernetes certificate authorities (CAs).

## Scheduled renewal

The certificate-renewal controller is enabled by
`CERT_RENEWAL_CONTROLLER_ENABLED` (default `true`). It evaluates each cluster at
`nextCertRenewalDue` and starts the `RenewK8sCertsV2` Temporal workflow with the
configured `CERT_RENEWAL_THRESHOLD_DAYS` (default `30`). The original
`RenewK8sCerts` type remains registered so existing v1 typed-activity histories
can replay during rollout.

A cycle acquires the cluster's mutually exclusive update lock, snapshots the
control-plane nodes, and then runs:

```text
cluster health → node renewal → cluster health → next node
```

The health gate requires stable control-plane membership, joined and Ready
nodes, reachable node agents, for multi-node clusters enough etcd voters to
tolerate losing one (two voters are refused), every local kube-apiserver `/readyz` check needed
for quorum-plus-one safety, and healthy static mirror pods after each node. The
typed node-agent operation also verifies that all local control-plane
components are running, etcd has the exact expected voting membership, its
local endpoint is healthy, and the local kube-apiserver is ready immediately
before and after renewal.

Each node's renewal is bracketed by the rollout framework's kube-vip
drain/restore node guard, as in the etcd-encryption and OIDC rollouts: the
guard removes kube-vip from the node and waits for VIP connections to drain
before the apiserver restarts, so worker kubelets pinned to that node fail over
instead of going NotReady. The restore runs after success, failure and
cancellation on a disconnected context before the node's postflight. A drain
failure stops the cycle before any renewal is dispatched. A restore failure
fails the cycle for that node even if renewal succeeded, and the message
records it; it does not retain the cluster mutation lock, which only tracks
unconfirmed renewal work. Nodes without kube-vip skip the drain. An agent that
predates the drain RPCs (`Unimplemented`) fails the cycle, as it does for the
etcd-encryption and OIDC rollouts, so upgrade such agents before renewal.

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

`nextCertRenewalDue` advances by `CERT_RENEWAL_INTERVAL` only after the workflow
succeeds. A failed scheduled cycle leaves the old due time intact and therefore
remains overdue.

## Forced renewal

Forced renewal is intentionally an operator-started Temporal workflow; there is
no REST or CLI trigger. Use a new workflow ID for every forced run, and do not
reuse the controller's `renew-k8s-certs-...` IDs:

```bash
temporal workflow start \
  --task-queue "$TEMPORAL_TASK_QUEUE" \
  --type RenewK8sCertsV2 \
  --workflow-id "operator-k8s-cert-renewal-$(date -u +%Y%m%dT%H%M%SZ)" \
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
status; legacy cleanup requires a matching durable terminal receipt under the
runbook's flock, which also excludes an active EXIT restoration. Neither cleanup
starts or resumes renewal. Healthy diagnostic postflight alone is not proof of
termination.

The V2 runbook stores immutable, operation-ID-keyed receipts in
`/var/lib/nke/k8s-cert-renewal/completed/`. Delayed attempts check their own receipt
before reading or resuming the shared phase file, so they cannot restart an old
operation or interfere with a newer one. Receipts preserve the original outcome
and must not be deleted while delayed attempts could still arrive. Only newly
started V2 operations use the receipt-aware phase format: old five-line state
remains readable/resumable but is never upgraded into fencing evidence.
Normal V2 legacy success also requires this receipt verification; a successful
outcome from a receipt-unaware override fails without retrying the override.

If termination cannot be confirmed (including missing, mismatched, unfinished,
malformed, or unreachable status), the cycle is recorded as failed with a
`cluster mutation lock retained` diagnostic. `updateInProgress` remains set to
block overlapping cluster mutations. The due time is not advanced. Legacy status
checks always use the embedded status-only implementation, not a configured
runbook override that might ignore the status-only mode.

The controller does not automatically relaunch the same failed scheduled cycle.
For a retained lock, first establish that the reported operation and all its
attempts have stopped, including any legacy manifest restoration, and repair the
reported node/health problem. An operator must then clear only that cycle's
`certificate_renewal` mutation lock with an ownership-checked database transaction
(check both the failed cycle ID and lock type under the cluster row lock). There
is no automatic retained-lock release or dedicated recovery API. Do not clear
the lock based only on cluster health or workflow completion. Once ownership is
safely released, start a uniquely named forced workflow.

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
with `etcdctl endpoint health --cluster`. Record the source revision, workflow
ID, Temporal result, certificate details, unchanged CA fingerprints, static-pod
container IDs, API probe results, and any known validation limitations.
