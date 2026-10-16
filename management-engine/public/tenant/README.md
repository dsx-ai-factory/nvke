# Tenant API contract

This pre-release API is under development and is not recommended for production
use. Backward compatibility is not currently enforced, including within `v1`.

`cluster/v1` defines the public `Cluster` representation of the internal
[`NVKECluster`](../../apis/v1/nvkecluster_types.go). The generated Go package is
`clusterv1`; the gRPC service is `cluster.v1.ClusterService`. This package provides
the contract, generated bindings, and spec mappings. The application service and
deployment are separate work.

Hand off the generated [OpenAPI 2.0 document](gen/cluster/v1/openapi.yaml) to API
consumers. This standalone YAML includes routes, nested schemas, required fields,
constraints, and lifecycle descriptions; its references resolve within the file.
The gateway generator retains some path/query limits only in descriptions;
those limits and cross-field rules remain part of the contract.
The generated [field reference](gen/cluster/v1/FIELDS.md) also lists spec exposure.

## Contract and ownership

[`cluster.proto`](apis/cluster/v1/cluster.proto) defines create, get, list, and
replace operations, request/resource envelopes, pagination, and public status
enums. Public field names are snake_case. Create and replace use separate spec
types; their requests cannot set status or arbitrary Kubernetes metadata.
`resource_version` carries an opaque datastore concurrency token; it is not the
public API version.

The [exposure policy](apis/cluster/v1/exposure.json) selects internal spec paths,
protobuf tags, and `create`/`read`/`replace` access. Unselected fields stay private.
Its `input` options can narrow writable list bounds and provide a public
description; read projections retain the internal schema's bounds.
These operations describe field representation, not caller authorization.
The contract defines no org/project resource or caller-selected storage namespace.

| Location | Ownership |
| --- | --- |
| `apis/cluster/v1/` | Maintained `cluster.proto`, exposure policy, HTTP/OpenAPI configuration, compatibility lock, and contract tests |
| `apis/cluster/v1/spec.proto` | Generated intermediate protobuf schema imported by `cluster.proto` |
| `gen/cluster/v1/` | Generated protobuf/gRPC/gateway Go, route metadata, OpenAPI, and field reference |
| [`mapping`](../../internal/public/tenant/apis/cluster/mapping) | Generated private spec mappings, maintained helpers, and projection tests |

The compiler in [`hack/public-api`](../../hack/public-api) derives spec types,
requiredness, descriptions, and validation constraints from the
[internal CRD](../../config/crd/bases/internal.nvke.nvidia.com_nvkeclusters.yaml).
[`gateway.yaml`](apis/cluster/v1/gateway.yaml) defines HTTP bindings;
[`openapi-options.yaml`](apis/cluster/v1/openapi-options.yaml) configures OpenAPI
output. Generated HTTP route metadata is checked against the compiled protobuf
descriptors.

Generated files have ownership headers and must be regenerated rather than
edited. For spec schema review, read [`spec.proto`](apis/cluster/v1/spec.proto).
The escaped `rawDesc` constants in generated Go encode that schema for the
protobuf runtime.

Future operator contracts can use `public/operator` and
`internal/public/operator`; extract shared definitions or helpers when both
audiences need them.

## Runtime integration decision

When adding the service runtime, connect the HTTP gateway through a generated
gRPC client using `RegisterClusterServiceHandlerFromEndpoint` or
`RegisterClusterServiceHandlerClient` backed by a real gRPC connection. This gives
HTTP and native gRPC requests the same RPC interceptor path for authentication,
authorization, and observability. Use the same wiring in Tilt and production.
The gateway and gRPC server can remain in one process and Deployment.

## Generate and test

Change the internal Go types, maintained proto, or configuration and regenerate.
For internal type changes, first [regenerate the CRD](../../DEVELOPMENT.md#build-and-tests).
From `management-engine`:

```sh
make generate-public-api
make test-public-contract test-unit
```

`test-public-contract` runs `check-public-api` before testing the descriptors,
compiler, and HTTP bindings. `test-unit` includes the mapping tests; `make test`
includes both suites. `check-public-api` can also be run alone to verify drift.

Generation installs pinned local tools, checks Buf formatting/lint, and generates
clients, gateway handlers, and OpenAPI. Buf uses the repository-wide pin; protobuf
and gateway plugins follow their runtime versions in `go.mod`. Annotation schemas
come from that same pinned gateway module in the temporary generation tree. The
drift check compares a clean temporary output tree and detects stale artifacts.

## Compatibility

CI runs `check-public-api` and the contract tests through `make test`. The check
rejects unacknowledged changes to the [contract lock](apis/cluster/v1/wire-lock.json)
and stale generated output. After reviewing a contract change, update the lock
and regenerate:

```sh
make public-api-tools
bash hack/public-api/generate.sh --update-lock
make test-public-contract test-unit
```

`--update-lock` permits intentional breaking changes during development. Policy,
schema, and protobuf validity checks still apply. Review the source, lock,
generated output, and affected tests together.

Before publishing a supported contract, add CI compatibility checks against an
independently protected released baseline, covering protobuf/JSON, exposure,
defaults, replacement behavior, and admission rules. These historical checks are
not implemented yet.

## Mapping boundary

The compiler supports objects, strings, and atomic string lists. Parents must be
selected before children, and children cannot gain operations their parent lacks.
Required fields beneath populated objects need create exposure or an explicit
private default. Unpopulated optional private objects are not inspected, and CRD
defaults are not interpreted.

Writable objects require a required child exposed for the same operation, and
writable lists must be required. These restrictions preserve presence despite
protobuf's inability to distinguish an absent repeated field from an empty one.
Optional read-only lists are supported. Unsupported selected schema shapes fail
generation; internal schema growth never exposes fields automatically.

Mappings validate scalar constraints and list cardinality, project selected spec
fields, and retain private/create-only fields during replacement. They do not
enforce authentication, authorization, networking semantics, lifecycle, or
optimistic concurrency. CEL rules are recorded for compatibility review but are
not evaluated by mappings; the datastore remains authoritative for admission.

### Private create values

The [exposure policy](apis/cluster/v1/exposure.json) supplies required internal
configuration that tenants cannot set at this time. The compiler rejects defaults on public
create fields, so these assignments never override tenant input.

| Stored field | Value | Rationale |
| --- | --- | --- |
| `apiServerAccess.publicAccessEnabled` | `false` | Public API-server access configuration is unsupported by the management-engine provisioning integration at this time. |
| `clusterNetwork.serviceDomain` | `cluster.local` | Matches the DNS domain configured by the NVKE controller's kubeadm renderer. Custom service domains are unsupported at this time. |

The [provisioning validation](../../controllers/nvkecluster_resolve.go) rejects
public access and domains other than `cluster.local`. The NVKE controller selects that domain in
its [defaults](../../../controller/internal/kubeadmconfig/render.go) and applies it
to [kubeadm networking](../../../controller/internal/kubeadmconfig/init.go).
An empty service domain is invalid under the [CRD](../../config/crd/bases/internal.nvke.nvidia.com_nvkeclusters.yaml).

These are service-selected spec values, not observations of a provisioned cluster.
They are excluded from tenant reads and writes. Replacement preserves stored values.
