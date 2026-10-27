{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}

# Node-agent readiness scheduling

The `node-agent-health` controller periodically calls the node agent to obtain
its version and reconcile `NodeAgentReady`. It uses a bounded worker pool and a
dedicated deadline for each reachability RPC. Settings come from the
`agentReadiness` block of `nvke-controller-config`'s `config.yaml`, loaded once at
startup. The Helm chart renders it from `nodeAgent.readiness` values:

```yaml
agentReadiness:
  workers: 8
  timeout: 30s
  interval: 2m
  jitter: 0.1
```

Workers, timeout and interval must be positive. Jitter must be finite and in
`[0,1)`; set `0` to disable it. The chart and host-local config supply the values
above; the file loader rejects omitted or zero workers and durations. A
programmatically constructed config still defaults zero workers and durations,
while zero jitter disables jitter.

The `NODE_AGENT_READINESS_*` environment variables are no longer read, and the
chart rejects them in its `env` list. Change the ConfigMap (or the file named by
`CONTROLLER_CONFIG_PATH` for host-local runs) and restart the controller to apply
new settings.

The interval is a delay after completion, **not a promise to visit every node
within two minutes**. Actual freshness includes queue wait and probe duration.
Initial inventory events can arrive together. Jitter spreads later requeues;
it does not spread that initial burst or impose a global request rate limit.
Database changes and the source's 15-minute inventory resync can enqueue nodes
before their periodic delay expires, producing additional bursts.
A sustained slow cohort can consume all workers, but each RPC attempt has a
bounded deadline. Database operations use the reconcile context; the RPC deadline
does not bound the entire reconcile. Tune the worker count and deadline against
fleet size, observed network/authentication latency, and database capacity.

`agentReadiness.timeout` controls agent reachability probes.
`NODE_HEALTH_CHECK_TIMEOUT` governs the separate tenant node health check.
A probe timeout marks the agent unready. Lifecycle controllers consume this
condition for readiness, patch selection, and remediation decisions.

The queue deduplicates keys and prevents concurrent reconciliation of the same
node. Readiness updates use transaction locking, deletion checks, and condition
comparison. Unchanged successful probes do not write to the database. Manager
shutdown cancellation returns without marking a node unready.

Metrics on the controller's Prometheus endpoint:

- `nvke_controller_node_agent_probes_active`: active agent RPCs, bounded by the workers in this process.
- `nvke_controller_node_agent_probe_duration_seconds{outcome="success|timeout|canceled|error"}`: RPC latency histogram, including transport setup.

Labels have no node identity, keeping cardinality bounded. Use histogram counts
for probe outcomes and the active gauge to see worker occupancy. Controller-runtime
queue metrics use a separate registry; exposing these probe metrics does not also
expose that queue registry.
