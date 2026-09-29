# Management Engine

The management engine connects directly to a configurable Kubernetes API serving
NKE and Cluster API (CAPI) resources. It provides a standalone controller-manager,
internal resource types and generated CRDs. Lifecycle reconciliation and the
public API service are not implemented.

Unless noted otherwise, run commands from `management-engine/`.

| Package | Purpose |
| --- | --- |
| `apis/v1` | Importable resource types and scheme registration |
| `config/crd/bases` | Generated CRD manifests |
| `cmd` | Startup, connection configuration, manager setup, and runtime tests |
| `internal/controller` | Resource reconcilers |

## Deployment topology

The Workload NKE Services Cluster runs the manager (`chart` / `config/default`).
The Workload NKE Datastore Cluster holds the CRDs, resources, and election Lease
(`config/state`); it needs no separate management-engine manager. Runtime uninstall
preserves datastore resources.

The current chart mounts a Secret containing `kubeconfig` and any referenced
credential files at `/etc/management-api`. Set the Secret name and datastore
election namespace in [runtime values](config/samples/runtime-values.yaml).
Supply Kubernetes credentials; automatic credential provisioning and renewal are
not implemented.

Install state with `make install-state STATE_CONTEXT=...`.
`make deploy HOST_CONTEXT=... IMG=...` applies the Kustomize runtime bundle,
including its `management-engine-system` namespace; the manager cannot start
until its credential Secret is provisioned. For Helm, provision the runtime
namespace and Secret first, then run
`make helm-deploy HOST_CONTEXT=... IMG=... HELM_EXTRA_ARGS='-f /path/to/values.yaml'`.

`make undeploy` and `make helm-uninstall` preserve the runtime namespace and
external Secrets. To remove them too, explicitly delete the runtime namespace;
this deletes everything in that namespace. Datastore resources are separate.

### Metrics

Deployments enable HTTPS metrics on port 8443; the standalone binary disables
metrics by default. Prometheus integration is disabled by default and no collector
is installed. See [Kubebuilder's metrics guide](https://book.kubebuilder.io/reference/metrics.html)
for certificates, access control and scraping configuration.

In this split deployment, metrics authentication and authorization currently use
the datastore kubeconfig. Services-cluster scraper tokens are not automatically
accepted; services-cluster metrics authorization is not wired yet.

## Run the controller manager

Use a datastore with the NKECluster and NodePool CRDs installed and credentials
authorized to watch them. The scaffolded reconcilers currently perform no lifecycle
operations:

```sh
make run MANAGER_ARGS="--kubeconfig /path/to/dev-kubeconfig --context development-management --leader-election-namespace nke-system"
```

Replace the example paths, context and namespace with your environment's values.
The election namespace must be supplied even when running in a cluster, and must
already exist. It does not restrict which org
namespaces controllers can watch. `make run` builds `bin/management-engine` before
starting it.

The manager is a client of the **management Kubernetes API**. Its own HTTP
listeners serve metrics and probes. Tenant workload clusters have separate
Kubernetes APIs; the existing CWE REST service is also a distinct interface.

### Connection configuration

Credentials and the target endpoint use controller-runtime's standard loading order:

1. `--kubeconfig`.
2. `KUBECONFIG`.
3. In-cluster ServiceAccount credentials.
4. The default kubeconfig (`$HOME/.kube/config`).

`--context` selects a context when loading a kubeconfig; it does not force
out-of-cluster loading. Missing entries in `KUBECONFIG` lists may be skipped, and
empty configurations may fall back to in-cluster credentials. The deployment
explicitly selects the datastore kubeconfig and disables host ServiceAccount-token
mounting. The management API must use HTTPS with certificate verification enabled.

Use trusted kubeconfigs and exec credential helpers. Required helpers must be
installed and terminate promptly; kubeconfig files must remain stable during loading.
Discovery, resource access and leader election all use the selected datastore.
The loader disables client-side throttling, matching controller-runtime's default
and relying on server-side API Priority and Fairness.

The datastore bundle grants `get/list/watch` on NKEClusters and NodePools across
namespaces, `get/create/update` on Leases and `create/patch` on Events in the election
namespace, and TokenReview/SubjectAccessReview creation for secure metrics.
API discovery uses Kubernetes' default permissions for authenticated identities.
Add write permissions when implementing reconciliation.
Constructing the manager does not verify API availability or resource permissions.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--kubeconfig` | Selection above | Management API credentials and endpoint |
| `--context` | Kubeconfig current context | Management cluster selection |
| `--leader-elect` | `true` | Coordinate replicas |
| `--leader-election-namespace` | Required with election | Namespace containing the Lease |
| `--metrics-bind-address` | `0` | Metrics address; deployments use `:8443` |
| `--metrics-secure` | `true` | HTTPS metrics with authentication and authorization |
| `--health-probe-bind-address` | `:8081` | HTTP `/healthz` and `/readyz`; `0` disables |
| `--zap-log-level` | `info` | Log verbosity; JSON output by default |
| `--version` | — | Print binary version and exit |

Run `bin/management-engine --help` for all flags, including metrics TLS options.

### Health and leadership

Readiness requires startup and synchronization of registered caches, including on
standby replicas; it does not certify API connectivity or authorization. API
requirements belong to each controller's watches and resource access, rather than
a central startup precheck. Probes use unauthenticated HTTP so the kubelet can
reach them.

Run one deployment group per management cluster. All replicas must share the
Lease `management-engine.nke.nvidia.com` in the same namespace. Election stays
enabled for single-replica deployments to cover rolling-upgrade overlap. Timing
uses upstream defaults: 15-second lease, 10-second renew deadline and 2-second
retry period.

Shutdown cancels work with a 30-second grace period and leaves the Lease to expire.
The process exits when `Start` returns, including on leadership loss.
For debugging with `--leader-elect=false`, stop deployed replicas
first and stop the host process before restoring them.

## Development

Follow the repository [development conventions](../docs/development.md) and
[workspace guide](../docs/go-workspace.md).

```sh
make build test lint
make generate manifests # after changing API types
make generate-check # verify generated CRDs and deepcopy code
make test-integration
make chart-check
```

Before submitting a PR, run `make preflight MODULES=management-engine` from the
repository root.

Controller and runtime tests use disposable envtest API servers and download
missing binaries unless `KUBEBUILDER_ASSETS` is set. Runtime integration tests cover
NKECluster event delivery, probes, leader handoff and shutdown.
For offline runs, supply existing assets:

```sh
KUBEBUILDER_ASSETS=/path/to/envtest-binaries \
  make test-integration
```

### Add an API or controller

For a new API, add its Go types and validation markers under `apis/v1`, register
its object and list in `AddToScheme`, and add its import path to [PROJECT](PROJECT)
using the existing resource entries as examples. Run `make generate manifests`
and include the new CRD in `config/crd/kustomization.yaml`.

For an API registered in `PROJECT` without a controller, replace `Example` with
its kind and run:

```sh
make kubebuilder
bin/kubebuilder create api --group internal --version v1 --kind Example \
  --resource=false --controller=true
```

`--resource=false` preserves the existing types. Kubebuilder's `--resource=true`
scaffolds types under `api/`, which requires adaptation to this project's `apis/`
layout. The CLI registers controllers in `cmd/main.go`; implement their
`Reconcile` methods and tests under `internal/controller`.

Add RBAC markers for required access and run `make manifests`. The generated
`config/rbac/role.yaml` is bound to the datastore ServiceAccount by `config/state`.

Declare watches and indexes explicitly. Cached reads of undeclared types fail;
use the API reader for uncached reads or access to `managedFields`. Access NKE/CAPI
resources directly and establish ownership before taking over resources managed
by existing controllers.

## API conventions

| Concern | Contract |
| --- | --- |
| Group/version | `internal.nke.nvidia.com/v1`, currently served and stored. “Internal” identifies the storage contract; Go types remain importable. |
| Tenant resources | Namespaced `NKECluster` / `nkeclusters` and `NodePool` / `nodepools`, with corresponding list kinds. |
| Operator resources | Cluster-scoped `MachineImage` / `machineimages`, `ClusterRelease` / `clusterreleases` and `NodePoolRelease` / `nodepoolreleases`, with corresponding list kinds. |
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
  apply -f config/crd/bases/
```

The manifests serve and store only `v1`. Existing `v1alpha1` installations need a
storage-version migration before removing the old version; these manifests alone
do not migrate stored objects.

## Resource examples

### Tenant resources

[NKECluster](apis/v1/nkecluster_types.go) defines cluster access, authentication,
networking, release selection, control-plane class and accelerated runtime.

See the [NKECluster sample](config/samples/internal_v1_nkecluster.yaml).

[NodePool](apis/v1/nodepool_types.go) defines worker capacity and configuration.
`replicas` is required; zero requests zero workers. Omitted rollout defaults to a
rolling update with one surge node; `maxSurge` accepts an integer or percentage.

See the [NodePool sample](config/samples/internal_v1_nodepool.yaml). Apply tenant
samples to an existing org namespace using `kubectl --context <state-context>
apply -n <org-namespace> -f <sample-file>`.

Status carries conditions, observed generation and resolved release. NodePool
also carries its cluster UID binding and observed/ready/updated replica counts;
unknown counts are absent and observed zero counts remain explicit.

### Operator resources

[MachineImage](apis/v1/machineimage_types.go) defines the immutable
identity of a bootable machine image: operating system, architecture, variant,
build date and the NICo iPXE boot contract.

```yaml
apiVersion: internal.nke.nvidia.com/v1
kind: MachineImage
metadata:
  name: nke-nico-vms-24.04-amd64-fde
spec:
  operatingSystem:
    family: ubuntu
    version: "24.04"
  architecture: amd64
  variant: fde
  buildDate: "2026-03-23"
  nico:
    ipxeScript: |
      #!ipxe
      chain ${boot-loader} disk_strategy={{ .DiskSelectionStrategy }}
```

[ClusterRelease](apis/v1/clusterrelease_types.go) defines a control-plane release, and
[NodePoolRelease](apis/v1/nodepoolrelease_types.go) defines a worker release: the
machine image its nodes boot and the initialization applied to them.

```yaml
apiVersion: internal.nke.nvidia.com/v1
kind: NodePoolRelease
metadata:
  name: nke-nico-vms-24.04-amd64-fde
spec:
  machineImageRef:
    name: nke-nico-vms-24.04-amd64-fde
  machineInit:
    userDataTemplate: |
      #cloud-config
      hostname: {{ .Hostname }}
  lifecycle: # mutable fields
    deprecated: false
```

## Kubebuilder layout and generated files

Keep [PROJECT](PROJECT) resource metadata and `cmd/main.go`'s scaffold markers;
Kubebuilder uses them to register new APIs and controllers. `make generate manifests`
does not update `PROJECT`. Keep single-group mode: the `internal` API group would
otherwise generate `internal/controller/internal`, which Go prevents `cmd` from importing.

Edit deployment sources under `config/`, then generate and validate the chart:

```sh
make chart
make chart-check
```

This produces separate `dist/runtime.yaml` and `dist/state.yaml` bundles and
generates chart templates through `config/helm`. Chart metadata, values and Helm
helpers are maintained separately; do not use `--force` when regenerating.

`IMG` defaults to `management-engine:dev`; supply the image you build/publish to
`make docker-build` and `make helm-deploy`. Chart generation does not build an
image or overwrite existing image settings in `chart/values.yaml`.
