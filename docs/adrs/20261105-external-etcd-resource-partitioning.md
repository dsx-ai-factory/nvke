# etcd Resource Sharding

**Status:** proposed
**Date:** 2026-11-05

## Context

NVKE's control plane currently uses the
[stacked topology](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/ha-topology/#stacked-etcd-topology):
each control-plane host runs an API server and a local etcd member, and the etcd
members form one replicated datastore. This upstream kubeadm diagram illustrates
this arrangement:

![Kubeadm stacked etcd topology: three control-plane hosts each run an API server and a local etcd member, with workers accessing the API through a shared endpoint.](https://kubernetes.io/images/kubeadm/kubeadm-ha-topology-stacked-etcd.svg)

NVKE provides the diagram's load-balancing function through
[kube-vip's BGP advertisements](../../agent/internal/agent/kube_vip.go) and the
fabric's ECMP routing. Route advertisement is gated on the local API server's
`/readyz`. The NVKE controller's
[kubeadm configuration renderer](../../controller/internal/kubeadmconfig/init.go)
selects the local etcd process, and the agent's
[membership operations](../../agent/internal/agent/etcd_member_removal.go)
connect to that local endpoint using kubeadm certificates. This design couples
API-server hosts to etcd membership and lifecycle.

From a scalability perspective, NVKE needs to support tenant clusters initially
targeting 5,000 to 10,000 worker nodes, with the expectation that this number
continues to grow. Publicly published large-cluster deployments and upstream
scalability guidance have already achieved these numbers and leverage horizontal
API-server scaling and resource-type etcd sharding as techniques for managing
control-plane load.

### Background

The two main control-plane scalability bottlenecks addressed by this decision
are:

1. **etcd write and storage contention:** addressed by sharding resource types
   across independent etcd clusters.
2. **kube-apiserver processing, inflight request concurrency and connection
   capacity:** addressed by horizontally scaling kube-apiserver replicas.

### Sharding etcd by Resource Type

etcd traffic grows: at the default
[ten-second Lease renewal interval](https://kubernetes.io/docs/reference/node/node-status/#heartbeats),
5,000–10,000 nodes generate approximately 500–1,000 Lease updates/s not even
considering other traffic. etcd's write commit latency is constrained by
[network round-trip time and durable disk synchronization](https://etcd.io/docs/v3.6/op-guide/performance/),
so writes are typically latency-bound when disk bandwidth is not exhausted.

Sharding by resource type distributes this work across independent etcd
clusters, each with its own consensus and storage paths. This can increase
aggregate write capacity and reduce contention between resource types, but does
not remove each shard's network and disk latency constraints. Kubernetes'
[large-cluster guidance](https://kubernetes.io/docs/setup/best-practices/cluster-large/)
identifies a separate Events store as a performance option.

Kubernetes routes unassigned resources to the default store configured by
`--etcd-servers` and supports per-resource routing through
[`--etcd-servers-overrides`](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-apiserver/),
using `group/resource` mappings. Each mapping assigns an entire resource type to
a store; it does not partition Pods or Leases by namespace or object key. The
override mechanism applies to resources compiled into the API-server binary.

> Note: Adding API replicas does not increase etcd write capacity. Adding etcd
> voting members also does not partition writes: each member replicates the same
> data, and additional replication can
> [reduce write performance](https://etcd.io/docs/v3.6/faq/#what-is-maximum-cluster-size).

### Horizontally Scaling kube-apiserver Replicas

As cluster size increases, kube-apiservers receive more load from additional
node and object counts in the form of processing requests, maintaining more
client watches, and sending more updates. This is on top of the work it does in
authentication, authorization, admission, and serialization. kube-apiservers can
be scaled vertically (given more CPU/memory), however each replica also limits
inflight work through
[API Priority and Fairness](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/),
allowing for excess requests to be queued or rejected with HTTP 429. Since slow
requests occupy capacity longer, concurrency limits can become a bottleneck even
before CPU saturates. Horizontally scaling kube-apiserver distributes processing
and connections and adds aggregate request concurrency when traffic reaches the
new replicas.

## Decision

We believe NVKE will need an external-etcd topology as tenant clusters grow to
10,000 nodes and beyond, so that API-server replicas can scale independently of
etcd voting membership. The
[upstream kubeadm external topology](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/ha-topology/#external-etcd-topology)
places etcd members on separate hosts, as illustrated below. The diagram shows
one etcd cluster; resource-type sharding would use multiple independent stores.

![Kubeadm external etcd topology: control-plane hosts run API servers that connect to etcd members on separate hosts.](https://kubernetes.io/images/kubeadm/kubeadm-ha-topology-external-etcd.svg)

However supporting this topology is a large body of work: NVKE must manage
storage bootstrap, membership, endpoints, upgrades and recovery independently of
API-server hosts. We will therefore proceed in two phases, beginning with opt-in
resource-type etcd sharding within the stacked topology. Sharding, together with
appropriate hardware and API-server sizing, may be sufficient to support 10,000
worker nodes without changing topology. Benchmarks and observability data will
establish supported capacity and remaining bottlenecks before we commit to the
larger external-etcd effort at a later time. Stacked mode will remain supported.

The decision is to continue using the existing stacked topology, but allow for
install-time decisions to configure sharded etcd clusters. This decision affects
the management engine, NVKE controller and node agent.

### Phase 1: Sharded etcd in the stacked topology

We will support multiple etcd shards colocated with API servers on control-plane
hosts. Each shard is a distinct etcd cluster with its own membership, quorum and
datastore; each host runs an API server and a member of each shard. Operators
can scale the stacked control-plane host count through ControlPlaneClass.
Scaling to five hosts is one option that could help reach 5,000 or 10,000 worker
nodes, giving five API-server replicas and five voting members per shard in that
configuration. This increases API processing capacity while resource
partitioning separates storage workloads. Additional voting members provide
fault tolerance rather than partitioning writes within a shard. Five hosts is a
configuration to qualify, not a host-count limit.

All etcd shards will be treated equally for availability, that is, every
configured shard is a dependency of API readiness, with no distinction between
critical and noncritical resource stores. We will retain full `/readyz` checks
for kube-vip route advertisement. The defragmentation controller must account
for every shard and preserve each shard's quorum and the API readiness contract
during maintenance.

Shard count and resource assignments will be fixed at installation, as specified
in the Phase 1 requirements below. Control-plane replica count remains mutable;
scaling must adjust membership in every shard while preserving the installed
resource map.

Phase 1 will include publishing benchmark results in
[`benchmarks/results/`](../../benchmarks/results/README.md) to substantiate the
benefits of sharding and any supported capacity claims. Results must compare
unsharded and sharded stacked configurations under comparable workloads,
identify hardware, replica counts and resource assignments, and distinguish real
workers from simulated nodes. Published measurements must cover API latency,
throughput, inflight requests and rejections, etcd commit latency, and resource
utilization, including cases where the expected gains are not observed. The
5,000-worker target remains unqualified until supported by published results
with an explicit workload and test scope.

#### Phase 1 requirements

We will extend the operator-published
[`ControlPlaneClass`](../../management-engine/apis/v1/controlplaneclass_types.go),
which already defines control-plane replica count and machine settings, to
include the shard layout and validated resource-to-shard assignments.
[`NVKECluster`](../../management-engine/apis/v1/nvkecluster_types.go) selects
the class; the management engine will pass the resolved configuration to the
NVKE controller at cluster creation. Sharding fields are proposed additions, not
capabilities of the current class API.

The selected shard layout and resource map will be persisted per cluster and
remain fixed after installation. Later edits to the class, or selection of a
different class, must not change an existing cluster's shard count or resource
assignments. Control-plane replica count remains reconciled from the selected
class, along with its existing machine settings and rollout policy. Unassigned
resources use the default store. Every API replica must use the same resolved
resource map, verified before receiving traffic and preserved through scaling,
replacement and upgrades. Member endpoint changes must preserve shard identity.

##### Illustrative Go API proposal

The following proposed additions show mutable class policy and a write-once
snapshot in `NVKECluster.spec`. Existing fields and validation markers are
omitted for brevity. This is illustrative; the exact schema and complete
validation will be finalized during implementation.

```go
// ControlPlaneClassConfig defines control-plane sizing and machine settings.
type ControlPlaneClassConfig struct {
    // Existing fields remain unchanged, including Replicas.

    // Etcd configures the shard layout for newly created clusters.
    // Omission selects a single default store.
    // +optional
    Etcd *EtcdShardConfig `json:"etcd,omitempty"`
}

// EtcdShardConfig defines logical stores and resource routing.
// It excludes replica count, endpoints and runtime membership.
type EtcdShardConfig struct {
    // Shards declares stores, including a required "default" shard.
    // +listType=map
    // +listMapKey=name
    // +kubebuilder:validation:MinItems=1
    Shards []EtcdShard `json:"shards"`

    // ResourceAssignments routes whole resource types to stores.
    // Unassigned resources use the default shard.
    // +optional
    // +listType=map
    // +listMapKey=group
    // +listMapKey=resource
    ResourceAssignments []EtcdResourceAssignment `json:"resourceAssignments,omitempty"`
}

// EtcdShard identifies an independently replicated etcd store.
type EtcdShard struct {
    Name string `json:"name"`
}

// EtcdResourceAssignment routes an API resource type to a declared shard.
type EtcdResourceAssignment struct {
    // Group is empty for the core API group.
    Group string `json:"group"`

    // Resource is the plural resource name, without an API version.
    Resource string `json:"resource"`

    // Shard names a declared logical store.
    Shard string `json:"shard"`
}

// NVKEClusterSpec configures a cluster's control plane and node pools.
// Existing validation markers remain.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.resolvedEtcd) || (has(self.resolvedEtcd) && self.resolvedEtcd == oldSelf.resolvedEtcd)",message="resolvedEtcd cannot be changed or removed once initialized"
type NVKEClusterSpec struct {
    // Existing fields remain unchanged, including ControlPlaneClass.

    // ResolvedEtcd is the controller-owned snapshot of the selected class's
    // shard configuration, persisted before provisioning begins.
    // +optional
    ResolvedEtcd *EtcdShardConfig `json:"resolvedEtcd,omitempty"`
}
```

The CEL rule permits initial assignment of `resolvedEtcd`, then rejects changes
or removal (makes it immutable). The rest remains mutable. Before provisioning,
the management engine must normalize omitted etcd policy to a single `default`
shard and persist the snapshot. Creation retries and subsequent reconciliation
must use that persisted snapshot, including after a class edit or class
selection change. Replica count, machine settings and rollout policy continue to
resolve from the selected class.

##### Shard lifecycle and availability

The NVKE controller and node agent must handle every colocated shard during
bootstrap, scaling, host replacement, upgrades, certificate renewal and
teardown. Health checks, member removal and defragmentation must target the
intended shard and verify its membership and quorum. Each shard's membership
changes must have one owner, serialize conflicting mutations and reconcile
observed membership before retrying. Quorum loss requires explicit recovery,
never automatic creation of an empty store.

Defragmentation, scaling, host replacement and upgrades must share a
cluster-wide maintenance availability budget across all shards and API hosts.
Preserving each shard's quorum alone is insufficient: maintenance on different
shards on different hosts can make every API replica unready even while every
shard retains quorum. Before taking another host or shard member out of service,
coordination must account for current failures and in-progress operations and
preserve ready API capacity and an advertised route to the shared endpoint.
Qualification must verify this contract for the chosen API-server endpoint
configuration, including whether clients use local-only etcd endpoints or can
fail over to remote members.

We will preserve mTLS for client and peer traffic with tenant-scoped trust and
certificates covering each shard's configured endpoints. Bootstrap and repair
must remain possible without the workload API or workload-hosted DNS. Exact
resource assignments, disk placement and tuning require qualification.

### Phase 2: External etcd with independent API-server scaling

If Phase 1 benchmarks and testing demonstrate the need, we will add an opt-in
external-etcd topology that retains resource-type sharding and separates storage
membership and lifecycle from API-server hosts. Operators will be able to add
API-server replicas without adding etcd voting members, and size each shard
independently. The need for this topology will be determined by measured API and
storage load rather than a fixed node-count threshold.

In external mode, API-host deletion must not remove etcd members, and storage
repair must not depend on workload Node draining or etcd mirror-Pod discovery. A
follow-up ADR should be written to elaborate on this decision.

## Consequences

Phase 1 reduces contention between resource types while retaining stacked
placement. It adds etcd processes, memory use and storage activity on each
control-plane host, and requires shard-aware monitoring, certificate renewal,
member replacement and defragmentation.

All shards on a host share its failure domain and may compete for hardware
resources, especially disk I/O. We think that this is acceptable due to etcd
primarily being latency and not throughput-bound, though this needs to be
benchmarked and validated.

Every shard remains a dependency of API readiness. Losing any shard can make all
API replicas unready and withdraw every route to the shared API endpoint.
Storage partitioning therefore provides performance isolation without API
availability isolation. Remediation must distinguish shared shard failures from
unhealthy hosts to avoid repeatedly replacing healthy machines.

Operators must choose shard count and resource assignments at installation;
later edits to ControlPlaneClass cannot repartition existing clusters.
Replica-count changes remain supported, with coordinated membership changes
across every shard. API-server count remains coupled to each shard's voting
membership, and a single hot resource type remains within one shard. API
processing, scheduling, admission and networking can still limit scale.

Published benchmarks and testing will determine whether stacked sharding meets
the required scale or whether independent API-server scaling justifies the
larger external-etcd effort. Phase 2 remains conditional on that evidence; this
ADR does not establish a 5,000 or 10,000-worker capacity guarantee.

## Options considered

- **Sharded stacked etcd first, external etcd if needed (chosen):** addresses
  storage contention while retaining stacked placement, then uses published
  benchmarks and testing to determine whether independent API scaling justifies
  the larger external-etcd effort.
- **External etcd and sharding together from the outset:** provides independent
  API scaling immediately, but makes the initial scaling target depend on
  introducing a separate storage lifecycle.
- **Stacked etcd with larger hosts and tuning alone:** preserves the existing
  topology but leaves resource types competing within one store.
- **Sharded stacked etcd only:** separates resource workloads but leaves API
  scaling coupled to etcd voting membership on stacked hosts.
