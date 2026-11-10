# Install Cluster Service with Helm

The `management-engine` chart packages Cluster Service and its datastore resources.
Both are enabled by default in the same Kubernetes cluster. An existing NVKE
controller provides the provisioning REST API used by the manager.

| Installation | `state.enabled` | `controller.enabled` | State access |
| --- | --- | --- | --- |
| Combined, default | `true` | `true` | Pod ServiceAccount |
| State only | `true` | `false` | Bind existing manager identities |
| Controller with existing local state | `false` | `true` | Pod ServiceAccount |
| Controller with remote state | `false` | `true` | Existing kubeconfig Secret |

`manager.enabled=false` remains supported to disable the manager. The new
`controller.enabled` switch also gates its ServiceAccount and observability
resources. State-only rendering needs no image or credential configuration.

## Generate manifests

From `management-engine/`, run `make chart-check` to regenerate canonical schemas
and RBAC, stage the shared dependency, and validate the chart. CI also runs
`make chart-assets-check` to reject uncommitted asset drift. These checks use Helm
and the module's pinned Go tools without connecting to a Kubernetes cluster.

For a combined installation:

```sh
helm template management-engine chart --namespace management-engine-system \
  -f config/samples/runtime-values.yaml > combined.yaml
```

The sample delivers a development infrastructure template ConfigMap through
`state.configMaps`, a list of objects with `name`, `namespace` and `data`.
Create the tenant namespace (`tenant-a` in the sample) before installation, and
replace the contents with the templates for your provider.
Go template expressions inside those strings remain literal for the manager to
interpret. `manager.infrastructureTemplateConfigMap` names that ConfigMap. The manager
looks up that name in each NVKECluster's tenant namespace. Supply one entry per
tenant namespace, reusing the same ConfigMap name; when reusing state, publish
those ConfigMaps on the datastore cluster before creating tenant clusters.

Set `manager.image.repository` and `manager.image.tag` to a published image,
or use a digest reference as the repository. Set `manager.nvkeController.endpoint`
to the existing provisioning service. If it requires a bearer token, supply
`manager.nvkeController.tokenSecret.name` and `.key`; the chart mounts that key
as `/etc/nvke-controller/auth/token` and sets the corresponding argument. The
Secret must already exist in the runtime namespace. An example credential overlay
is in `config/samples/nvke-controller-values.yaml`.

For DSX, the intended CSC deployment co-locates Cluster Service and its APIs,
the datastore and the existing NVKE controller in the same cluster. This uses
local ServiceAccount access and reuses the catalog CRDs owned by the NVKE
controller. Split installations remain available for other deployment needs.
Merge the working template example with site settings:

```sh
helm template cluster-service chart --namespace cluster-service-system \
  -f config/samples/runtime-values.yaml -f config/samples/dsx-values.yaml > dsx.yaml
```

Replace the example registry, image tag, endpoint and tenant namespace with
reviewed site inputs. The manager needs network access to the datastore's
Kubernetes API and the configured NVKE controller endpoint. Scrapers need access
to the optional metrics Service. Coordinate API Services and gateway exposure with the site integration. Tenant
routes use `/v1/`, with TLS and DNS managed by the site. Keep operator API access
internal. The chart owns backend, kubeconfig and election namespace
arguments. `manager.args` supplies additional options such as `--namespace`;
if used, that namespace must match the tenant ConfigMaps and requests.

Use Helm's `--create-namespace` during installation, or arrange namespace creation
through the installer; `helm template` sets namespace references but creates
nothing. Tenant namespaces and any explicitly separate state namespace must exist.
The examples demonstrate rendering; live reconciliation is validated separately.

## Separate installations

Render state on its destination cluster:

```sh
helm template management-engine-state chart --namespace management-state-system \
  -f config/samples/state-values.yaml > state.yaml
```

The state sample enables the three catalog CRDs as well as the three Cluster
Service CRDs for a fresh datastore. Set `nvke-crds.enabled=false` when the NVKE
controller or a dedicated catalog release already owns them on that cluster.

Set `state.rbac.subjects` to the manager's actual identity. Local identities
use a ServiceAccount name and namespace. An authenticated external SPIFFE
identity can use a Kubernetes `User` subject, according to the datastore's
authentication mapping. A SPIFFE URI in a binding does not itself establish trust.
Bindings grant the generated manager permissions and namespace-scoped leader
election permissions. `state.rbac.tenantAPIBindings` accepts a namespace and
subjects per tenant; `state.rbac.operatorAPISubjects` binds existing operator API
identities. These optional bindings reuse the current API roles, independently
of API workload packaging. Empty lists grant no API permissions. Use one tenant
binding entry per namespace and list all its identities together.

For a controller in the same cluster, disable state, set
`manager.stateNamespace`, and bind its ServiceAccount in the state release.
No kubeconfig is mounted. State-only releases with an empty subject list render
roles without bindings, so permissions can be supplied independently.

For a controller on another cluster:

```sh
helm template management-engine chart --namespace management-engine-system \
  -f config/samples/remote-controller-values.yaml > controller.yaml
```

Create the referenced Secret in the controller's namespace before installation.
Its configured key is mounted as `/etc/management-api/kubeconfig`; its
`current-context` selects the datastore connection. Issuance,
delivery and rotation belong to the identity provisioning workflow. The chart
renders references only. Set an explicit state namespace; install state and its
RBAC first. Kubernetes API access, DNS and network connectivity must work between
the runtime and datastore clusters.

`state.namespace` selects the namespace for state-side election permissions and
defaults to the Helm release namespace. `manager.stateNamespace` selects the
manager's election namespace, defaulting to `state.namespace` or the release
namespace. They must agree in a combined installation. Tenant requests and
templates remain in tenant namespaces; release catalog resources are cluster-scoped.
Any overridden namespace must exist. Rendering validates references and namespace
agreement; external objects and caller permissions require installation checks.

Secure manager metrics use the datastore API for TokenReview and
SubjectAccessReview. Combined local installations grant these permissions when
secure metrics are enabled. A separate state release grants them only when
`state.rbac.metricsAuth=true`; bind the manager identity through
`state.rbac.subjects`. This grants the manager permission to check requests;
the scraper separately needs permission to GET `/metrics`.

The remote example disables metrics and its ServiceMonitor. A runtime-cluster
ServiceAccount token is not automatically valid in the datastore cluster. Before
enabling remote collection, configure a scraper identity accepted by the
datastore and its `/metrics` permission, plus TLS trust and network access.

## NKTP integration

The [NVKE telemetry pipeline](../../docs/nktp/components/node-collector.md)
collects pod stdout/stderr and discovers ServiceMonitors. Cluster Service can
render a monitor with `prometheus.enabled=true` when metrics are enabled. For a
co-located deployment, verify the collector identity has GET `/metrics` access
and that the monitor's bearer-token file is available in the collector. The
current monitor uses self-signed TLS unless certificate management is enabled.
Confirm the site's TLS policy and configure the target allocator's access to any
referenced certificate Secrets before enabling collection.

When the chart's network policy is enabled, it admits scrapers from namespaces
labeled `metrics: enabled`; the NKTP namespace must match that rule or the site
must provide equivalent access. Logs need no chart-specific exporter.
Validate monitor discovery, authentication, TLS and successful scraping with the
telemetry team before enabling site collection. Remote collection also needs
the datastore identity configuration described above.

## CRD ownership, upgrades and retention

Chart version `0.2.0` changes the default from remote manager-only to combined
local installation. Existing remote users must explicitly set `state.enabled=false`
and configure `manager.stateAccess`, including their Secret and namespace.
Remove manually supplied kubeconfig, context, election-namespace, backend and
infrastructure-template arguments and their credential volumes; use the named
chart inputs instead. The examples use development image tags; pin a compatible
published image and chart revision for an installation.

The chart owns its three specific schemas: NVKECluster, ControlPlaneClass and
ClusterReleaseChannel. `state.crds.install=false` delegates those definitions to
an existing installer. Their canonical files live in `chart/files/crds`; canonical
manager RBAC lives in `chart/files/roles`. Kustomize uses the same files.

MachineImage, ClusterRelease and NodePoolRelease are supplied by the shared
[`nvke-crds` catalog chart](../../controller/deploy/charts/nvke-crds/README.md).
The pinned dependency declares the supported catalog package, but defaults to
`nvke-crds.enabled=false`: the NVKE controller normally already owns these APIs.
Installing both application charts with their defaults therefore gives them
separate schema ownership without duplicate CRDs.

For a fresh deployment, the recommended arrangement is one dedicated catalog
release installed before both consumers, with `releaseCRDs.install=false` on the
NVKE controller and `nvke-crds.enabled=false` here. Wait for CRD establishment
before creating catalog records. Helm dependencies do not express dependencies
on another installed release; deployment tooling must order these releases and
pin compatible versions. For a standalone installation, explicitly enable
`nvke-crds.enabled=true` in exactly one application release instead. On separate
datastore clusters, provide the required APIs in each cluster.

Follow the shared chart's explicit migration procedure before changing an
existing catalog owner. `state.enabled=false` disables this chart's schemas,
state RBAC/configuration and catalog dependency together. `make manifests`
regenerates canonical definitions; `make chart-deps` stages the pinned dependency
before rendering or packaging chart source.

CRDs are rendered as Helm resources with `helm.sh/resource-policy: keep`.
Helm applies their schemas on upgrades and retains them on uninstall or when
state is disabled. Retaining CRDs preserves their custom resources; review
schema compatibility and back up state before an upgrade. A rollback does not
undo data migrations. Retained objects still carry their former Helm ownership
metadata, so transferring them to another release requires an explicit migration.

The chart does not create or delete its release namespace. State RBAC and
bootstrap ConfigMaps are removed with the state release; retained tenant
resources therefore remain but cannot be managed until access and configuration
are restored. Do not delete a state namespace or CRD as routine cleanup: that
can delete tenant resources. Moving state between clusters requires a separate
data migration; changing chart switches does not move it.
