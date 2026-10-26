# Node pool lifecycle: design

> **Status:** draft for review. Proposals, not commitments.

## Problem

Operators need to run a node pool's whole lifecycle through the API and `nvkectl`:

- Create a nodepool with default labels, taints, size bounds, an instance type and an image
- Scale and update it
- Remove a chosen node
- Get a clear error for an invalid request (ex: adding a node above maxNodeCount)
- Upgrade it without dropping below a minimum size
- Delete a nodepool

Most of this exists. This document covers the gaps, choosing the smallest change that meets each requirement.

## Summary

| Requirement | Today | Change |
|---|---|---|
| Size bounds | None | `minNodeCount`, `maxNodeCount` |
| Default labels and taints | Set at creation only | Updatable (will not automatically roll nodes) |
| Change image or version | API only, unchecked | `nvkectl patch nodepool`; validation (will not roll nodes) |
| Remove a chosen node | `DELETE /nodes/{id}`, then the pool replaces it | Optionally shrink the pool; `nvkectl delete node` |
| Reject invalid requests | Some checks | Bounds, release, version, node-in-pool |
| Upgrade keeps minimum size | Auto mode marks old nodes; someone else deletes them | Auto mode can replace them, bounded by `minNodeCount` |
| Delete pool | Works | None |

## Non-goals

- Autoscaling. The bounds validate requests; NVKE never changes `desiredNodeCount` itself. Potential future improvement.
- Changing instance type, partitions or SSH key groups in place. Add a new pool instead. Should this be scoped in?
- Control-plane pools.
- Updating LaunchLayer pools. Their provider fields are fixed at creation, so image changes are NICo only. Can this be a future improvement?
- Applying label or taint changes to nodes already in the pool. Updates will not roll nodes, they'll gain them if deleted-and-readded.

## Design

### Size bounds

Add `minNodeCount` (default 0) and `maxNodeCount` (default none) to create, update and read. After a create or update, the pool must satisfy `minNodeCount ≤ desiredNodeCount ≤ maxNodeCount`; otherwise the API returns `400` naming the field and the pool is unchanged. Checking the merged pool, not the request, means a patch can change bounds and size together. Bounds are not allowed on control-plane pools, and existing pools read as 0 and none.

`maxNodeCount` can be changed but not cleared. The API's PATCH semantics table makes `null` a `400` on every field and says `null` never clears a value. Clearing with `null` would make `maxNodeCount` the one exception. `0` cannot stand for "no bound", because `0` means a literal zero elsewhere in the API (`maxSurge`, `recycleBudget`). Until the bound can be cleared, an operator can set a large value instead.

Supporting `null` is a small change if reviewers accept the exception:

- Mark `maxNodeCount` `nullable: true` in the update request only.
- Give the field a Go type that tells `null` from an omitted field.
- Clear the bound on `null` in the handler.
- Let `nvkectl patch nodepool`, which refuses `null` today, send it for this field.
- Add the exception to the PATCH semantics table.

### Label and taint updates

Add `nodeLabels` and `nodeTaints` to the update request. A supplied value replaces the stored one, `{}` and `[]` clear, and an omitted field is unchanged. Validation matches creation.

The controller already reads a pool's labels and taints when each node joins, so new and replacement nodes get the change without a rollout or a new NodeSet. Nodes already in the pool keep theirs. Changing them in place would need paced eviction for `NoExecute` taints and a record of which keys NVKE owns.

### Image and version checks

`nico.release` and `kubernetesVersion` are already updatable and roll the pool through a new NodeSet. `nvkectl patch nodepool` gains both, and the update path gains the checks it lacks:

- `nico.release` must name an existing `NodePoolRelease`. The API server already has a management-cluster reader for the release endpoints. The check reuses it with the lookup provisioning already does (`machinerelease.ResolveNodePoolRelease`).
- `kubernetesVersion` must be within the Kubernetes version-skew window: no newer than the control plane, and at most 3 minor versions behind it. This is the window the control-plane upgrade already checks against worker pools. A lower version than the pool's current one stays allowed, because any version change creates a new NodeSet, so a pool can roll back today. The API description says the version "must be newer than the current version", but nothing has ever enforced that, so the description changes to match.

### Removing a chosen node

`DELETE /nodes/{id}` gains two optional body fields:

- `decrementDesiredNodeCount` lowers the pool's `desiredNodeCount` by one in the same transaction as the delete, so the pool does not replace the node. The API returns `400` if this would go below `minNodeCount`. The field is ignored for control-plane nodes.
- `nodePoolID` makes the API return `400` if the node is not in that pool. Without it, the API has no pool context and can only return `404` for an unknown node.

Cordon, drain, `forceUnreachable` and `machineHealthIssue` are unchanged. A separate pool-scoped endpoint was considered and rejected: it would be a second way to delete a node and would add nothing these fields lack.

`nvkectl delete node <name> --cluster <cluster> --pool <pool> [--replace]` resolves the node within the pool and always sends `nodePoolID`. Without `--replace` the pool shrinks; with it, the pool replaces the node.

**Default: not final or decided.** The proposed default for `decrementDesiredNodeCount` is `true`. That changes a bare `DELETE`, which today replaces the node. Callers that rely on replacement, such as break-fix automation, would have to send `false`, and the API compatibility check cannot detect this. Defaulting to `false` keeps existing behavior and makes shrinking opt-in. See open question 1.

### Upgrade without dropping below the minimum

Today a version or release change creates a new NodeSet, and `maxSurge` lets the pool add new nodes ahead of removals. Auto mode marks up to `recycleBudget` old nodes `NeedsRecycle` at a time but never removes them; an operator or a break-fix system deletes them. Nothing enforces a minimum.

Add a pool field `recycleAction: Mark | Replace`, default `Mark`, which keeps today's behavior. With `Replace`, auto mode deletes the nodes it selects instead of marking them. It uses the controller delete path the provisioning monitor already uses (`nodes.DeleteWithTx`, desired count unchanged), so each node is drained as in a manual delete and replaced on the new NodeSet. Two changes to auto mode's selection:

- Exclude nodes already being deleted, which today's query does not need to do.
- Cap each pass so that available nodes minus selected nodes stay at or above `minNodeCount`. "Available" means Ready, schedulable and not deleting, the set auto mode already counts.

If `maxSurge` is 0 and `minNodeCount` equals `desiredNodeCount`, no node can ever be removed. So the update rejects `Replace` unless `desiredNodeCount - minNodeCount + maxSurge ≥ 1`, naming `recycleAction`.

This is the largest change: one field, its validation, and a delete path and a cap in auto mode. It adds no new controller.

## Compatibility

- All new fields are optional and additive.
- The node delete default is a behavior change; see **Removing a chosen node**.
- The release and version checks reject updates that would otherwise fail at provisioning or break version skew. Rolling a pool back to a lower version stays allowed.
- Size bounds and `recycleAction` add columns whose defaults keep existing behavior: `min_count` 0, `max_count` null, `recycle_action` `Mark`.

## Testing

- Unit tests for each change.
- `demo.sh` runs against the local Tilt stack, using the Cluster API Docker provider on kind. It prints one pass or fail line per scenario and exits non-zero on any failure.
- The local stack has no NICo or LaunchLayer, so NICo release handling is covered by unit tests only. `controller/nico-sim` may let the demo cover NICo pools; that is still to be checked.

## Open questions

1. **Node delete default.** Should `decrementDesiredNodeCount` default to `true` or `false`?
2. **Instance types.** The controller creates Cluster API NICo provider (CAPNICO) Machines and does not call the NICo API. An unregistered instance type is therefore reported asynchronously, as `InstanceTypeNotFound` on the NicoMachine, rather than rejected at the API. Does that satisfy the "unregistered instance type" scenario, or should NVKE add a NICo API lookup and the client dependency that comes with it?
3. **Upgrade scope.** Is `recycleAction: Replace` needed in the first pass? The alternative is to document the manual procedure: delete marked nodes one at a time, keeping `minNodeCount` available.
