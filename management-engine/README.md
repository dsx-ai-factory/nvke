# Management Engine internal API

This module defines the management engine. It contains Go types, generated CRDs and
schema tests. Reconciliation and public API servers are subsequent work.

```text
management-engine/
├── apis/
│   ├── v1alpha1/            # Internal types, generated deepcopy code and tests
│   ├── crds/                # Generated, installable CRD manifests
│   ├── boilerplate.go.txt   # Header used by the deepcopy generator
│   └── boilerplate.yaml.txt # Header used by the CRD generator
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

## API conventions

| Concern | Initial decision |
|---|---|
| Internal API group | `internal.nke.nvidia.com`. The group identifies internal storage. |
| Internal version | `v1alpha1`, initially both served and storage version. |
| Kinds and resources | `NKECluster` / `nkeclusters`, `NodePool` / `nodepools`; list kinds are `NKEClusterList` and `NodePoolList`. |
| Scope | Both resources are namespaced. All tenant-owned downstream CAPI objects belong to that same org namespace. |
| Names | Client-selected Kubernetes DNS subdomain names, at most 253 characters; immutable native `metadata.name`. Identity is kind + org + name. Server-assigned UID distinguishes recreations. Provider names may require separately derived shorter names. |
| Org mapping | Org onboarding assigns one stable Kubernetes namespace per org per management cluster and records the org to management-cluster/namespace mapping. API routing resolves this authoritative mapping from the authenticated org. Never derive authority from a caller-supplied namespace, display name or sanitized org slug.|
| Metadata | Full Kubernetes `TypeMeta` and `ObjectMeta` internally. Public projections expose only permitted metadata. Internal requests require Kubernetes `apiVersion`/`kind`; public requests may omit them. |
| Concurrency | Kubernetes `resourceVersion` is an opaque token. Public update handlers must carry it through and translate stale writes into conflicts. |
| Spec ownership | Tenant intent occupies the documented spec fields.  Operators may use subresources for imperative operations. Controllers own status. Future public projection must preserve fields the caller cannot write. |
| Conditions | `metav1.Condition`, merged by `type`, with `observedGeneration`, reason, message and transition time. `Ready=True` is positive; `Reconciling=True` means progress; `Stalled=True` requires intervention. Absent conditions mean unknown. A stale Ready condition does not establish readiness for the current generation. |
| References | Names identify intent. Resolve tenant resource references namespace within the org namespace and pin lifecycle ownership by UID. Configuration names identify operator-published offerings; their resource schemas and catalog scopes are part of the management engine. |
| NodePool binding | `spec.clusterName` is immutable. The reconciler will record `{name, uid}` in `status.clusterRef` once resolved and set a matching controller owner reference to NKECluster. Admission prevents changes/removal of an existing binding. Reconcile must check that the current cluster UID still matches before creating or adopting anything. |
| Deletion | Native deletion timestamps and controller-owned finalizers. Finalizer implementation and downstream deletion ordering belong to the lifecycle tickets. |

Namespace scoping is a storage boundary, not complete authorization. The future
API tier must authenticate the org, authorize each verb/field, and prevent
cross-org routing. The schema cannot verify reference existence, a live cluster
UID, or operator privileges. Status writes must be restricted through RBAC when
the controller is deployed.

## Public and internal versions

The SADD public groups are `nke.nvidia.com` and `operator.nke.nvidia.com`, with
`v1` in its examples. They are separate contracts from
`internal.nke.nvidia.com/v1alpha1`, though prior to moving to the public apis will move to v1.

Public protobuf contracts will generate RPC and/or REST/OpenAPI surfaces. Internal
Go types generate Kubernetes CRDs. Explicit projection/enrichment will map
between the two. Never serve the internal structs directly as the public API.

## Initial resource shapes

NKECluster follows the design's access, authentication, network, release channel,
control-plane class and accelerated-runtime selection. Authentication currently
supports the claim-based JWT.

```yaml
apiVersion: internal.nke.nvidia.com/v1alpha1
kind: NKECluster
metadata:
  name: prod-us-west
  namespace: org-example # assigned by org onboarding, not public request input
spec:
  apiServerAccess:
    publicAccessEnabled: false
  clusterNetwork:
    pods:
      cidrBlocks: [10.244.0.0/16]
    services:
      cidrBlocks: [10.96.0.0/12]
    serviceDomain: cluster.local
  release:
    channel:
      name: stable
      version: "1.34"
  controlPlaneClass: standard
  acceleratedRuntime:
    name: none
```

NodePool follows the design's cluster reference, replicas, instance type, release,
template and rollout shape. `replicas` is required: explicit zero requests zero
workers, while missing/null is rejected. Omitted rollout defaults to a rolling
update with one surge node.

`maxSurge` accepts integers or percentage strings.

```yaml
apiVersion: internal.nke.nvidia.com/v1alpha1
kind: NodePool
metadata:
  name: gpu-a
  namespace: org-example
spec:
  clusterName: prod-us-west
  replicas: 8
  instanceType: gb200-nvl72
  release:
    name: gb200-ubuntu-24.04
    version: "2026.09.1"
  template:
    metadata:
      labels:
        workload: pretraining
    spec:
      taints:
        - key: nvidia.com/gpu
          value: present
          effect: NoSchedule
  rollout:
    strategy:
      type: RollingUpdate
      rollingUpdate:
        maxSurge: 1
```

Status contains conditions, observed generation and resolved release. NodePool
also reports its cluster UID binding and observed/ready/updated replica counts;
unknown counts are absent, observed zero counts remain explicit.
There is no separate management-plane Node resource at this time.

## Development

Follow the repository [development conventions](../docs/development.md) and
[workspace guide](../docs/go-workspace.md). From the repository root:

```sh
make -C management-engine generate       # pinned controller-gen; types → CRDs
make -C management-engine generate-check # detect stale generated output
make -C management-engine build test lint
make preflight MODULES=management-engine
```

Install the generated definitions into an explicitly chosen development cluster:

```sh
kubectl --context <development-context> apply -f management-engine/apis/crds/
```
