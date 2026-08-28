# Node-agent desired-state in-place rollout

> Status: draft for review
>
> Tracks: [NVIDIA/nke#205](https://github.com/NVIDIA/nke/issues/205)

## Problem

NKE records each node's observed agent version, but it has no cluster-scoped
desired agent configuration or controller that converges nodes to it. APT also
cannot safely replace the agent from inside `nke-agent.service`: the Debian
`prerm` stops that service, so systemd can terminate the package transaction.

**Solution summary:** `UpdateCluster` persists a required control-plane version
and an optional worker version. A DB-backed controller resolves the site-wide
APT or bundle method from CWE configuration, then uses the existing in-place
rollout framework one node at a time, control-plane first. Control-plane rollout
holds the cluster update lock; worker rollout does not. The agent RPC delegates
self-replacement to an independent systemd updater.

## Architecture sequences

### 1. API and database model

```mermaid
sequenceDiagram
    autonumber
    actor Client as Internal API client
    participant API as UpdateCluster API
    participant DB as PostgreSQL
    participant Reconciler as NodeAgentUpgrade reconciler

    Client->>API: PATCH role versions<br/>controlPlane required<br/>workers optional
    API->>DB: Lock cluster row and read UpdateInProgress
    alt Another cluster update is active
        API-->>Client: 409 Conflict
    else Update is admitted
        API->>DB: Persist desired config + next generation + Pending
        opt Control-plane target changes or is retried
            API->>DB: Set node_agent_upgrade lock<br/>with generation + owner
        end
        API-->>Client: 200 NodeAgentUpgrade=Pending
        DB-->>Reconciler: Desired-state change event
    end
    Note over DB: Cluster rows hold desired versions<br/>Node rows hold observed versions
    DB-->>Reconciler: Observed node version change event
```

### 2. Reconciliation and rollout orchestration

```mermaid
sequenceDiagram
    autonumber
    participant Reconciler as NodeAgentUpgrade reconciler
    participant DB as PostgreSQL
    participant Temporal as Temporal workflow
    participant Rollout as In-place rollout executor
    participant NodeOp as Per-node operation

    Reconciler->>DB: Load role target, generation,<br/>observed versions, and phase state
    alt This role phase is Failed for the generation
        Reconciler-->>DB: No action; wait for explicit API retry
    else The role has drift
        Reconciler->>DB: Record Running + owner + CWE method<br/>and write workflow outbox
        opt Control-plane phase
            Reconciler->>DB: Confirm generation-owned cluster lock
        end
        DB-->>Temporal: Start workflow from committed outbox
        Temporal->>Rollout: Serial + halt on failure
        loop One node at a time
            Rollout->>NodeOp: Apply the node's role target
            NodeOp-->>Rollout: Applied, Unchanged, or Failed
        end
        alt Every node succeeds
            Temporal->>DB: Record Succeeded if fence matches<br/>Clear matching control-plane lock
        else A node fails
            Rollout->>NodeOp: Roll back prior nodes in reverse order
            NodeOp-->>Rollout: Restoration outcomes
            Temporal->>DB: Record terminal Failed if fence matches<br/>Clear CP lock only if every node is restored
        end
    end
    Note over DB,Temporal: Status writes and lock clears compare generation + owner<br/>Stale attempts cannot mutate current state
```

### 3. Per-node agent upgrade and restore

```mermaid
sequenceDiagram
    autonumber
    participant Operation as CWE per-node operation
    participant RPC as nke-agent upgrade RPC
    participant Systemd as systemd
    participant Updater as Independent updater service
    participant Agent as nke-agent.service

    Operation->>RPC: Start target version + CWE-configured method
    RPC->>RPC: Persist idempotent request and status
    RPC->>Systemd: Start updater without blocking
    Systemd->>Updater: Run outside the agent service cgroup
    alt APT
        Updater->>Agent: Install the staged exact package
    else Bundle
        Updater->>Agent: Install the staged exact bundle
    end
    Updater->>Agent: Restart and verify the requested version
    alt Verification fails
        Updater->>Agent: Restore the exact previous version
        Updater->>Updater: Record FailedRestored or FailedNotRestored
    else Verification succeeds
        Updater->>Updater: Record Succeeded
    end
    Operation->>RPC: Poll durable status<br/>Retry restart disconnects
    alt Requested or restored version is reported
        Operation->>RPC: New SPIFFE mTLS connection + GetKubeadmStatus
        RPC-->>Operation: Fresh running agent_version
    else Node is not restored
        RPC-->>Operation: FailedNotRestored
    end
```

## Decision and alternatives considered

The sources of truth remain separate:

| Concern | Owner |
|---|---|
| Qualified artifacts, compatibility, and provenance | Release metadata and SBOM |
| Desired version for each cluster role | `Cluster.nodeAgentConfig` |
| APT or bundle delivery method for the site | CWE configuration |
| Observed running version | Node status in the CWE database |
| Safe convergence and attempt history | Reconciler and Temporal rollout |

An SBOM or OS image can describe or seed a release, but cannot express a
per-cluster target or converge retained nodes. ArgoCD is not the owner because
the host systemd agent must remain manageable when Kubernetes is unavailable.
A node-local auto-updater would lose coordinated serial rollback; an imperative
upgrade endpoint would not preserve intent for replacement or drifted nodes. A
separate upgrade resource was not selected because the target is durable cluster
configuration rather than an independently approved or scheduled operation.

## Non-goals

- Parallel node-agent upgrades.
- Node-agent artifact publication or qualification.

## Existing integration points

- `nodeagentreadiness` gets `agent_version` from `GetKubeadmStatus` and writes
  `nodes.agent_version`.
- `UpdateCluster` already locks the cluster row and uses
  `conditions.updateInProgress` to serialize cluster updates.
- `dbreconcile/dbwrapper` publishes cluster and node changes to DB-backed
  controller sources and periodically resyncs them.
- `controller/internal/rollout` already provides serial execution,
  `RollbackHaltOnAnyFailure`, local restore outcomes, and reverse-order
  rollback. Its target selection currently includes control-plane nodes only.

## API and persisted state

### UpdateCluster contract

Add `nodeAgentConfig` to `UpdateClusterRequest` and the `Cluster` response:

```json
{
  "nodeAgentConfig": {
    "controlPlane": {
      "version": "v0.1.31"
    },
    "workers": {
      "version": "v0.1.30"
    }
  }
}
```

- Omitting top-level `nodeAgentConfig` from an update request leaves the current
  desired state unchanged. When it is supplied, `controlPlane` is required and
  `workers` is optional.
- Each supplied role requires only `version`. The API does not expose the
  installation method.
- Omitting `workers` from a supplied config clears any worker target and stops
  worker reconciliation.
- Setting top-level `nodeAgentConfig` to `null` clears all targets when no
  node-agent rollout is running.
- Accept exact release SemVer and canonicalize it to `vMAJOR.MINOR.PATCH`.
  Downgrades are allowed. An identical canonical config is a no-op unless the
  current control-plane or worker phase is `Failed`; resubmitting it then
  explicitly starts a new generation.
- Return `409` when another `UpdateInProgress` operation owns the lock.
- On change or explicit retry, one transaction increments the desired-state
  generation, persists the desired versions, and records
  `nodeAgentUpgrade.state=Pending`. It sets
  `UpdateInProgress=node_agent_upgrade` when the control-plane target changes or
  its failed phase is retried. The controller acquires the same fenced lock for
  later control-plane drift, such as a replacement node. Worker-only changes do
  not set the cluster lock. Return `200` with the node-agent state; the HTTP
  handler does not start Temporal.

### CWE update method

A required site-scoped CWE setting, `NODE_AGENT_UPDATE_METHOD`, selects `apt` or
`bundle`. The reconciler validates it and snapshots the resolved value into each
workflow and node operation. Changing this setting does not reinstall an agent
or alter an in-flight workflow; it applies to the next desired-state generation.
Artifact locations and qualification remain release configuration, not API
input.

### Database model

Add cluster columns:

```text
desired_control_plane_node_agent_version text NOT NULL DEFAULT ''
desired_worker_node_agent_version        text NOT NULL DEFAULT ''
node_agent_config_generation             bigint NOT NULL DEFAULT 0
```

The existing `nodes.agent_version` remains actual state. Add
`nodes.agent_version_observed_at` so pre-restart observations cannot satisfy
post-upgrade verification.

The cluster's `nodeAgentUpgrade` condition stores the desired-config snapshot,
resolved method, generation, and separate control-plane and worker phase status.
Each phase stores its workflow owner ID, `Pending | Running | Succeeded |
Failed`, attempt timing, and a sanitized message. `Failed` is terminal for that
role phase and generation. `UpdateInProgress` remains the shared cluster
mutation lock; add `node_agent_upgrade` to its operation-type enum for the
control-plane phase only. Its entry also carries the generation and owner ID
used to fence lock release.

## Reconciliation and topology lock

Add a cluster-scoped `nodeagentupgrade` controller:

1. Reconcile on cluster desired-state changes, mapped node actual-state changes,
   and the existing periodic resync.
2. Return when the config is absent or the cluster is not provisioned or is
   deleting. Do not launch work for a role phase that is `Failed` in the current
   generation; a failed control-plane phase also prevents the worker phase.
3. Target provisioned, non-deleting control-plane nodes. Include workers only
   when `nodeAgentConfig.workers` is present. Wait while a target node is
   provisioning, deleting, recycling, or under repair.
4. Compare each target with its role's exact version and require a fresh
   observation. Reconcile control-plane nodes before starting the optional worker
   phase.
5. On control-plane drift, atomically acquire the cluster update lock with the
   generation and workflow owner ID. Worker drift does not acquire that lock.
6. Record `Running`, snapshot `NODE_AGENT_UPDATE_METHOD`, and write the workflow
   outbox entry. The workflow rechecks node lifecycle, agent readiness, and RPC
   support before changing each node.
7. Every workflow status write and lock clear performs a compare-and-swap on the
   cluster ID, desired generation, and workflow owner ID. A zero-row update means
   the workflow is stale; it stops without changing status or releasing a lock.

Temporal may perform bounded activity retries inside one running workflow. Once
they are exhausted and the workflow finishes unsuccessfully, the controller
records the role phase as `Failed` and does not create a replacement workflow
for that role and generation. After correcting the cause, an operator must
resubmit either the same desired config or a new one through `UpdateCluster`;
admission increments the generation and creates a new attempt. A failed worker
phase does not prevent later control-plane drift from converging. New nodes are
reconciled using the configured target unless their role phase is `Failed`.

The cluster-wide topology lock applies only to the control-plane phase. While it
is active, API handlers and reconcilers block scaling, provisioning, scheduling,
recycling, repair, replacement, and deletion. Health and status observation
continue. Clear the lock only when the matching fenced workflow converges or
fully restores every affected control-plane node. Retain it when any such node
is not proven restored.

A failed-and-restored control-plane phase clears its matching lock but remains
`Failed` until explicit retry. `FailedNotRestored` retains the lock. In that
case, a separate administrative recovery action must restore and verify every
affected control-plane node, then compare-and-swap the same generation and owner
before clearing the lock. Periodic reconciliation never clears a failed lock.

Worker rollout is serial but does not set `UpdateInProgress` or block unrelated
cluster mutations. It rechecks the selected worker's lifecycle immediately
before each node operation and stops the current attempt if that node or the
desired generation changed. A worker failure is terminal for its role phase and
generation but never retains a cluster-wide lock.

## In-place rollout orchestration

Extend rollout target selection without changing the control-plane default used
by etcd encryption and OIDC:

```text
TargetControlPlane
TargetWorkers

Strategy:       Serial
RollbackPolicy: HaltOnAnyFailure
```

Run control-plane and worker rollout as separate role phases. The worker phase
starts only after control-plane convergence and does not inherit the
control-plane cluster lock. Order each role by node name. Snapshot the desired
generation, resolved method, role target, and workflow owner when a phase starts;
derive each per-node operation ID from the rollout and node IDs.

For each target node, the rollout operation:

1. Reads the live running version and returns `OutcomeUnchanged` on an exact
   match.
2. Starts the upgrade and polls durable status. Agent disconnects during restart
   are retryable.
3. On success, reconnects over SPIFFE mTLS, calls `GetKubeadmStatus`, requires
   the exact target version, and persists `agent_version` with a fresh
   `agent_version_observed_at`.
4. Maps a verified local restore to `OutcomeFailedRestored`; otherwise it
   returns `OutcomeFailedNotRestored`.

The first failure stops forward progress. Even `OutcomeFailedRestored` causes
all previously applied nodes in that role phase to roll back in reverse order.
A worker failure does not roll back an already-converged control plane. Clear a
control-plane cluster lock only when its generation and owner still match and
every affected control-plane node is proven restored.

## Agent RPC and independent updater

Add narrow asynchronous RPCs:

```text
StartNodeAgentUpgrade(operation_id, target_version, method)
GetNodeAgentUpgradeStatus(operation_id)
StartNodeAgentRollback(operation_id, upgrade_operation_id)
```

`method` is `APT` or `BUNDLE` and is supplied only by the controller from its CWE
configuration; it is not cluster API input. The RPCs accept no package name,
URL, path, install arguments, or command. `InstallPackages` must reject
`nke-agent`, and `InstallBundle` must reject the `agent` component. Capability
preflight must not fall back to either generic RPC.

The same operation ID and request returns existing status; different inputs for
the same ID return `AlreadyExists`. A concurrent updater operation returns
`FailedPrecondition`. Status remains readable after agent restart and records
the action, method, previous and target versions, phase, outcome, artifact
identity, timestamps, and sanitized error.

The RPC atomically writes the request and initial status under
`/var/lib/nke/agent-upgrades/<operation-id>/`, then runs:

```text
systemctl start --no-block nke-agent-updater@<operation-id>.service
```

PID 1 starts `nke-agent-updater` in a separate service cgroup. The updater owns
staging, installation, restart, local verification, and local restore. A
singleton lock prevents overlapping jobs, and atomic status writes make retries
and updater restarts resumable without depending on the gRPC server process.

### APT method

Before stopping the agent, resolve and verify the exact target and rollback
`.deb` files. Install the target noninteractively with `--allow-downgrades`, then
verify `dpkg-query`, the installed binary version, and
`nke-agent.service`. The configured repository suite and APT select the node's
OS release and native architecture; the RPC supplies only the exact version. On
failure, restore and verify the exact previous package.

### Bundle method

Before stopping the agent, verify the exact bundle and snapshot the current
binary and agent-owned service configuration. Use a fixed agent-owned entrypoint
without caller arguments and preserve `/etc/default/nke-agent`. On failure,
restore the snapshot, reload systemd, restart the service, and verify the exact
previous version.

The updater reports `FailedRestored` only after verifying the previous version;
otherwise it reports `FailedNotRestored`.

## Observability

Track completed node-agent upgrades with one bounded outcome label: `success`
or `failure`.

## Compatibility and bootstrap

Existing agents reach the first release containing the RPC and updater through
the one-time bash-based exact-version APT bootstrap runbook. Afterward, the
controller manages upgrades and downgrades. Clusters without `nodeAgentConfig`
remain unchanged, and preflight fails before mutation if any targeted node lacks
the dedicated RPC.
