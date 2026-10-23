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

See [API generation](DEVELOPMENT.md) for internal CRDs and public tenant contracts.
Repository-wide guidance lives in [development conventions](../docs/development.md)
and the [workspace guide](../docs/go-workspace.md).
Run commands below from `management-engine/`.

```sh
make build test lint
make generate manifests # after API or RBAC changes
git diff --exit-code -- apis/v1 config/crd/bases config/rbac/role.yaml chart # check generated-file drift
make test-controller # controller integration tests (envtest)
make -C .. chart-crds # after changing MachineImage, ClusterRelease, or NodePoolRelease
make -C .. chart-crds-check # verify the NVKE controller chart's generated CRDs
make chart-check # generate, lint, and render the management-engine chart
```

For a drift check, start with a clean working tree before regeneration.
`make test` includes `test-controller`; the separate command runs that suite alone.

### Tilt provider

Add `management-engine` to the provider list in `controller/tilt_config.json`,
keeping the infrastructure providers you use. For CAPD:

```json
{
  "providers": ["capi_core", "bgp-containers", "capd", "management-engine"]
}
```

After the controller's normal Kind/CAPI setup, run from `management-engine/`:

```sh
make tilt-up
```

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
| Conditions | Use `metav1.Condition` with per-condition `observedGeneration` matching the NVKECluster's generation. There is no aggregate `status.observedGeneration`. Missing or stale conditions do not establish readiness. Unknown counts are absent; observed zero counts are explicit. |
| References | Resolve tenant references within the org namespace and pin lifecycle ownership by UID. Release references select operator-published configuration. |
| Node pools | `spec.nodePools` embeds [worker configuration](apis/v1/nodepool_types.go) within NVKECluster; see the [cluster sample](config/samples/internal_v1_nvkecluster.yaml). |
| Replicas | `spec.nodePools[].replicas` expresses desired worker count; zero requests zero workers. Rollout policy controls replacement. |
| Deletion | Use deletion timestamps and controller-owned finalizers. |

Schema validation cannot prove reference existence, current UIDs, or caller
privileges. Namespace scoping also requires API authorization and controller RBAC.

