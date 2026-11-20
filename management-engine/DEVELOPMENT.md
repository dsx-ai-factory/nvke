# Cluster Service Development

Run commands from `management-engine/` with GNU Make (`gmake` on macOS).
Generation targets install pinned tools. Change the sources, not generated files.

## Internal Kubernetes API

Edit Go types and kubebuilder markers under `apis/v1`. For a new kind, register
it with `SchemeBuilder` and update `PROJECT`. Add the new CRD to
`config/crd/kustomization.yaml`, which the Kustomize base consumes and
`make install-state` applies to the state cluster.

```sh
make generate manifests
make test-api
```

This regenerates deepcopy methods, canonical CRDs and RBAC, and
`dist/install.yaml`. Runtime manifests and permission rules are maintained under
`config/`. Preserve `PROJECT` metadata and scaffold markers.

## Public HTTP APIs

The tenant and operator contracts are generated into `api/tenant.openapi.yaml`
and `api/operator.openapi.yaml` from the registered resources and their schemas.
Edit the source types and registrations, then run:

```sh
make generate-openapi generate-openapi-check
make test-unit test-integration lint lint-openapi
```

The `Naming` fields returned by `Tenant()` and `Operator()` in
`pkg/publicapi/surfaces.go` independently select `resource.SnakeCase` (the default)
or `resource.CamelCase`. This is a source setting shared by the HTTP server and
OpenAPI generator, not a deployment flag.
It applies to declared JSON fields, query parameters, selector paths, error
field paths and error codes such as `already_exists`. Requests must use the
selected spelling. Dictionary keys, other enum values, headers and opaque
tokens retain their values. Kubernetes objects keep their canonical field names.

The snake_case documents omit datastore CEL expressions because those rules
refer to canonical Kubernetes field names. Datastore admission still enforces
them. The NVKE controller's separate REST API retains its existing contract.

## Helm chart generation

Run `make chart` after changing a runtime manifest or state definition. It runs
the pinned [Kubebuilder Helm plugin](https://book.kubebuilder.io/plugins/available/helm-v2-alpha)
in fresh staging, then applies the packaging extensions in `hack/chart/`.
`chart/` contains generated output; edit the canonical manifests or extension
sources rather than repairing Helm templates by hand.

The plugin supplies workload settings, probes, security contexts and runtime
values. Extensions supply state/controller switches, validated connection
references, conditional mounts and binding identities. Their insertion points
are checked against the pinned plugin output. Changes to ordinary workload
settings therefore propagate from Kustomize through generation.

`make chart-check chart-assets-check` verifies the generated chart and all
installation modes. Fresh staging regenerates runtime defaults before adding
chart-specific values; in-place plugin generation would preserve old values.

Review and commit generated changes together with their sources.
