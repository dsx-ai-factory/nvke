# controller/api

The HTTP API of the Cloud Workflow Engine: one OpenAPI specification, and the Go
client, server and model types generated from it.

It is a Go module of its own so that a consumer can depend on the API types
without pulling in the engine and everything the engine needs. It is imported by
`controller`, `cli` and `controller/test/e2e`.

## What is written by hand, and what is generated

`openapi.yaml` is the source of truth. It is OpenAPI 3.0.3, and it describes 15
paths.

These three files are generated from it and must not be edited:

| File | What it holds |
|---|---|
| `models.gen.go` | The request and response types |
| `server.gen.go` | The server interface the engine implements |
| `client.gen.go` | The client the CLI and the tests use |

The generator is oapi-codegen. It is pinned in `oapi-codegen.mod` and
`oapi-codegen.sum`, which are separate from this module's own `go.mod` so that
the generator's dependencies cannot affect what this module ships. `generate.go`
holds the `go:generate` directive and explains how to move the pin.

`patch-optional-bodies.go` and `prepend-license.go` post-process the generated
output.

## Changing the API

Edit `openapi.yaml`, then regenerate and commit the result:

```bash
make -C controller generate-api
```

Operations, schemas and properties that are not part of the customer-facing
API carry `x-fern-ignore: true`. Fern, which builds the REST API Reference tab
of the documentation site from this file, leaves them out; Swagger UI at
`/api/docs/` and the generated Go are unaffected. Today that is the `infra`
provider, `teleportEnabled`, the legacy `controlPlane` node-pool flag, and the
`/osProfiles` and `/configProfiles` endpoints.

Commit the changed `*.gen.go` files with the specification change. They are
checked in on purpose, so that consumers do not need the generator.

The target runs the generator with the Go toolchain this module's `go.mod`
declares, which is the one CI uses. The copy of the spec embedded in
`server.gen.go` is gzip-compressed and the compressed bytes differ between Go
releases, so generating with the workspace's newer toolchain would produce a
blob the gate rejects.

If you forget, CI fails. `make -C controller generate-check` regenerates and then
fails if the committed output differs, and the merge gate runs it. The failure
names the files that are out of date.

## Rendering the reference

Fern renders `openapi.yaml` into the REST API Reference tab of the documentation
site (`fern/generators.yml`, `docs/index.yml`). For one self-contained page, to
review a change or to hand to a customer:

```bash
make -C controller/api docs    # writes controller/.local/api-reference.html; needs Node
```

Both honour `x-fern-ignore`: Fern reads it directly, and `redocly.yaml` tells
Redocly to drop the same elements before rendering. The `API docs` workflow
builds this page for every pull request that touches the spec or the docs and
attaches it as the `api-reference` artifact, runs `fern check`, and publishes a
Fern preview of the whole site, API Reference tab included, with the URL in the
job summary. Fork pull requests get no preview, because GitHub withholds secrets
from them; push the branch to this repository and dispatch the workflow instead.
On `main` it publishes the site.

## Checks

```bash
make -C controller/api lint
make -C controller/api test
```

`lint` also runs `lint-spec`, which lints `openapi.yaml` with vacuum against
`openapi-lint.yaml` and fails the merge gate on any finding: every operation,
parameter, schema and property must carry a description, every example must
match its schema, and a description must not sit beside a bare `$ref`, because
Redoc, Swagger UI and the generator ignore `$ref` siblings. Put the text on the
referenced schema. In a response schema the `$ref` may instead be wrapped in
`allOf`; request schemas must not do that, because kin-openapi then reports a
validation failure inside the property as a raw schema dump instead of the
one-line request-body error. The `infra` properties of the request schemas
carry `x-fern-ignore` beside their `$ref` on purpose; `openapi-lint-ignore.yaml`
lists them so the rule stays an error everywhere else.
The rules, and why each is on or off, are commented in the ruleset.

This module has **no test files**. `make test` therefore compiles the module and
reports success without running anything. That matters when you change the API:
nothing here will catch a break, and the modules that will are `controller`,
`cli` and `controller/test/e2e`. Build those too before you open a pull request.

There is no `fmt` target, on purpose. Formatting the generated files belongs to
the generator.
