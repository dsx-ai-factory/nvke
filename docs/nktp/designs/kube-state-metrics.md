# kube-state-metrics

## Purpose

NVKE telemetry pipelines (NKTP) uses stock kube-state-metrics (KSM) to expose Kubernetes object state.
In AI-scale Kubernetes clusters, KSM must balance scrape response size, its own per-pod and aggregate CPU and memory use, Kubernetes API server load,
and concurrent API watches. The designs below compare ways to address these scaling pressures and the tradeoffs of each.

## Design options

The ratings compare how each design scales in each dimension.

- **🟢 Good** scales well in this dimension.
- **🟡 Mixed** scales with significant tradeoffs.
- **🟠 Poor** scales inefficiently in this dimension.
- **🛑 Insufficient** leaves a known scaling bottleneck unresolved because it cannot divide the affected workload further.

### 1. Monolithic KSM

One KSM Pod collects all selected Kubernetes objects through API server list and watch requests, retains their state, and exposes their metrics.
The [`--resources` option][ksm-options] selects the resource types.

```text
# Pseudocommand for each KSM pod
ksm --resources=<all-resource-types>
```

- **Scrape response size.** 🛑 *Insufficient*. At scale, the single scrape response can exceed what an individual Collector Pod can process.
- **Per-Pod KSM requirements.** 🛑 *Insufficient*. All processing and state are held in one Pod.
- **Aggregate KSM requirements.** 🟢 *Good*. Each object is decoded once, with the overhead of only one KSM process.
- **API server load.** 🟢 *Good*. The API server serves each object to one KSM instance, without duplicated API server load.
- **API server watches.** 🟢 *Good*. One set of resource watches.

### 2. Hash sharding

KSM's [hash sharding][ksm-hash-sharding] assigns objects to shards using each object's unique identifier (UID).

```text
# Pseudocommand for each KSM pod
ksm --resources=<all-resource-types> --shard=0 --total-shards=2
ksm --resources=<all-resource-types> --shard=1 --total-shards=2
```

- **Scrape response size.** 🟢 *Good*. `N` shards reduce scrape response size per KSM Pod to roughly `1/N`. Enough shards must be configured to keep each response
  manageable. An individual object's metrics remain on one shard. Total scrape response bytes remain roughly the same, divided among shards.
- **Per-Pod KSM requirements.** 🟡 *Mixed*. `N` shards reduce retained state per KSM Pod to roughly `1/N`. Each shard still decodes all received objects before
  choosing which to retain.
- **Aggregate KSM requirements.** 🟠 *Poor*. Total KSM decoding work grows roughly `N` times. Total memory used to retain object state remains roughly the same,
  divided among shards. CPU and memory requests add across all shards.
- **API server load.** 🟠 *Poor*. Serving the same objects to all `N` shards increases API server load roughly `N` times.
- **API server watches.** 🟡 *Mixed*. Every shard opens watches for all selected resource types, multiplying API server watches by `N`.

### 3. Partitioned collection

Separate KSM instances request different resource types from the API server, selected through [KSM's `--resources` option][ksm-options].

```text
# Pseudocommand for each KSM pod
ksm --resources=pods
ksm --resources=deployments,statefulsets,daemonsets
ksm --resources=<remaining-resource-types>
```

- **Scrape response size.** 🛑 *Insufficient*. Filtering at the source reduces the data each instance exposes in scrape responses. Total scrape response bytes are roughly
  unchanged, but divided among instances. One partition can still be too large, for example Pods.
- **Per-Pod KSM requirements.** 🛑 *Insufficient*. Filtering at the source reduces the data each instance processes and retains. Resource partitions may be unequal, so each
  instance may need its own CPU and memory budget.
- **Aggregate KSM requirements.** 🟢 *Good*. Across all instances, total CPU needed to process objects and memory needed to retain their state remain roughly
  the same as monolithic KSM.
- **API server load.** 🟢 *Good*. Each resource type is watched by one KSM instance, avoiding the duplicated API server load of hash sharding.
  For the same object coverage, API server load for those objects remains roughly `1x` the monolithic baseline instead of hash sharding's `Nx`.
- **API server watches.** 🟢 *Good*. One set of resource watches, divided among KSM instances.

### 4. DaemonSet

Run one KSM Pod per node, each collecting all selected resource types through the [`--resources` option][ksm-options].

```text
# Pseudocommand for each KSM pod
ksm --resources=<all-resource-types> # On node-a
ksm --resources=<all-resource-types> # On node-b
# One Pod on each remaining node.
```

- **Scrape response size.** 🛑 *Insufficient*. Each response still covers the whole cluster.
- **Per-Pod KSM requirements.** 🛑 *Insufficient*. Each instance retains the whole cluster's selected object state.
- **Aggregate KSM requirements.** 🟠 *Poor*. CPU and memory costs for the same collection repeat on every node.
- **API server load.** 🟠 *Poor*. Every instance collects the same objects, multiplying API server load by the node count.
- **API server watches.** 🟠 *Poor*. Every node opens the full set of resource watches.

### 5. Pod DaemonSet + hash sharding

Use [KSM's node filtering][ksm-daemonset-sharding] to collect scheduled Pods on their nodes.
Collect unscheduled Pods and all other resource types centrally, using [hash sharding][ksm-hash-sharding] across that entire central collection.
The [`--track-unscheduled-pods` option][ksm-options] restricts central Pod collection to Pods without a node assignment.

```text
# Pseudocommand for each KSM pod
ksm --resources=<all-resource-types> --track-unscheduled-pods --shard=0 --total-shards=2
ksm --resources=<all-resource-types> --track-unscheduled-pods --shard=1 --total-shards=2

ksm --resources=pods --node=node-a
ksm --resources=pods --node=node-b
# One Pod on each remaining node.
```

- **Scrape response size.** 🟢 *Good*. DaemonSet responses follow the local Pod population rather than the whole cluster.
  Hash sharding divides central responses among enough shards to keep each response manageable.
- **Per-Pod KSM requirements.** 🟡 *Mixed*. DaemonSet requirements follow node-local Pod count. Hash sharding reduces retained state per central Pod,
  but every central shard still decodes all unscheduled Pods and other selected resource types.
- **Aggregate KSM requirements.** 🟠 *Poor*. Scheduled Pod collection is not duplicated, but total decoding work can still reach a substantial fraction of
  `N` times the monolithic baseline because every central shard decodes all remaining resource types. The DaemonSet reserves CPU and memory requests on every node.
- **API server load.** 🟠 *Poor*. Scheduled Pod collection avoids duplicated API server load, but total API server load can still reach a substantial fraction of
  `N` times the monolithic baseline because all `N` central shards collect every remaining resource type, including those that do not need sharding.
- **API server watches.** 🟡 *Mixed*. A Pod watch per node is combined with a full set of central resource watches per shard.

### 6. Pod DaemonSet + resource partitioning

Use [KSM's node filtering][ksm-daemonset-sharding] so each DaemonSet instance collects only scheduled Pods on its own node.
Use independent central workloads for unscheduled Pods and other resource types.
Low-cardinality resource types can be configured to share a KSM instance, while high-cardinality types run separately.
KSM's [`--track-unscheduled-pods` option][ksm-options] covers Pods without a node assignment.

```text
# Pseudocommand for each KSM pod
ksm --resources=pods --track-unscheduled-pods
ksm --resources=deployments,statefulsets,daemonsets
ksm --resources=<remaining-resource-types>

ksm --resources=pods --node=node-a
ksm --resources=pods --node=node-b
# One Pod on each remaining node.
```

- **Scrape response size.** 🛑 *Insufficient*. DaemonSet responses follow the local Pod population rather than the whole cluster.
  Even with a dedicated central KSM instance, a high-cardinality resource type may exceed one Pod's scrape response budget.
- **Per-Pod KSM requirements.** 🛑 *Insufficient*. The DaemonSet distributes collection of Pods, often one of the highest-cardinality resource types, across nodes so
  per-instance KSM requirements grow with node-local Pod count rather than cluster-wide Pod count.
  Size each DaemonSet Pod's CPU and memory budget for the number of Pods it monitors on its node. Central workloads have independent CPU and memory budgets.
  Even with a dedicated KSM instance, a high-cardinality resource type may exceed one Pod's memory budget.
- **Aggregate KSM requirements.** 🟡 *Mixed*. Each object is received and decoded by one KSM instance. The DaemonSet's requests are reserved on every node, in
  addition to the central workloads' requests.
- **API server load.** 🟢 *Good*. The API server serves each object to one KSM instance, without duplicated API server load.
- **API server watches.** 🟡 *Mixed*. The per-node watch overhead remains, plus central resource watches.

### 7. Pod DaemonSet + resource partitioning + selective sharding

Partition central workloads by resource type first. Use hash sharding as a last resort when a single resource type still exceeds a Pod's memory or scrape response budget.
For example, Deployments, StatefulSets, and DaemonSets may be small enough to collect together.
Jobs may be collected separately but still exceed a single Pod's budget, so only Jobs are sharded in this example.

```text
# Pseudocommand for each KSM pod
ksm --resources=pods --track-unscheduled-pods
ksm --resources=deployments,statefulsets,daemonsets
ksm --resources=jobs --shard=0 --total-shards=2
ksm --resources=jobs --shard=1 --total-shards=2
ksm --resources=<remaining-resource-types>

ksm --resources=pods --node=node-a
ksm --resources=pods --node=node-b
# One Pod on each remaining node.
```

- **Scrape response size.** 🟢 *Good*. DaemonSet responses follow the local Pod population rather than the whole cluster.
  Selective hash sharding divides the remaining large resource types to keep central scrape responses manageable.
- **Per-Pod KSM requirements.** 🟢 *Good*. The DaemonSet distributes collection of Pods, often one of the highest-cardinality resource types, across nodes so
  per-instance KSM requirements grow with node-local Pod count rather than cluster-wide Pod count.
  Size each DaemonSet Pod's CPU and memory budget for the number of Pods it monitors on its node. Central workloads have independent CPU and memory budgets.
  Selective hash sharding reduces retained state per Pod for those high-cardinality resource types, but each shard still decodes all objects of its resource
  type.
- **Aggregate KSM requirements.** 🟡 *Mixed*. Small resource types share instances. Resource partitioning avoids repeated KSM decoding work. Sharding adds requests
  and decoding work only for the resource types that need it, rather than multiplying them across all collection. The DaemonSet still reserves requests on every
  node.
- **API server load.** 🟢 *Good*. Resource partitioning avoids duplicated API server load. Selective hash sharding adds API server load only for the resource types
  that need it, rather than multiplying it across all collection.
- **API server watches.** 🟡 *Mixed*. The DaemonSet's per-node watch overhead still needs validation at large scale.
  Additional watches from selective hash sharding are expected to be negligible compared with the per-node watches.

## Design comparison

| Design | Scrape response size | Per-Pod KSM requirements | Aggregate KSM requirements | API server load | API server watches |
| --- | --- | --- | --- | --- | --- |
| Monolithic KSM | **🛑 Insufficient** | **🛑 Insufficient** | **🟢 Good** | **🟢 Good** | **🟢 Good** |
| Hash sharding | **🟢 Good** | **🟡 Mixed** | **🟠 Poor** | **🟠 Poor** | **🟡 Mixed** |
| Partitioned collection | **🛑 Insufficient** | **🛑 Insufficient** | **🟢 Good** | **🟢 Good** | **🟢 Good** |
| DaemonSet | **🛑 Insufficient** | **🛑 Insufficient** | **🟠 Poor** | **🟠 Poor** | **🟠 Poor** |
| Pod DaemonSet + hash sharding | **🟢 Good** | **🟡 Mixed** | **🟠 Poor** | **🟠 Poor** | **🟡 Mixed** |
| Pod DaemonSet + resource partitioning | **🛑 Insufficient** | **🛑 Insufficient** | **🟡 Mixed** | **🟢 Good** | **🟡 Mixed** |
| Pod DaemonSet + resource partitioning + selective sharding | **🟢 Good** | **🟢 Good** | **🟡 Mixed** | **🟢 Good** | **🟡 Mixed** |

## Recommendation

For AI-scale clusters, use design 7, combining the Pod DaemonSet, resource partitioning, and selective hash sharding.

## Configuration shape

```yaml
kube-state-metrics:
  namespaces: [] # All namespaces.
  daemonset:
    enabled: true
    kubernetes: ... # Similar to OTel workloads.
  deployments:
    unscheduled-pods:
      enabled: true
      resource_types: [pods]
      track_unscheduled_pods: true # Requires resource_types to be exactly [pods].
      shards: 1
      kubernetes: ...
    workloads:
      enabled: true
      resource_types: [deployments, statefulsets, daemonsets, replicasets, replicationcontrollers, jobs, cronjobs, horizontalpodautoscalers, poddisruptionbudgets]
      track_unscheduled_pods: false
      shards: 1
      kubernetes: ...
    networking:
      enabled: true
      resource_types: [services, endpointslices, ingresses, networkpolicies]
      track_unscheduled_pods: false
      shards: 1
      kubernetes: ...
    storage:
      enabled: true
      resource_types: [persistentvolumes, persistentvolumeclaims, volumeattachments]
      track_unscheduled_pods: false
      shards: 1
      kubernetes: ...
    configuration:
      enabled: true
      resource_types: [configmaps]
      track_unscheduled_pods: false
      shards: 1
      kubernetes: ...
    cluster:
      enabled: true
      resource_types: [nodes, namespaces, storageclasses, resourcequotas, limitranges, leases, certificatesigningrequests, mutatingwebhookconfigurations, validatingadmissionpolicies, validatingadmissionpolicybindings, validatingwebhookconfigurations]
      track_unscheduled_pods: false
      shards: 1
      kubernetes: ...
```

Deployment names are user-defined and form names such as `nktp-ksm-workloads`. `kubernetes` passes through workload settings.

The shared namespace list applies unless a workload supplies its own `namespaces` list. An empty list selects all namespaces. Empty strings are ignored.
Helm generates collection arguments and RBAC from each central workload's `resource_types` and effective namespace selection.

The DaemonSet always collects scheduled Pods on its node. `track_unscheduled_pods: true` selects Pods without a node assignment, regardless of Pod phase,
is supported only for central workloads, and requires `resource_types` to be exactly `[pods]`. It defaults to `false`.
Helm supplies the DaemonSet's `--node` argument and `NODE_NAME` through the downward API.

`shards` sets the number of hash shards for a central workload. Helm supplies `--shard` and `--total-shards`.

## Proposed default groups

| Workload | Resources |
| --- | --- |
| `daemonset` | Scheduled Pods, collected per node |
| `deployments.unscheduled-pods` | Pods without a node assignment |
| `deployments.workloads` | Controllers, Jobs, autoscalers, and disruption budgets |
| `deployments.networking` | Services, EndpointSlices, Ingresses, and NetworkPolicies |
| `deployments.storage` | PersistentVolumes, PersistentVolumeClaims, and VolumeAttachments |
| `deployments.configuration` | ConfigMaps |
| `deployments.cluster` | Nodes, Namespaces, StorageClasses, quotas, limits, Leases, certificates, and supported admission resources |

Defaults assign each selected resource type to one collection group, with one shard per central workload unless its size requires more.

Secret metrics are opt-in because collection requires list/watch access to complete Secret objects, including their contents.
Add `secrets` to a workload's `resource_types` to enable them. Its `namespaces` list limits access. An empty list grants access across the cluster.

`mutatingadmissionpolicies` and `mutatingadmissionpolicybindings` are disabled by default because KSM uses their
`admissionregistration.k8s.io/v1` APIs, which older clusters do not serve.
Add them to `resource_types` only when the cluster serves both APIs.

[ksm-options]: https://github.com/kubernetes/kube-state-metrics/blob/main/docs/developer/cli-arguments.md
[ksm-hash-sharding]: https://github.com/kubernetes/kube-state-metrics#horizontal-sharding
[ksm-daemonset-sharding]: https://github.com/kubernetes/kube-state-metrics#daemonset-sharding-for-pod-metrics
