# T2 etcd data-encryption-key rotation

`RotateEtcdEncryptionKey` is the operator-initiated CWE Temporal workflow for a
CWE-managed tenant cluster that already has API object encryption enabled. It
adds a secondary key to every control-plane API server, promotes it everywhere,
rewrites existing objects, removes old keys everywhere, then finalizes the
management-plane key Secret. This rotates the data-encryption key; it does not
rotate an etcd CA, API certificate, storage-disk key, or external KMS key.

The OSS alpha path uses kubeadm static pods, local etcd, node-agent RPCs, and
Kubernetes secretbox encryption. It has no public rotation REST endpoint and no
automatic rotation policy. A three-node control plane is the HA qualification
baseline. A single control plane can use the same workflow, with its usual loss
of redundancy; other topologies need their own qualification evidence.

## Prerequisites

Before starting, record the controller and agent versions, cluster UUID,
control-plane node UUIDs, topology, operator, time, and approved change window.

- `GET /api/clusters/{clusterID}` reports `etcdEncrypted: true`, a provisioned
  cluster, no deletion, and no `conditions.updateInProgress`.
- Every control-plane node is joined, reachable, and running a node agent built
  from this release or later. Rotation needs the `GetEtcdEncryptionKeyIdentity`
  RPC and the `api_encryption_key_name` request fields, which this release adds.
  No published agent up to and including `agent/v0.3.0-rc.1` has them. The
  workflow's existing v0.1.26 version gate does not prove these capabilities, so
  check each control-plane node before starting. For agents serving plain mTLS,
  check through gRPC reflection, using the controller's mTLS client identity:

  ```bash
  grpcurl <mTLS flags> <node-ip>:15005 list proto.NodeAgent | grep -Fx proto.NodeAgent.GetEtcdEncryptionKeyIdentity
  ```

  A missing line means the agent must be upgraded first. This check does not
  work against agents running `--grpc-auth-mode=tls-jwt`: they send an
  authentication record after the TLS handshake that grpcurl cannot read, so
  `list` fails even on a current agent. For those agents, verify the deployed
  agent version instead, or rely on the `prepare`-phase identity probe described
  next, which refuses older agents before any key is distributed. If a rotation starts
  anyway, it fails in the `prepare` phase, before any key is distributed,
  with "upgrade older node agents" in the workflow failure. A recorded `prepare`
  failure releases the mutation lock: upgrade the agents, then start a rotation
  with a **new workflow ID**, not a reset of the failed workflow (see recovery
  below). Any already-staged pending key is preserved and reused.
- Membership must stay fixed throughout the operation. No upgrades, recycling,
  certificate renewal, or other mutations may overlap.
- The API servers run **Kubernetes 1.34 or newer** and use CWE's encryption
  configuration with automatic reload. Each node-local `/metrics` endpoint must
  expose `apiserver_encryption_config_controller_last_config_info` and a valid
  `process_start_time_seconds`. The loaded-config metric is an alpha metric;
  verify its availability on the deployed build. Kubernetes 1.33 and earlier
  do not provide this proof, so Begin refuses them before taking the mutation
  lock. Upgrade Kubernetes before starting rotation; counter-only confirmation
  is unsupported. The agent needs permission to read local `/metrics`.
  The active key exists in CWE's key Secret; no pending key from an unrelated
  operation is present. A pending key left by a released `prepare` failure is
  reused. Inspect field presence only; do not print Secret data.
- Every control-plane node is already rolled to the cluster's Kubernetes
  version, with no control-plane upgrade pending. Raising the cluster's
  Kubernetes version records the target before the nodes are upgraded, so
  Begin also compares each node's pinned control-plane configuration with the
  cluster version. It refuses before taking the mutation lock when a node
  differs or its version cannot be determined; wait for the upgrade to finish,
  then start the rotation.
- CWE can read/update its management-plane key Secret and contact each tenant
  node over its configured SPIFFE mTLS connection. PostgreSQL, Temporal, and the
  CWE worker task queue are available.
- Operator access to the tenant API and an established backup/recovery process
  are available. Keep backup material and kubeconfigs outside this repository.
  A pre-rotation etcd snapshot still requires its corresponding old key; retiring
  that key from live nodes does not make that snapshot recoverable without it.

The workflow snapshots the control-plane node set and runs health/preflight
checks before rollout. Key updates hot-reload API server configuration; they do
not drain or restart the node. Existing rollout verification confirms reload
before moving to the next node. Failure stops subsequent phases.

Rotation admission holds the cluster row lock and refuses unfinished CP joins,
provisioning/upgrades, accepted deletes, node/pool teardown, and reboots before
staging a key. The same lock fences new control-plane API/controller mutations
in both controller-managed and legacy node-pool modes. Delayed Temporal activities
also reserve node work before external calls, even when their legacy inputs omit
progress messages. Worker-only node-pool changes and health/drift observations
continue. Failures in `prepare` release this fence; failures from `add_secondary`
on retain it until their existing owner recovers.
Cluster deletion is the exception: it is always accepted, teardown is not
fenced, and the rotation stops at its next phase or membership check.

New workflow executions recheck the fixed node snapshot and every API server's
readiness after rewrite. A read-only embedded probe requires exactly the expected
live etcd member names, no learners, and healthy cluster endpoints; it uses the
node's existing `crictl`, `etcdctl` container and `jq`. The workflow then confirms
pending-primary configuration/reload on every target and repeats the membership
and readiness checks before removing old keys. A failure keeps both keys and
ownership. Legacy histories keep their recorded command sequence through the
`etcd-key-rotation-membership-convergence` version marker; captured-history replay
and live topology/failure qualification remain required before site acceptance.

The management key Secret retains `active-name` and `pending-name` alongside
the corresponding key bytes. Kubernetes ciphertext includes the key name, so
equal bytes under a different name cannot decrypt existing objects. Rotation
chooses and saves a new name before distribution, verifies it at each mutation,
and finalizes the name and bytes together. Subsequent control-plane init, join,
and replacement use the saved active name. A replacement with an older agent
is refused before kubeadm bootstrap when it cannot accept that name.

Existing Secrets without name fields are migrated by querying every joined
control-plane agent for the stored name of the managed key. All nodes must
agree; unreachable or older agents, missing active keys, and conflicting names
block migration without changing live encryption configurations.

Control-plane init and join stay available on such clusters. When no node can
report a name, because none has joined yet or every joined agent predates the
identity RPC, they use the original bootstrap name, the only name earlier
controllers and agents used, and record nothing. They still fail if an agent is
unreachable or an upgraded agent reports a different name. A cluster rotated by
an earlier controller may already use another storage name, so upgrade every
control-plane agent before replacing nodes on it; the next init, join, or
rotation then records the proven name. A new rotation that reuses a pending
key from an interrupted earlier rotation adopts its existing name wherever
already distributed.

A rotation started by an earlier controller and still running when the
controller is upgraded keeps its original protocol, and its node agents cannot
be upgraded while it holds the mutation lock. If the earlier controller staged
its pending key, the key has no saved name: every node mutation sends it
unnamed, so each agent derives the name itself or matches the existing key by
its bytes, and no identity RPC is called. Finalization then makes the key active
without a saved name, and the next init, join, or rotation discovers it as
described above. If the pending key was staged after the upgrade, it has a
saved name and every mutation verifies that name as in a new rotation.
Do not repair a missing management field by renaming a live storage key.

## Invocation and observation

Choose the actual Temporal address, namespace, and CWE task queue from the site
configuration. Use a new workflow ID for each rotation and retain it for recovery.
An execution that reuses the ID of a succeeded rotation fails with a
non-retryable precondition error before any node work, instead of reporting
success without rotating.
The input contains only the tenant cluster UUID. The following values are
placeholders, not deployment defaults:

```bash
rotation_cluster_id=00000000-0000-0000-0000-000000000001
rotation_workflow_id="etcd-key-rotation-${rotation_cluster_id}-$(date -u +%Y%m%dT%H%M%SZ)"
rotation_temporal_address=temporal.example.invalid:7233
rotation_temporal_namespace=example-namespace
rotation_task_queue=example-cwe-task-queue

temporal workflow start \
  --address "$rotation_temporal_address" \
  --namespace "$rotation_temporal_namespace" \
  --type RotateEtcdEncryptionKey \
  --task-queue "$rotation_task_queue" \
  --workflow-id "$rotation_workflow_id" \
  --id-reuse-policy RejectDuplicate \
  --id-conflict-policy Fail \
  --input "{\"clusterID\":\"$rotation_cluster_id\"}"

temporal workflow describe \
  --address "$rotation_temporal_address" \
  --namespace "$rotation_temporal_namespace" \
  --workflow-id "$rotation_workflow_id"
```

The CLI's `start`, `describe`, and `reset` syntax is described in the
[Temporal workflow command reference](https://docs.temporal.io/cli/command-reference/workflow).
Do not configure a run/execution timeout shorter than the planned rewrite and
recovery allowance. The rewrite poller allows up to six hours. Server-side
termination/timeouts can bypass workflow cleanup and cannot stop a node's
background rewrite; a closed workflow is not proof of safe mutation completion.

The cluster REST response exposes `conditions.etcdKeyRotation`:

| Field | Meaning |
|---|---|
| `workflowID` | The owner used for inspection and reset; never a metric label |
| `state` | `running`, `succeeded`, or `failed` |
| `phase` | `prepare`, `add_secondary`, `promote_primary`, `rewrite`, `remove_old_keys`, or `finalize` |
| `startedAt` | When ownership was first acquired |
| `lastProgressAt` | Last executed phase entry or terminal state change |
| `completedAt` | When success/failure was recorded; absent while running |
| `failureReason` | `operation_failed` or `canceled`, without arbitrary error text |

Polling does not advance `lastProgressAt`. A controller restart preserves this
state because it is in PostgreSQL. A failed rotation records `updateFailed`.
In `prepare` it releases the mutation lock; from `add_secondary` on it retains
`conditions.updateInProgress.type: etcd_key_rotation`. In those later phases,
new certificate-renewal, upgrade, recycling, and rotation requests remain blocked
until recovery completes. Legacy workflow histories retain their original
`etcd_encryption` lock/cleanup behavior through a Temporal version marker;
absence of `etcdKeyRotation` is unknown observation, not successful rotation.

## Phase-aware recovery

A recorded `prepare` failure releases `updateInProgress`, because no node has
received the pending key. Fix the prerequisite, then start a rotation with a
**new workflow ID**. Keep the key Secret unchanged: the new execution reuses
any staged pending key and its name. Do not reset the failed workflow; after
release it no longer owns the mutation lock.

From `add_secondary` on, a failed rotation keeps `updateInProgress` deliberately.
Until finalization, the key Secret's active key is the old one, while nodes may
already use the pending key as their primary (from `promote_primary` on) or have
dropped the old key (from `remove_old_keys` on). A node that joined then would bootstrap with the
old key alone and could not read objects written with the new one. The lock
therefore defers control-plane joins, scale-up, remediation, and node deletes
until the original workflow finishes. Cluster deletion is always accepted.

For these retained-lock failures, first fix connectivity, authorization, reload,
or rewrite prerequisites. Inspect the owning workflow and nodes while keeping
the mutation lock. Do not delete the pending key, hand-edit encryption
configuration, edit database conditions, start a competing rotation, or
restore an old configuration after old keys were removed.

Prefer automatic activity retry while the original execution is running. If it
has failed/canceled in `add_secondary` or later, recover by **resetting that same
workflow ID**, after proving all old key-update activity attempts are terminal
and no conflicting node work
can run. If an execution is stuck, investigate its outstanding activities first;
termination alone cannot establish that its node operation stopped.

An API server restart during a node key update also recovers through retry of
that same operation. Its private on-node WAL retains the existing operation ID
and counter field, but completion uses the **loaded configuration**, not a counter
from the old process. The agent compares the exact intended file with the local
API server's loaded-config identity, confirms it twice with one stable process
and server identity, and checks the disk file and WAL ownership before cleanup.
The same proof applies to an already-desired file without a WAL, including
pre-removal promotion checks. No-op verification creates no new WAL. A conflicting
operation, changed file, stale/malformed proof or metrics outage cannot authorize
checkpoint deletion. Failed verification preserves a pending WAL; the caller's
deadline and the normal two-minute confirmation limit bound metrics waits.

Keep node configuration and its WAL untouched while restoring local metrics or
upgrading an unsupported API server. Never delete the WAL or lower its saved
counter manually to force progress. Restore the original operation's prerequisites
and retry it; retain the mutation owner and pending key until it is verified.
Loaded-config fingerprints stay inside the agent and must not be copied into
operator evidence. Kubernetes records the current config at startup and after a
successful reload ([startup record](https://github.com/kubernetes/kubernetes/blob/v1.34.2/staging/src/k8s.io/apiserver/pkg/server/options/etcd.go#L302),
[reload success record](https://github.com/kubernetes/kubernetes/blob/v1.34.2/staging/src/k8s.io/apiserver/pkg/server/options/encryptionconfig/metrics/metrics.go#L96-L101)).
Live acceptance still needs a controlled API server restart on the supported site;
local regressions exercise these RPC, metrics and filesystem states without a
deployed API server. Independent filesystem writers must remain quiesced because
they do not share the node agent's mutation mutex.

For retained-lock failures, select the `WorkflowTaskCompleted` event that scheduled
the specific failed mutation, using the Temporal UI/history, so earlier successful commands remain
recorded. Do not reset to the first workflow task as a general recovery shortcut.
The safe boundary depends on the phase:

| Failed phase | Recovery boundary and required checks |
|---|---|
| `prepare` | Confirm the recorded failure released the mutation lock, fix the prerequisite, and start a rotation with a **new workflow ID**, not a reset. Leave the key Secret unchanged; the new execution reuses an existing pending key and its name. |
| `add_secondary` | Before the failed node update. Keep the same pending key; prior successful nodes stay recorded. |
| `promote_primary` | Before the failed node promotion. Verify secondary distribution completed everywhere; do not remove either key. |
| `rewrite` | If the original rewrite remains running, reset after the acknowledged `StartEtcdEncryptionRewrite` result to resume polling with its recorded operation ID. This works only before the original run's six-hour poll deadline: the reset replays the deadline computed when Start was scheduled, so once six hours have passed since then, the reset run fails at once with "did not complete within 6h0m0s". The usual reason the rewrite is still running is that the original run hit that deadline. In that case wait for the old rewrite operation to become terminal on the executor node, then reset before Start so the reset run gets a new rewrite ID and a fresh deadline. If it failed terminally, reset before Start so the reset run gets a new rewrite ID. Verify the old operation is terminal before starting that ID. The recorded phase also stays `rewrite` while the workflow runs the post-rewrite membership check, pending-primary re-promotion and second membership check. If the rewrite already succeeded and one of those checks failed, reset before the failed check without starting a new rewrite. |
| `remove_old_keys` | Before the failed node removal. Verify the rewrite succeeded first; never reintroduce removed keys as a rollback. |
| `finalize` | Before `FinalizeEtcdRotationKey` or `CompleteEtcdEncryptionKeyRotation`, according to which failed. Key finalization is idempotent when pending state was already cleared. Do not replay key staging or earlier distribution. |

While the original run is open, an executor-agent process restart needs no
workflow reset: each poll that observes its matching `running` operation reissues
Start with that same ID. The agent treats this as a no-op while its rewrite is
active, resumes the persisted operation when its goroutine was lost, and preserves
terminal success or failure. Completion still requires the matching operation to
report `succeeded`; a missing, different, or unknown operation fails the activity
without starting a replacement.

A reset that preserves the acknowledged Start result also preserves that ID.
Resetting **before** Start generates a new ID from the new Temporal run ID, which
the agent rejects while the previous ID remains `running`. Do not use that
boundary to recover a process restart, even if the old agent has no live rewrite
goroutine. These reset boundaries follow the recorded activity inputs/results;
captured-history replay and a controlled reset on the supported site are still
required to qualify an operator's chosen event.

Resetting before key staging **after finalization has happened** could generate a
second pending key and start a different rotation under the old owner. Such a
reset is unsupported. A reset replay also does not rerun database writes from
successful checkpoint activities. The durable state may stay `failed` until an
actually executed later checkpoint or successful completion proves progress;
that is intentional. Completion accepts its failed `finalize` checkpoint under
the same owner and atomically clears the lock.

After an operator has reviewed the exact reset point, the command shape is:

```bash
temporal workflow reset \
  --address "$rotation_temporal_address" \
  --namespace "$rotation_temporal_namespace" \
  --workflow-id "$rotation_workflow_id" \
  --event-id "$reviewed_workflow_task_completed_event_id" \
  --reason "$reviewed_reset_rationale"
```

### Control-plane host lost during a rotation

Every phase, and every reset of it, needs all control-plane nodes in the
rotation's snapshot to be reachable and ready. A recorded `prepare` failure
releases the lock, allowing remediation to replace a lost host before starting
a rotation with a new workflow ID. From `add_secondary` on, the retained lock
prevents replacing a node until the rotation completes: a replacement joins with
the key Secret's active key alone, which stays the old key until finalization.

Keep the lock and take etcd backups from the surviving nodes. No database edit
releases the lock, not even in `add_secondary`: the recorded phase can move
back after a reset, so it does not prove that no node uses the new key. Restore
the lost host with the same node identity and reset the workflow as described
above, or restore the cluster from backup.

Record the original and reset run IDs, chosen event ID, rationale, and results.
Do not publish full history dumps from unrelated workflows; retain only reviewed,
sanitized rotation evidence. If the pending/active key state no longer matches
recorded history, retain the lock and perform expert recovery with backups.
For retained-lock failures, there is no automatic recovery API or unattended
lock-release procedure, and no supported database edit releases the lock. Only
successful completion of the owning workflow releases it.

## Prove completion and preexisting-object rewrite

Success requires all of the following, rather than only a completed workflow:

1. The active key fingerprint changed, active-since advanced, and the pending key
   was removed. Compute fingerprints inside trusted tooling; do not print key
   bytes or Secret manifests.
2. `etcdEncrypted` remains true; the recorded rotation is `succeeded` with its
   owner ID and completed timestamp; `updateInProgress` and `updateFailed` clear.
3. Every control-plane API remains ready, and kube-apiserver restart counts do
   not change. Continuous API traffic must remain within the site's agreed
   error/latency budget.
4. A fixture Secret created **before** rotation has unchanged contents and is
   readable through every API server **after** old keys are removed.
5. That fixture's raw etcd secretbox storage header identifies a different key
   after rotation, and freshly written probes through every API server use that
   same new header. This establishes both rewrite and primary-key convergence.
   Merely creating a new encrypted Secret after rotation does not prove rewrite.

The existing encrypted-cluster e2e specification now performs these checks using
a public fixture value. Its verifier keeps raw ciphertext in protected temporary
files and returns only a SHA256 of the public storage header. It does not print
keys, plaintext data, ciphertext, or storage key names.

With the local CAPD deployment and its required test images prepared, run the
complete three-control-plane-node encrypted-cluster lifecycle:

```bash
make -C controller test-e2e \
  ARGS='--fail-on-empty --label-filter="capd && encryption && !etcd-encryption-enable"'
```

For an existing qualifying cluster, set `lifecycle.reuseCluster` in the external
e2e configuration file (see [e2e configuration](../test/e2e/README.md#configuration)).
The same selected specs then skip create/delete and run against that cluster.
This command is mutating: run it only on the selected qualification environment.
The corresponding NICo container lane replaces `capd` with `nico-containers`.
Do not select only `etcd-key-rotation` for a fresh deployment: that skips the
required create specification.

## Evidence and qualification still required

Local regression coverage exercises owner/phase retry idempotency, Kubernetes
version admission, prepare-failure lock release and pending-key reuse, later-phase
failed-lock retention, delayed cleanup, reset finalization, all workflow failure
phases, legacy command paths, secret-safe error boundaries, and the verifier's detection
of unchanged keys, stale API primaries, payload/read/write failures, and plaintext.
Those tests establish code behavior, not live qualification.

Before accepting the rotation feature, retain sanitized evidence from the selected supported
site/topology for success under continuous traffic, changed active key, cleared
pending state, preexisting Secret rewrite and readability, all-node convergence,
and a controlled failure/recovery at a safe boundary. Record revisions, versions,
cluster/node IDs, timestamps, traffic summary, outcome, and checks that were not
executed. Link alert and audit evidence supplied by their corresponding feature
changes. Do not claim unsupported topologies or a live pass from mocked tests.
