# API compatibility

`openapi.yaml` is the contract the Cloud Workflow Engine serves and that
`nkectl`, the e2e suite, Operator automation and the REST API Reference are all
built from. This document says what that contract promises, how a change to it
is checked, and what to do when a change breaks it.

The check is `make -C controller/api compat-check`, and the merge gate runs it.

## Why this is separate from generate-check

`make -C controller generate-check` asks whether the committed `*.gen.go` files
match `openapi.yaml`. That question says nothing about compatibility. Remove a
response field, narrow a type, or add a required request property, and
generate-check passes — the generated code matches the new spec perfectly —
while every client built against the old spec breaks.

Regeneration correctness and compatibility are different questions. The gate
described here is the second one.

## What is authored and what is derived

| File | Role |
|---|---|
| `openapi.yaml` | Authored. The contract, and the only input to the check. |
| `models.gen.go`, `server.gen.go`, `client.gen.go` | Derived. Compared by `generate-check`, not by this check. |

The comparison reads `openapi.yaml` and nothing else. A `.gen.go` file cannot
carry a compatibility verdict: it is a projection of the spec into one language,
and a consumer on the wire is bound by the spec.

`x-fern-ignore` is not honoured here. It marks the surface the documentation
site leaves out, not surface that is free to change: `nkectl` and
`controller/test/e2e` are generated from the whole file, so an endpoint hidden
from the published reference still has clients.

## How the API is versioned

Two version numbers exist, and they mean different things.

`controller/api/vX.Y.Z` git tags are the module version. Every change to
`controller/api/**` on `main` gets one, allocated by
[`controller-api-tag.yml`](../../.github/workflows/controller-api-tag.yml) as the
next patch. The number says "this is a distinct, immutable snapshot a consumer
can pin". It does **not** claim the snapshot is compatible with the one before
it — it is allocated before anyone has asked that question. The check below is
what answers it.

`info.version` in `openapi.yaml` is the API's release stage, which readers of
the REST API Reference see. It is `0.1.0-alpha`: the API is in public preview.

Alpha is the reason this gate blocks rather than merely reports. A breaking
change to an alpha API is allowed — what is not allowed is making one without
noticing. The gate converts a silent break into an explicit, reviewed line of
configuration.

## The baseline

The newest published `controller/api/vX.Y.Z` tag.

That is the intentional baseline: the most recent spec a consumer could actually
have pinned. The merge base is not, because it is a point in this branch's
history that no consumer ever saw, and `main` is not, because an unreleased
commit on `main` is not something anyone depends on either.

Compare against a different one when you need to — to see the accumulated
change across several releases, say:

```bash
OPENAPI_COMPAT_BASELINE=controller/api/v0.0.12 make -C controller/api compat-check
```

If no published tag is present, or the tag holds no `openapi.yaml`, or either
spec will not parse, the check exits 2 and fails. It never reports "compatible"
from a comparison it could not make.

## What counts as breaking

Anything [oasdiff](https://github.com/oasdiff/oasdiff) reports under its
`breaking` rule set, which is its error and warning levels. Representative
examples, by check id — the id is what a waiver is written in:

| Check id | What it catches |
|---|---|
| `api-removed-without-deprecation` | an endpoint deleted with no deprecation period |
| `response-required-property-removed` | a field a client is entitled to read |
| `response-success-status-removed` | a success status a client branches on |
| `new-required-request-property` | a field every existing caller now omits |
| `request-property-became-required` | the same, for a property that was optional |
| `request-parameter-became-required` | the same, for a parameter |
| `request-property-enum-value-removed` | a value a caller is sending today |
| `api-security-removed` | an authentication requirement dropped from an endpoint |
| `request-parameter-removed` | a parameter a caller is sending today (warning level) |

Additive change is not reported and does not fail: a new endpoint, a new
optional parameter, a new optional response property, a new enum value in a
response, added prose.

Two gaps are worth knowing, because the check will stay quiet about both.
Removing an **optional** response property is `response-optional-property-removed`
at info level, and renaming an `operationId` is `api-operation-id-removed`, also
info — and the second one renames a generated Go method, so it breaks every
compiled consumer while the wire contract is untouched. Neither fails this gate.
Review them by hand.

## Deprecating, then removing

Removal is a three-step sequence, and the gate enforces each step.

**1. Deprecate, and say when it goes.** Mark the operation `deprecated: true`
and give it an `x-sunset` date at least 180 days out:

```yaml
  /cniPlugins:
    get:
      operationId: listCNIPlugins
      deprecated: true
      x-sunset: "2027-06-01"
```

`deprecated: true` without `x-sunset` fails (`api-deprecated-sunset-missing`), and
a sunset date closer than 180 days fails (`api-sunset-date-too-small`). An
open-ended deprecation is not a deprecation.

**2. Wait.** Removing the operation before its sunset date fails
(`api-path-removed-before-sunset`).

**3. Remove it.** Once the date has passed, deleting the operation is no longer
a breaking change and the gate lets it through.

180 days is the notice period for the whole API. It lives in
`hack/openapi-compat-check.sh` as `OPENAPI_DEPRECATION_DAYS`; changing it is a
change to the promise, not a knob to turn for one pull request.

It is one number for the whole API because no operation declares an
`x-stability-level`, and oasdiff treats an operation without one as stable.
Giving part of the surface a shorter notice period means labelling it there
first.

## When you mean to break it

Three options, in the order to prefer them.

**Make the change additive.** A new optional field beside the old one, or a new
endpoint beside the old one, costs no client anything. Most "breaking" changes
are a rename in disguise.

**Deprecate first**, per the sequence above. This is the route for anything that
has to go away eventually.

**Waive it.** For a break that is deliberate and cannot wait — which an alpha API
will have — name the check id in `OPENAPI_COMPAT_WAIVED`, in
[`make/tool-versions.mk`](../../make/tool-versions.mk) beside the govulncheck
waiver, so CI and a laptop read the same value:

```make
OPENAPI_COMPAT_WAIVED       ?= response-required-property-removed
OPENAPI_COMPAT_WAIVER_OWNER ?= A. Maintainer
OPENAPI_COMPAT_WAIVER_REVIEW ?= 2027-03-01
```

Three things about a waiver, all deliberate:

- It is scoped to the check id, not to the endpoint, so it is blunt: waiving
  `response-required-property-removed` waives every removed response property in
  the same pull request. That is uncomfortable on purpose.
- A waiver that no longer matches anything **fails** the gate, which makes
  deleting it the job of whoever fixed the break rather than nobody's job.
- The review date only warns. A date that fails the build gets the mechanism
  deleted the first time it fires on a Monday; the stale-waiver rule above is
  what actually stops a waiver list from growing forever. Name a person as the
  owner, not a team.

Once the change is on `main`, the next `controller/api/vX.Y.Z` tag becomes the
baseline, the finding stops being reported, and the stale-waiver rule tells you
to take the waiver out.

## Running it

```bash
make -C controller/api compat-check
```

It builds oasdiff from `oasdiff.mod`, so there is nothing to install, and it
needs the repository's tags — a tagless or shallow clone fails with the `git
fetch --tags` it wants. Exit 0 is compatible, 1 is a breaking change, 2 is a
verdict it could not reach.
