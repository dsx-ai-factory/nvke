# controller/api

The HTTP API of the Cloud Workflow Engine: one OpenAPI specification, and the Go
client, server and model types generated from it.

It is a Go module of its own so that a consumer can depend on the API types
without pulling in the engine and everything the engine needs. It is imported by
`controller`, `cli` and `controller/test/e2e`.

## What is written by hand, and what is generated

`openapi.yaml` is the source of truth. It is OpenAPI 3.0.3, and it describes 17
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

Commit the changed `*.gen.go` files with the specification change. They are
checked in on purpose, so that consumers do not need the generator.

If you forget, CI fails. `make -C controller generate-check` regenerates and then
fails if the committed output differs, and the merge gate runs it. The failure
names the files that are out of date.

## Checks

```bash
make -C controller/api lint
make -C controller/api test
```

This module has **no test files**. `make test` therefore compiles the module and
reports success without running anything. That matters when you change the API:
nothing here will catch a break, and the modules that will are `controller`,
`cli` and `controller/test/e2e`. Build those too before you open a pull request.

There is no `fmt` target, on purpose. Formatting the generated files belongs to
the generator.
