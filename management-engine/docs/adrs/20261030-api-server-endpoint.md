# Caller-supplied API server endpoint on NVKECluster

**Status:** proposed
**Date:** 2026-10-30

## Context

The NVKE controller rejects more than one control-plane node, upgrades, and control-plane node replacement unless the cluster was created with a `controlPlaneVIP` ([`control_plane_vip.go`](../../../controller/internal/server/control_plane_vip.go)). It accepts the VIP only at creation; its update request has no field for it ([`models.gen.go`](../../../controller/api/models.gen.go)). `NVKECluster` cannot pass one, and nothing in the repository allocates VIPs or checks they are unique.

## Decision

We will add an optional `spec.apiServerEndpoint.address`, sent to the controller as `controlPlaneVIP` on create, and report the address the controller holds in `status.apiServerEndpoint.address`.

- The address must be unicast IPv4 outside the pod and service networks. The caller ensures no other cluster uses it.
- The spec is editable until status reports an address, then fixed. If it still drifts from the controller, reconciliation stops and names the address to restore.
- Errors use the `ClusterReconciled` condition and never name another cluster or tenant.
- The field sits beside `apiServerAccess`, not inside it: one is where the API server is served, the other is who may reach it.

## Consequences

Clusters created through the cluster service can run highly available control planes and be upgraded.

Until uniqueness is enforced, one tenant can claim another's address and break its API server.

The cluster service creates the cluster on the first reconcile that passes validation, so the address is editable only while another part of the spec is being rejected. After that, a wrong address means recreating the cluster.

Later needs stay additive:

- **Managed allocation:** leave the spec unset and report the chosen address in status. This is why status has its own copy.
- **Hostname and port:** add them to the status type, which is separate from the spec type for this reason. The field is named `address` rather than `vip` for the same reason.
- **Public endpoint:** report it in status when `apiServerAccess.publicAccessEnabled` is true.
- **Uniqueness and per-tenant ranges:** validation only, with no API change.

## Options considered

- **Caller-supplied, fixed once reported (chosen):** matches what the controller needs without the service owning address lifecycles.
- **Service-allocated only:** simpler for users, but nothing in the repository allocates addresses.
- **`spec.controlPlaneVIP`:** smallest change, but ties the name to one mechanism and leaves no room for a hostname or port.
- **Fixed at first apply:** simpler, but blocks fixing a typo on a cluster that was never created.
- **Under `apiServerAccess`:** mixes location with access, and access settings can change after creation.
