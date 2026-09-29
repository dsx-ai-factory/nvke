# Management Engine

The management engine connects directly to a configurable Kubernetes API serving
NKE and Cluster API (CAPI) resources. It provides a standalone controller-manager,
internal resource types and generated CRDs. The controller registry is currently
empty; lifecycle reconciliation and the public API service are not implemented.

| Package | Purpose |
| --- | --- |
| `apis/v1` | Importable resource types and scheme registration |
| `apis/crds` | Generated CRD manifests |
| `cmd/manager` | Executable configuration and process lifecycle |
| `pkg/manager` | Importable connection configuration and runtime construction |
| `pkg/controllers/registry` | Production controller registration |

## Run the controller manager

Use an existing development management cluster. The empty runtime does not require
NKE or CAPI CRDs; install them when adding controllers that watch those resources.
Run from the repository root:

```sh
make -C management-engine run MANAGER_ARGS="--kubeconfig /path/to/dev-kubeconfig --context development-management --leader-election-namespace nke-system"
```

Replace the example paths, context and namespace with your environment's values.
The election namespace must be supplied even when running in a cluster, and must
already exist. It does not restrict which org
namespaces controllers can watch. `make run` builds the executable at
`management-engine/bin/management-engine` before starting it.

The manager is a client of the **management Kubernetes API**. Its own HTTP
listeners serve metrics and probes. Tenant workload clusters have separate
Kubernetes APIs; the existing CWE REST service is also a distinct interface.

### Connection configuration

Credentials and the target endpoint are selected in this order:

1. `--kubeconfig`, with optional `--context`.
2. `KUBECONFIG`, with optional `--context`.
3. An explicit `--context` selects the default kubeconfig, even inside a cluster.
4. With no explicit selectors, client-go loads in-cluster ServiceAccount
   credentials, or the default kubeconfig when running outside a cluster.

Invalid selections fail without falling back to another cluster. Every nonempty
path in a selected `KUBECONFIG` list must exist. The management API must use HTTPS
with certificate verification enabled. Redirects must remain HTTPS on the selected
API hostname and port.

Kubeconfigs and their exec credential helpers must come from trusted sources.
Kubeconfig files must remain unchanged during loading; the loader does not take an
atomic snapshot across files. Embedded consumers supplying `rest.Config.Transport`
or `WrapTransport` are responsible for preserving certificate verification and the
selected API origin. These hooks execute trusted Go code inside the process; the
manager cannot enforce those guarantees inside a custom transport.

Kubeconfig TLS settings and client-go exec credentials are preserved; required
exec helpers must be installed. Embedded consumers can pass a `*rest.Config` to `manager.New`.
Discovery, resource access and leader election all use that target.
The loader disables client-side throttling, matching controller-runtime's default
and relying on server-side API Priority and Fairness. Embedded consumers can set
`rest.Config.QPS` and `Burst` before calling `manager.New`.

The runtime identity requires API discovery access, `get/create/update` on
`coordination.k8s.io` Leases and `create/patch` on core Events in the election
namespace. Add resource permissions with each controller's watches and writes.
The empty registry requires no NKE/CAPI object permissions. Constructing the
manager does not verify API availability or resource permissions.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--kubeconfig` | Selection above | Management API credentials and endpoint |
| `--context` | Kubeconfig current context | Management cluster selection |
| `--leader-elect` | `true` | Coordinate replicas |
| `--leader-election-namespace` | Required with election | Namespace containing the Lease |
| `--metrics-bind-address` | `127.0.0.1:9090` | HTTP `/metrics`; `0` disables |
| `--health-probe-bind-address` | `:8081` | HTTP `/healthz` and `/readyz`; `0` disables |
| `--log-level` | `INFO` | JSON log level |
| `--version` | — | Print binary version and exit |

### Health and leadership

Readiness requires startup and synchronization of registered caches, including on
standby replicas; it does not certify API connectivity or authorization. API
requirements belong to each controller's watches and resource access, rather than
a central startup precheck. Exec credential helpers must terminate promptly.
Prometheus metrics use unauthenticated HTTP on loopback port 9090 by default,
matching CWE. Set an explicit address only when access is restricted to trusted
scrapers. Probes use unauthenticated HTTP on all interfaces so the kubelet can
reach them.

Run one deployment group per management cluster. All replicas must share the
Lease `management-engine.nke.nvidia.com` in the same namespace. Election stays
enabled for single-replica deployments to cover rolling-upgrade overlap. Timing
uses upstream defaults: 15-second lease, 10-second renew deadline and 2-second
retry period.

Shutdown cancels work with a 30-second grace period and leaves the Lease to expire.
Embedding processes must exit promptly when `Start` returns, including on
leadership loss. For debugging with `--leader-elect=false`, stop deployed replicas
first and stop the host process before restoring them.

## Development

Follow the repository [development conventions](../docs/development.md) and
[workspace guide](../docs/go-workspace.md). From the repository root:

```sh
make -C management-engine build test lint
make -C management-engine generate       # after changing API types
make -C management-engine generate-check # verify generated CRDs and deepcopy code
make -C management-engine test-integration
make preflight MODULES=management-engine
```

Integration tests start a disposable API server and verify NKE/CAPI event delivery,
probes, metrics, leader handoff and shutdown. The target downloads missing test
assets: envtest for Kubernetes 1.37.0 and CRDs from the module-pinned CAPI version.
For offline runs, supply existing assets:

```sh
KUBEBUILDER_ASSETS=/path/to/envtest-binaries \
CAPI_CRD_DIR=/path/to/cluster-api/config/crd/bases \
  make -C management-engine test-integration
```

### Add a controller

The command calls `manager.New`, `registry.Register`, then `Start`. Runtime
construction is independent of registration, so other binaries can choose which
controllers to install.

1. Add a controller under `pkg/controllers/<resource>`, accepting production
   dependencies through its constructor.
2. Register its `SetupWithManager` call in `pkg/controllers/registry`, returning
   errors with the controller's name. Reuse the manager's scheme, clients, cache,
   logging and event recording.
3. Declare watches and indexes during setup, add required schemes, and provide
   matching RBAC. Cached reads of undeclared types fail; use the API reader for
   intentional uncached reads. The cache strips `managedFields` to reduce memory
   use; controllers that need field ownership metadata must use the API reader.
4. Keep NKECluster and NodePool resources in their org namespace. Preserve existing
   CWE-managed CAPI resources in place; define downstream placement and ownership
   when implementing lifecycle controllers.
   Keep flag parsing, signals and process exits in the command, and test observers
   and fakes in test code.

Use resource APIs directly. The migration replaces NKE's existing REST API embedded
in CWE; new controllers must not depend on CWE REST endpoints. Replacing existing
workflow or database behavior requires an explicit ownership handoff. A separate
election Lease does not prevent two different controllers from writing the same
objects.

## API conventions

| Concern | Contract |
| --- | --- |
| Group/version | `internal.nke.nvidia.com/v1`, currently served and stored. “Internal” identifies the storage contract; Go types remain importable. |
| Resources | Namespaced `NKECluster` / `nkeclusters` and `NodePool` / `nodepools`, with corresponding list kinds. |
| Org mapping | One stable namespace per org per management cluster. Public routing must use the authenticated org's authoritative mapping, never a caller-supplied namespace or derived slug. |
| Names and identity | Client-selected Kubernetes DNS subdomain names, at most 253 characters. Names are immutable; UIDs distinguish recreations. Provider names may need shorter derived forms. |
| Metadata and concurrency | Standard Kubernetes metadata and opaque `resourceVersion`. Public projections must preserve concurrency checks and expose only permitted fields. |
| Field ownership | Tenant intent belongs in spec; controllers own status. Public updates must preserve fields the caller cannot write. |
| Conditions | `metav1.Condition`, merged by type with `observedGeneration`. `Ready` indicates readiness, `Reconciling` progress and `Stalled` required intervention. Missing or stale conditions do not establish readiness. |
| References | Resolve tenant references within the org namespace and pin lifecycle ownership by UID. Configuration references identify operator-published offerings. |
| NodePool binding | `spec.clusterName` is immutable. Reconciliation must record `{name, uid}` in `status.clusterRef` and set the matching NKECluster owner reference. Admission prevents changing/removing an existing binding; reconciliation must verify the UID before creating or adopting resources. |
| Deletion | Kubernetes deletion timestamps and controller-owned finalizers govern lifecycle cleanup. |

Namespace scoping provides a storage boundary. Public APIs must enforce identity,
org and field authorization; controller status writes require RBAC. Schema
validation cannot establish reference existence, current cluster UID or privileges.

Internal Go types generate CRDs. Public user and operator contracts
(`nke.nvidia.com` and `operator.nke.nvidia.com`) have independent versions and
require explicit projection/enrichment; do not expose internal structs directly.

## Install NKE resource types

Before creating NKE resources, apply the generated CRDs to the management API:

```sh
kubectl --kubeconfig /path/to/dev-kubeconfig --context development-management \
  apply -f management-engine/apis/crds/
```

The manifests serve and store only `v1`. Existing `v1alpha1` installations need a
storage-version migration before removing the old version; these manifests alone
do not migrate stored objects. Go consumers must update imports to `apis/v1`.

## Resource examples

[NKECluster](apis/v1/nkecluster_types.go) defines cluster access, authentication,
networking, release selection, control-plane class and accelerated runtime.

```yaml
apiVersion: internal.nke.nvidia.com/v1
kind: NKECluster
metadata:
  name: prod-us-west
  namespace: org-example
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
```

[NodePool](apis/v1/nodepool_types.go) defines worker capacity and configuration.
`replicas` is required; zero requests zero workers. Omitted rollout defaults to a
rolling update with one surge node; `maxSurge` accepts an integer or percentage.

```yaml
apiVersion: internal.nke.nvidia.com/v1
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
```

Status carries conditions, observed generation and resolved release. NodePool
also carries its cluster UID binding and observed/ready/updated replica counts;
unknown counts are absent and observed zero counts remain explicit.
