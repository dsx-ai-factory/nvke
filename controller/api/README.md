# controller/api

The HTTP API of the Cloud Workflow Engine: one OpenAPI specification, and the Go
client, server and model types generated from it.

It is a Go module of its own so that a consumer can depend on the API types
without pulling in the engine and everything the engine needs. It is imported by
`controller`, `cli` and `controller/test/e2e`.

## What is written by hand, and what is generated

`openapi.yaml` is the source of truth. It is OpenAPI 3.0.3, and it describes 15
paths. It is the only authored schema in the directory, and the only file whose
change can break a consumer.

These three files are generated from it and must not be edited:

| File | What it holds |
|---|---|
| `models.gen.go` | The request and response types |
| `server.gen.go` | The server interface the engine implements |
| `client.gen.go` | The client the CLI and the tests use |

The other authored files beside the specification configure what is done to it,
and are not part of the API surface:

| File | What it governs |
|---|---|
| `config-{models,server,client}.yaml` | What oapi-codegen emits |
| `openapi-lint.yaml`, `openapi-lint-ignore.yaml` | The spec lint (`make lint`) |
| `openapi-compat-waivers.yaml` | Accepted breaking changes ([Compatibility](#compatibility)) |
| `redocly.yaml` | The rendered reference page |

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
of the documentation site from this file, leaves them out. Swagger UI at
`/api/docs/` loads `GET /openapi-public.yaml`, which drops the same surface.
`GET /openapi.yaml` and the generated Go keep it. Today that is the `infra`
provider, `teleportEnabled`, the legacy `controlPlane` node-pool flag, and the
`/releases` and `/configProfiles` endpoints.

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

## Compatibility

`generate-check` proves the committed Go matches the specification. It says
nothing about whether the specification change itself is safe. A removed field,
a narrowed type or a newly required property regenerates perfectly and breaks
every client already built against the old specification. Those are different
questions, and there is a separate gate for the second:

```bash
make -C controller api-compat-check
```

It compares `openapi.yaml` against the newest published `controller/api` version
tag and fails on any breaking change that is not waived. The merge gate runs it.
How the baseline is chosen, how a waiver is written, and the full list of
conditions that fail rather than pass, are in
[`../hack/openapi-compat/README.md`](../hack/openapi-compat/README.md).

### What a client is holding

There is one surface. `/api` carries no version segment, no version header and
no content negotiation: the running management plane serves exactly the
specification in this file, and `GET /api/version` reports the build it came
from.

What a consumer can pin is the Go module:

```bash
go get github.com/dsx-ai-factory/nvke/controller/api@v0.0.17
```

`controller-api-tag.yml` publishes a `controller/api/vX.Y.Z` tag for every `main`
commit that touches this directory. That tag is an **immutable snapshot, not a
semantic-version contract** — it only ever increments the patch number, so no
tag has ever signalled a break and none ever will. Compatibility is held by the
gate above instead of by the version number, which is why a break has to be
waived by name rather than released as a major.

`info.version` in `openapi.yaml` (`0.1.0-alpha`) describes the maturity of the
API, not the module. The surface is public preview.

### Making a change compatibly

Reach for the shape that adds rather than the one that edits:

| Want to | Compatible way |
|---|---|
| Add data to a response | Add an optional property. A client that does not know it ignores it. |
| Take new input | Add an optional request property with a server-side default. Making an existing property required is breaking. |
| Replace a field | Add the new one, deprecate the old one, keep serving both. |
| Change what a field means | Do not. Add a new field; the old one keeps its old meaning. |
| Narrow a type or an enum | Breaking. Widening an enum of *response* values is also breaking for a client that switches exhaustively — call it out in review. |
| Replace an endpoint | Add the new one and deprecate the old one. |

### Deprecating, then removing

Deprecation is additive, so it passes the gate on its own. Removal is the
breaking step — and for an **operation** it is the one case that can clear the
gate without a waiver, provided the notice was real and has run out.

1. Mark the operation `deprecated: true`, and give it an `x-sunset` date at
   least **180 days** out:

   ```yaml
     /widgets:
       get:
         operationId: listWidgets
         deprecated: true
         x-sunset: "2027-06-01"
   ```

   OpenAPI has no sunset concept of its own, only the `deprecated` boolean;
   `x-sunset` is the extension the surrounding tooling has converged on. The
   gate holds this declaration to its word: `deprecated: true` with no
   `x-sunset`, an `x-sunset` that is not a `YYYY-MM-DD` date, a date less than
   180 days away, and an `x-sunset` on an operation that is not marked
   deprecated all fail. Each is a deprecation that announces nothing a consumer
   can act on.

2. Say so in the description too: what replaces it, and from when. The
   description is what Swagger UI and the Fern reference render, so it is where
   a consumer actually reads it. `x-sunset` is for the gate; the prose is for
   the person.

3. Keep serving it. A deprecated operation still has its documented behaviour.

4. Remove it once the date has passed. **No waiver** — the gate reports the
   removal as one it did not have to be asked about, because the promise was
   made, kept and discharged. Removing it before the date, or without having
   deprecated it, still fails and still needs a waiver.

Only operations have this route. A deprecated **property** or **parameter** has
no sunset rule behind it, so removing one is a break like any other and takes a
waiver. That is a deliberate limit rather than an oversight: the engine reports
an operation removal as a single change naming the operation, which is what
makes "this exact removal was announced" a decidable question, and a property
removal is reported in a shape where it is not.

The notice period is one number for the whole API, `deprecationNoticeDays` in
[`../hack/openapi-compat/lifecycle.go`](../hack/openapi-compat/lifecycle.go). It
is a promise to consumers, so moving it is a decision taken once and in the
open, not a flag raised to get one pull request through.

### When a waiver is the right answer

Mostly it is not: the entries below are the cases where it is.

- **Surface marked `x-fern-ignore: true`.** It is not customer-facing — today
  the `infra` provider, `teleportEnabled`, the legacy `controlPlane` node-pool
  flag, and `/releases` and `/configProfiles`. The gate still sees a break,
  because the specification is one document; the blast radius is in-tree.
- **A field that never shipped**, added and removed between two published tags.
- **A deliberate break the team has agreed to take**, where the waiver is the
  record of that decision rather than a way around it.

Removing a deprecated operation whose sunset has passed is **not** on this list,
and does not need an entry. If you find yourself writing a waiver for a removal,
check whether the deprecation route above applies first — it usually should
have.

A waiver is deleted once the baseline tag has moved past the release that
carried the break. A waiver that matches nothing fails the gate, so it cannot be
forgotten. Recording the `baseline` it was written against is the one
exemption: when the check runs against a stable `controller/api/vX.Y.Z` tag
*later* than that one, the waiver reports as expired instead of failing, so a
break that has since shipped does not fail unrelated branches. Any other
relation leaves it failing — the same tag, an earlier one, or a baseline that
is not an orderable release tag at all (a commit SHA, a branch, a release
candidate). Checking against an older or unorderable ref therefore does not
retire a waiver.

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

Two checks live outside this module's Makefile, because both run from
`controller/`:

```bash
make -C controller generate-check    # the committed Go matches the spec
make -C controller api-compat-check  # the spec change does not break a client
```

The authored tests in this module cover the stdlib OIDC issuer under
`testutil/oidc`, which the controller reference and end-to-end suites share.
`make test` runs those tests and compiles the generated API package. It does not
catch a break in generated Go consumers, so build `controller`, `cli` and
`controller/test/e2e` too before opening a pull request. A break in the contract
is `api-compat-check`'s job, and that check runs from `controller/`.

There is no `fmt` target, on purpose. Formatting the generated files belongs to
the generator.
