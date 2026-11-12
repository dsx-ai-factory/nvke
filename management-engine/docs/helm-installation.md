# Install Cluster Service with Helm

The `cluster-service` chart packages Cluster Service and its datastore resources.
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

From `management-engine/`, run `make chart` to regenerate the Helm chart from
canonical manifests with the pinned Kubebuilder plugin. Then run
`make chart-check` to stage dependencies and validate every installation mode.
CI runs generation checks to reject stale chart output. These checks use Helm
and the module's pinned Go tools without connecting to a Kubernetes cluster.

For a combined installation:

```sh
helm template cluster-service chart --namespace cluster-service-system \
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
to the optional metrics Service. Tenant API integration responsibilities are
summarized below. The chart owns backend, kubeconfig and election namespace
arguments. `manager.args` supplies additional options such as `--namespace`;
if used, that namespace must match the tenant ConfigMaps and requests.

Use Helm's `--create-namespace` during installation, or arrange namespace creation
through the installer; `helm template` sets namespace references but creates
nothing. Tenant namespaces and any explicitly separate state namespace must exist.
The examples demonstrate rendering; live reconciliation is validated separately.

## Tenant API integration responsibilities

The current chart installs the manager and state resources, with an optional
metrics Service. Tenant and operator API Deployments and Services are the next
packaging step in [CLI-126](https://linear.app/nvidia/issue/CLI-126).
The [tenant API registration design](https://docs.google.com/document/d/1sX8NWUMMkxvXnxeamuq0xBt4GnrFiRfueu6maI9d_1I/edit)
describes the planned exposure integration; the detailed operator contract is
tracked in [CLI-129](https://linear.app/nvidia/issue/CLI-129).

| Requirement | Chart / API packaging | Site operator or integration |
| --- | --- | --- |
| Services | Current chart supplies the optional manager metrics Service. CLI-126 supplies separate tenant and operator API Services. | Choose installation namespaces and supply the existing identities and configuration referenced by the workloads. |
| Gateway or proxy | Follow-up exposure packaging adds an optional tenant HTTPRoute and Gateway ingress rule. | Supply a Gateway/listener or configure an existing proxy to target the tenant Service. |
| Request routing | The tenant exposure contract uses `/v1/` paths, forwards the caller's `Authorization` header unchanged, and uses HTTP from the entry point to the tenant Service. | Configure the entry point to honor that contract. |
| TLS, hostname and DNS | Site-specific public names and certificates are supplied through integration settings. | Terminate external TLS and provide the hostname, certificate and DNS record. |
| Network access | The current optional NetworkPolicy admits metrics scrapers. Tenant API ingress rules follow in exposure packaging. | Provide connectivity to the datastore Kubernetes API, NVKE controller and, for authenticated APIs, issuer discovery/JWKS endpoints; allow the entry point to reach the tenant Service. |
| Operator API | API packaging supplies an internal Service; the exposure design publishes no operator route. | Restrict operator access to approved internal callers. |

For DSX OS, the planned site overlay attaches the tenant route to the CSC's
shared HTTPS Gateway, using a hostname such as `cluster-<site>.<suffix>`. DSX OS
integration supplies the site's certificate, DNS record and Gateway settings.
Other operators provide their own Gateway or proxy and the equivalent inputs.
The current DSX values example configures the co-located manager/state deployment;
the tenant entry point is delivered by the follow-up API exposure work.

## Separate installations

Render state on its destination cluster:

```sh
helm template cluster-service-state chart --namespace management-state-system \
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
helm template cluster-service chart --namespace cluster-service-system \
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

Version `0.2.0` renames the development chart to `cluster-service`, with default
release `cluster-service` in namespace `cluster-service-system`. To upgrade an
existing development release in place, keep its release name and namespace and
set `nameOverride: management-engine` to preserve resource names and selectors.
With `make helm-deploy`, retain `HELM_RELEASE=management-engine` and
`HELM_NAMESPACE=management-engine-system`, and include
`--set nameOverride=management-engine` in `HELM_EXTRA_ARGS` alongside the required
configuration. `fullnameOverride` alone does not preserve selectors. Using the
new defaults creates a separate release rather than upgrading the old one.

Chart version `0.2.0` changes the default from remote manager-only to combined
local installation. Existing remote users must explicitly set `state.enabled=false`
and configure `manager.stateAccess`, including their Secret and namespace.
Remove manually supplied kubeconfig, context, election-namespace, backend and
infrastructure-template arguments and their credential volumes; use the named
chart inputs instead.

For development integration, pin the NVKE Git commit containing the chart and
use a manager image built from that same commit, pinned by digest. This pairs
state schemas, permissions and runtime arguments from one source revision.
Chart version `0.2.0` pins catalog chart `0.1.0` in `Chart.lock`; those package
versions alone do not identify a compatible manager image. Record the source
commit and image digest in the site configuration. Cross-revision compatibility
needs separate validation; artifact publication is a separate delivery step.
The example image tags are placeholders for those reviewed installation inputs.

The chart owns its three specific schemas: NVKECluster, ControlPlaneClass and
ClusterReleaseChannel. `state.crds.install=false` delegates those definitions to
an existing installer. Their canonical files live in `config/crd/bases`; canonical manager RBAC lives
in `config/rbac`. `make chart` packages generated copies for Helm. Kustomize
consumes the canonical files directly.

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
regenerates canonical definitions; `make chart` updates Helm output, and
`make chart-deps` stages the pinned dependency
before rendering or packaging chart source.

Reapplying the same pinned chart revision and values to the same Helm release
name and namespace updates the existing objects rather than creating a second
installation. The render checks verify that repeated generation is deterministic.

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
