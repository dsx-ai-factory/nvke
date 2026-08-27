# Node Lifecycle

## Overview

This document describes the normal lifecycle controls for worker nodes in NKE tenant clusters:

- **Auto-provisioning** keeps a node pool converged toward its desired count.
- **Auto-mode** marks nodes in deprecated NodeSets for recycle.
- **External break-fix systems** decide when workload-aware cordon, drain, and termination can happen.
- **Operators** can manually reboot or terminate individual nodes when needed.

> **Important:** NKE is not responsible for deciding when tenant workloads may be disrupted. Cordon, drain, and worker node SLO policy must be handled by the tenant SRE team and/or a break-fix system such as [NVSentinel](https://github.com/NVIDIA/NVSentinel) integrated with your workload scheduler. NKE provides the lifecycle APIs; policy enforcement is yours.

## Auto-provisioning

Auto-provisioning is NKE's desired-count reconciliation for node pools. It is **not** workload-driven autoscaling.

The node pool's `desiredNodeCount` is the target number of nodes. CWE continuously compares the desired count with the node records for the pool. When the pool is below desired count, CWE creates pending node records and provisions them if matching infrastructure capacity is available. If capacity is not available, the replacement waits until capacity returns.

"Matching capacity" means more than an idle physical node. CWE must find capacity that satisfies the node pool requirements including instance type, hardware characteristics, allowed subnet placement, and available IP space. A node pool can stay pending even when suitable hardware exists if the configured subnet or IP allocation is exhausted.

Auto-provisioning does **not** automatically remove healthy in-service nodes when the actual count exceeds the desired count. Scale-down still requires an explicit termination flow.

Auto-provisioning makes node replacement mostly hands-off after a reboot, repair, recycle, or manual termination. If the node pool desired count remains unchanged, a terminated worker is replaced when capacity is available.

## Adjusting a Node Pool's Desired Count

Use desired count when the intended steady-state size of the node pool changes. Do not use it as a workload autoscaler; it should reflect resource governance or an explicit operator decision.

### Increasing the Desired Count

1. Open the CWE UI.
2. Find the NodePool and click **Edit Node Pool**.
3. Increase the desired node count and save.

Nodes are automatically provisioned when capacity becomes available. Pending nodes in the UI represent desired capacity waiting for matching resources.

### Decreasing the Desired Count

1. Open the CWE UI.
2. Find the NodePool and click **Edit Node Pool**.
3. Lower the desired node count and save.

NKE does not automatically terminate healthy in-service nodes only because desired count was reduced. This can leave the pool with more nodes than desired. To complete the scale-down, terminate selected nodes using [Terminating a Node](#terminating-a-node).

> **Warning:** Only terminate active worker nodes when the tenant disruption policy allows it. Prefer a maintenance window or nodes that are not running tenant workloads.

## Auto-mode Recycling

Auto-mode gradually replaces worker nodes in deprecated NodeSets. CWE marks eligible nodes with `nke.nvidia.com/NeedsRecycle`, and [NVSentinel](https://github.com/NVIDIA/NVSentinel) (or your break-fix system) decides when those nodes can be cordoned, drained, and terminated.

## Rebooting a Node

If rebooting a node is required, use one of the following approaches:

1. **Via [NVSentinel](https://github.com/NVIDIA/NVSentinel)** — preferred when the node is being recycled or repaired, as this gives NVSentinel the opportunity to apply workload-aware cordon and drain policy before the reboot.
2. **Via the CWE UI** — find the node and use the reboot action.
3. **Via `nkectl`** — `nkectl` provides cluster and node-level operations.

## Terminating a Node

Manual node termination should be the exception. Prefer the [NVSentinel](https://github.com/NVIDIA/NVSentinel) break-fix flow when the node is being recycled or repaired — that path applies workload-aware policy before termination.

Two approaches:

1. **Via the CWE UI** — find the problematic node and click **Delete Node**. If the NodePool desired count remains unchanged, terminated nodes are automatically replaced when matching capacity becomes available.

2. **Via `nkectl`** — connect to the cluster and use the node termination command.

If the node is still present in Kubernetes but is already gone from CWE, it is a Kubernetes-only zombie node. Remove it manually:

```bash
kubectl delete node <node-name>
```

## Troubleshooting Node Lifecycle

**Node stuck in Pending state:**

- Check CWE logs for capacity allocation errors.
- Verify the configured subnet has available IP space.
- Confirm the infrastructure provider (NICo/Forge) has matching capacity.

**Node not joining after provisioning:**

- Check `nke-agent` on the node: `systemctl status nke-agent`
- Verify network connectivity from the site controller to the node on port 15005 (gRPC)
- Check CWE workflow logs in the Temporal UI (port-forward `temporal-web` in the site controller cluster)

**Node shows Ready in Kubernetes but NKE considers it unhealthy:**

- NKE tracks node state in its own database. Check CWE UI for the node's recorded state.
- CWE reconciles the node's Kubernetes state with NKE's records; check CWE logs if there's a discrepancy.
