# Install Management Engine with Helm

The `management-engine` chart packages the manager and its datastore resources.
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

From `management-engine/`, run `make chart-check` to refresh the generated CRD
and RBAC assets, lint the chart, and test its rendered resources. CI also runs
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

For a DSX CSC render, merge the working template example with site settings:

```sh
helm template cluster-service chart --namespace cluster-service-system \
  -f config/samples/runtime-values.yaml -f config/samples/dsx-values.yaml > dsx.yaml
```

Replace the example registry, image tag, endpoint and tenant namespace with
reviewed site inputs. The manager needs network access to the datastore's
Kubernetes API and the configured NVKE controller endpoint. Scrapers need access
to the optional metrics Service. Tenant API Services, gateway routing, `/v1/`,
TLS and DNS follow the [API exposure contract](https://nvidia.atlassian.net/browse/NKX-19411);
the operator API remains internal. The chart owns backend, kubeconfig and election namespace
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
SubjectAccessReview. The state release includes those permissions when
`metrics.enabled` and `metrics.secure` are true. Configure those settings
consistently for separate releases. Scraper identity and reachability are
operator configuration; disable metrics until they are configured.

## CRD ownership, upgrades and retention

Chart version `0.2.0` changes the default from remote manager-only to combined
local installation. Existing remote users must explicitly set `state.enabled=false`
and configure `manager.stateAccess`, including their Secret and namespace.
Remove manually supplied kubeconfig, context, election-namespace, backend and
infrastructure-template arguments and their credential volumes; use the named
chart inputs instead. The examples use development image tags; pin a compatible
published image and chart revision for an installation.

CRDs come from `config/crd`, and manager RBAC comes from the generated controller
markers. `make manifests` updates chart assets; it does not regenerate the
maintained Helm templates. Commit refreshed assets with their source changes.

Choose one installer for each CRD in a cluster. The NVKE controller chart already
includes MachineImage, ClusterRelease and NodePoolRelease by default. When this
chart owns all Management Engine CRDs, disable that chart's `releaseCRDs.install`
before installation. When an existing installer owns the full CRD set, set
`state.crds.install=false` here. A second Helm release cannot silently take over
an existing owner's CRDs; inspect and deliberately migrate ownership first.
The proposed shared CRD chart can replace this packaging once its ownership
contract is agreed.

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
