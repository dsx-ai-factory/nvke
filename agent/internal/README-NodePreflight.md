# ExecuteNodePreflight — Node Readiness Checks

Design for the `ExecuteNodePreflight` gRPC RPC added to the NVKE agent (May 2026).

## Problem

Cluster provisioning workflows need to know when a node is actually ready for kubeadm
and package operations—not just when the agent process is listening.

The NVKE controller gates first on agent reachability (for example via
`GetAgentVersion` / NodeAgentReady conditions). That is necessary but
insufficient: cloud-init may still be running `runcmd` steps (package installs,
sysctl, service restarts) after the agent starts. Proceeding too early produces
flaky kubeadm preflight failures or half-configured nodes.

We need a remote way to assert bootstrap completion (and optionally run kubeadm
preflight) in a single agent call.

## Solution

`ExecuteNodePreflight` runs:

1. **Optional cloud-init check** — set `check_cloud_init_status` on the request.
2. **Optional file checks** — caller-defined `file_exists` assertions in `checks[]`.
3. **Optional kubeadm preflight** — embedded `kubeadm init|join phase preflight`.

`ExecuteKubeadmPreflight` remains as a standalone RPC for callers that only need
kubeadm checks. Both RPCs share a `runKubeadmPreflight` helper.

## API

### Request (`ExecuteNodePreflightRequest`)

| Field | Type | Description |
|-------|------|-------------|
| `check_cloud_init_status` | `CheckCloudInitStatus` | When set, agent runs `cloud-init status` |
| `checks` | `repeated NodePreflightCheck` | Optional `file_exists` checks |
| `kubeadm_preflight` | `KubeadmPreflightRequest` | Optional kubeadm init/join preflight |

At least one of `check_cloud_init_status`, `checks`, or `kubeadm_preflight` must be set.

```protobuf
message CheckCloudInitStatus {}

message NodePreflightCheck {
  string name = 1;  // optional; defaults derived from check type/path
  oneof check {
    FileExistsPreflightCheck file_exists = 2;
  }
}

message FileExistsPreflightCheck {
  string path = 1;  // must be absolute
}
```

### `check_cloud_init_status` behavior

The agent runs `cloud-init status` and interprets output:

| Output | Agent `status` | Caller action |
|--------|----------------|---------------|
| contains `status: done` | `ready` | Proceed |
| contains `status: running` | `notready` | Poll / retry |
| contains `status: error` | `error` | Fail (permanent) |

The result name is always `cloud-init-status`.

### Response (`ExecuteNodePreflightResponse`)

| Field | Description |
|-------|-------------|
| `status` | Aggregate result (see below) |
| `results` | Per-check outcomes (`name`, `passed`, `message`) |
| `kubeadm_preflight` | Full `KubeadmPreflightResponse` when requested |
| `error_message` | Human-readable summary when status is not `ready` |

**Status values**

| Status | Meaning |
|--------|---------|
| `ready` | All requested checks passed |
| `notready` | A node check failed transiently (e.g. cloud-init still running) |
| `error` | Invalid request, I/O failure, cloud-init error, or kubeadm preflight error |

### Error handling convention

Like `ExecuteKubeadmPreflight`, validation and check failures return **gRPC OK**
with `status = "error"` (or `"notready"`) and `error_message` set—not a gRPC
error status. This keeps orchestration logic in the response body and avoids
Temporal retry loops on expected "not ready yet" outcomes.

Only unexpected agent failures propagate as RPC errors.

## Execution flow

```
Caller (controller activity)
        │
        ▼
ExecuteNodePreflight
        │
        ├─► If check_cloud_init_status set
        │       └─► cloud-init status
        │
        ├─► For each check in checks[]
        │       └─► file_exists → os.Stat(path)
        │
        └─► If kubeadm_preflight set
                └─► runKubeadmPreflight()
        │
        ▼
Aggregate status + error_message
```

**Status aggregation rules**

- Start at `ready`.
- Any failed node check → `notready` (checks still run to completion).
- Kubeadm preflight `error` → top-level `error` (overrides `notready`).

## Security and path handling

`file_exists` paths must be **absolute**. When `NodeAgentServer.RootPath` is set
(unit tests), paths are resolved under that root and rejected if they escape it.
Production agents leave `RootPath` empty and stat the real filesystem.

## Example usage

**Default (what the NVKE controller sends for typical nodes):**

```json
{
  "check_cloud_init_status": {}
}
```

**Legacy sentinel file** (profiles that `touch /run/cloud-init.done`):

```json
{
  "checks": [
    {
      "name": "sentinel",
      "file_exists": { "path": "/run/cloud-init.done" }
    }
  ]
}
```

**Cloud-init + kubeadm init preflight:**

```json
{
  "check_cloud_init_status": {},
  "kubeadm_preflight": {
    "config": "<rendered kubeadm ClusterConfiguration YAML>",
    "role": "INIT"
  }
}
```

See the main [README](../README.md#executenodepreflight-grpcurl) for `grpcurl` examples.

## NVKE controller integration

The controller calls this RPC from the `NodePreflight` Temporal activity in
`controller/internal/temporal/activities/node_preflight.go`.

**Sequence per node**

1. **`GetAgentVersion`** — confirms the agent process is reachable and returns
   its version.
2. **Version gate** — agents older than `v0.1.24` skip strict preflight (no
   `CheckCloudInitStatus` support) and treat reachability as sufficient.
3. **`ExecuteNodePreflight`** — typically with `check_cloud_init_status: {}`
   (CAPD/docker-machine nodes may use a sentinel `file_exists` check instead;
   see `nodePreflightRequestForNode` in that activity).

Polling, timeouts, and whether a prolonged `status: running` cloud-init outcome
is treated as soft vs hard failure are owned by the controller activity /
workflow layer, not by the agent.

## Files

| File | Role |
|------|------|
| `pkg/agent/proto/node_agent.proto` | RPC and message definitions |
| `internal/agent/preflight.go` | `ExecuteNodePreflight` and check implementations |
| `internal/agent/agent.go` | Shared `runKubeadmPreflight` |
| `internal/agent/preflight_test.go` | Unit tests |
| `controller/internal/temporal/activities/node_preflight.go` | Controller caller |

## Out of scope (follow-ups)

- Additional check types beyond `check_cloud_init_status` and `file_exists`
- Polling/retry policy (belongs in the orchestrator, not the agent)
- Deprecating `ExecuteKubeadmPreflight` (keep for now)
