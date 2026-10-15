# Management Engine

The management engine is designed to manage NVKE workload clusters through
declarative APIs and reconciliation built on Cluster API (CAPI) conventions.
Its architecture separates user and operator APIs from internal Kubernetes
resources and lifecycle controllers. This module provides the internal resource
types, generated CRDs, controller-manager runtime, and tenant API contract generation.

| Path | Purpose |
| --- | --- |
| `apis/v1` | Resource types and scheme registration |
| `config/crd/bases` | Generated CRDs |
| `cmd` | Connection configuration and manager startup |
| `controllers` | Reconcilers |
| [`public/tenant`](public/tenant/README.md) | Tenant contract inputs in `apis/`; generated clients, HTTP bindings, and references in `gen/` |
| `hack/public-api`, `internal/public/tenant/apis/cluster/mapping` | Exposure compiler and generated spec mappings with their helpers |

## Development

See [Development](DEVELOPMENT.md) for setup, Tilt, tests, and scaffolding.
Repository-wide guidance lives in [development conventions](../docs/development.md)
and the [workspace guide](../docs/go-workspace.md).
Run commands below from `management-engine/`.

```sh
make build test lint
make generate manifests # after API or RBAC changes
git diff --exit-code -- apis/v1 config/crd/bases config/rbac/role.yaml chart # check generated-file drift
make test-controller # controller integration tests (envtest)
make test-install # requires a prepared environment without Tilt
make -C .. chart-crds # after changing MachineImage, ClusterRelease, or NodePoolRelease
make -C .. chart-crds-check # verify the controller chart's generated CRDs
make chart-check # generate, lint, and render the management-engine chart
```

For a drift check, start with a clean working tree before regeneration.
`make test` includes `test-controller`; the separate command runs that suite alone.
See [installation test setup](DEVELOPMENT.md#tests) before running `test-install`.

## Deployment topology

| Cluster | Contents |
| --- | --- |
| Services | Manager and credentials (`chart` / `config/default`) |
| Datastore | CRDs, resources, identity/RBAC, and election Lease (`config/state`) |

The chart mounts the `management-state-client` Secret at `/etc/management-api`.
Supply a `kubeconfig` key and any credential files it references, and set the
datastore election namespace in [runtime values](config/samples/runtime-values.yaml).
Local development provisions these credentials; other deployments must supply
and renew them.

Install datastore resources, then deploy through Kustomize or Helm:

```sh
make install-state STATE_CONTEXT=datastore
make deploy HOST_CONTEXT=services IMG=management-engine:dev
# Or, with the services namespace and credential Secret already provisioned:
make helm-deploy HOST_CONTEXT=services IMG=management-engine:dev HELM_EXTRA_ARGS='-f /path/to/values.yaml'
```

Replace the context and image names for your deployment. `deploy` creates the
`management-engine-system` namespace; the manager requires
its credential Secret before starting. `IMG` defaults to `management-engine:dev`.
`make undeploy` and `make helm-uninstall` preserve the runtime namespace, external
Secrets, and datastore resources. Deleting the runtime namespace removes its
remaining contents.

## Run the controller manager

Install the datastore CRDs and authorize the manager's identity before running:

```sh
make run MANAGER_ARGS="--kubeconfig /path/to/kubeconfig --context datastore --leader-election-namespace management-state-system"
```

`make run` builds and starts `bin/management-engine`; use its `--help` for flags
and defaults.

### Connection configuration

Credential selection follows `--kubeconfig`, `KUBECONFIG`, in-cluster credentials,
then `~/.kube/config`. `--context` selects a kubeconfig context; missing or empty
kubeconfig entries can fall back to in-cluster credentials. Deployments explicitly
select the datastore kubeconfig and disable host ServiceAccount-token mounting.

Use HTTPS with certificate verification for the datastore. Discovery, resource
access, and election use the same datastore.

The [datastore RBAC](config/state) defines resource and leader-election permissions.
Keep it aligned with controller watches and writes.
Constructing the manager does not verify API availability or permissions.

### Health and leadership

Use resource status and conditions to assess reconciliation; process health and
readiness alone do not establish workload readiness. See
[installation tests](DEVELOPMENT.md#tests) for runtime validation.

Run one deployment group per datastore. Replicas share the
`management-engine.nke.nvidia.com` Lease in an explicitly selected, existing
namespace. That namespace does not restrict controller watches. The chart enables
election even for a single replica to cover rolling updates.

Stop deployed replicas before debugging with
`--leader-elect=false`, and stop the debug process before restoring them.

## API conventions

All types use `internal.nvke.nvidia.com/v1`. Public tenant and operator contracts
(`nke.nvidia.com` and `operator.nke.nvidia.com`) are separately versioned
projections; do not expose internal structs directly.

| Resource | Scope | Definition and example |
| --- | --- | --- |
| NVKECluster | Org namespace | [Cluster configuration](apis/v1/nvkecluster_types.go), [sample](config/samples/internal_v1_nvkecluster.yaml) |
| MachineImage | Cluster | [Immutable image identity and NICo boot contract](apis/v1/machineimage_types.go) |
| ClusterRelease | Cluster | [Control-plane image and initialization](apis/v1/clusterrelease_types.go) |
| ClusterReleaseChannel | Cluster | [Release catalog](apis/v1/clusterreleasechannel_types.go), [sample](config/samples/internal_v1_clusterreleasechannel.yaml) |
| NodePoolRelease | Cluster | [Worker image and initialization](apis/v1/nodepoolrelease_types.go) |

| Concern | Contract |
| --- | --- |
| Tenancy | One namespace per org per datastore. Public APIs derive org identity from authentication and enforce resource and field authorization. |
| Identity | Immutable client-selected DNS-subdomain names (at most 253 characters); UIDs distinguish recreations. Provider names may have shorter limits. |
| Concurrency | Preserve opaque `resourceVersion` and field ownership. Spec expresses desired state; controllers report progress through status. |
| Conditions | Use `metav1.Condition` and `observedGeneration`. Missing or stale conditions do not establish readiness. Unknown counts are absent; observed zero counts are explicit. |
| References | Resolve tenant references within the org namespace and pin lifecycle ownership by UID. Release references select operator-published configuration. |
| Node pools | `spec.nodePools` embeds [worker configuration](apis/v1/nodepool_types.go) within NVKECluster; see the [cluster sample](config/samples/internal_v1_nvkecluster.yaml). |
| Replicas | `spec.nodePools[].replicas` expresses desired worker count; zero requests zero workers. Rollout policy controls replacement. |
| Deletion | Use deletion timestamps and controller-owned finalizers. |

Schema validation cannot prove reference existence, current UIDs, or caller
privileges. Namespace scoping also requires API authorization and controller RBAC.

## Install NVKE resource types

For CRDs only, without the datastore identity/RBAC bundle:

```sh
kubectl --kubeconfig /path/to/kubeconfig --context datastore apply -f config/crd/bases/
```

See [sample usage](DEVELOPMENT.md#sample-resources) for creating tenant resources.
