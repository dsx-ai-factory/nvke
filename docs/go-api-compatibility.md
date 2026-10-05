# Go API compatibility

Which Go packages in this repository other code may import, what is promised
about them, and what enforces the promise.

## The decision

`controller/api` is a supported contract. Nothing else in this repository is.

That was not written down anywhere before, and the ambiguity was the problem
rather than the answer: an importer had no way to tell which packages were safe
to build against, and this repository had no check that would notice when one of
them broke.

### Why `controller/api` is supported

It was already being released as if it were, in three ways:

- It is a Go module of its own, with its own `go.mod`, which exists so that a
  consumer can depend on the API types without pulling in the engine. Its
  [README](../controller/api/README.md) says exactly that.
- [`.github/workflows/controller-api-tag.yml`](../.github/workflows/controller-api-tag.yml)
  publishes a `controller/api/vX.Y.Z` tag for every change to it. You do not
  build automatic version tagging for a package nobody is meant to pin.
- `controller`, `cli` and `controller/test/e2e` already import it, and the
  generated client is the supported way to call the engine's HTTP API.

### Why nothing else is

These were the other candidates, and each is internal:

| Package | Why it is not a supported contract |
|---|---|
| `agent/pkg/agent/proto` | Generated gRPC stubs for the engine-to-node-agent link. Both ends ship together; the wire format is versioned by the release, not by an import path |
| `agent/pkg/auth`, `agent/pkg/nkeauth` | Authentication helpers for that same internal link |
| `cluster-api-provider-provisioned/api/v1alpha1` | A Cluster API provider's types. `v1alpha1` is the promise: the Kubernetes API conventions govern it, and the CRD is the interface operators use, not the Go package |
| `cluster-api-provider-provisioned/controllers` | Reconciler implementation. Nothing outside the provider should construct one |
| Everything else | Module-internal. Not released separately and not tagged |

None of these is tagged on its own, none is documented as importable, and no
consumer outside this repository is known to import any of them.

## The promise on `controller/api`

The module is pre-1.0, and semantic versioning says less at `v0` than people
remember, so the promise is stated rather than implied:

- **A patch bump never removes or retypes an exported symbol.** Adding is fine;
  taking away or changing the shape of what is there is not.
- **A breaking change gets a minor bump**, which at `v0` is the increment that
  signals it. It is never shipped under a patch tag.
- **The generated code is the contract, not the generator.** `openapi.yaml` is
  the source of truth for what gets generated, but what is promised is the Go
  API that results from it, because that is what an importer compiles against.
- **Pre-release tags promise nothing.** Only `vX.Y.Z` is a baseline.

Nothing here is a support commitment to a party outside NVIDIA. It is a
statement about what a version number means, so that a consumer can pin one and
reason about an upgrade.

## What enforces it

[`hack/api-compat.sh`](../hack/api-compat.sh), through `make api-compat`, and in
CI through `.github/workflows/api-compat-check.yml`, which is a required member
of the merge gate.

It checks out the newest `controller/api/vX.Y.Z` tag into a temporary git
worktree, runs `apidiff` over the module as it is at that tag and as it is in
the working tree, and fails when the report lists an incompatible change.

The baseline is the newest published tag because that is the version the next
patch bump claims compatibility with. `controller-api-tag.yml` increments the
patch and pushes without looking at what changed, so "the patch bump that is
about to happen" is the thing worth gating.

Three ways a compatibility check passes without having checked anything, and
what this one does instead:

| Failure mode | What happens here |
|---|---|
| No baseline — a module never tagged, or a clone fetched without tags | Fails, naming both causes. It does not treat "nothing to compare" as "nothing wrong" |
| The tool errored | Fails. `apidiff` exits 0 whether or not it found a break, so the verdict is read from the report and a non-zero status is treated as the tool failing, never as a clean result |
| The package did not load, so its API came back empty | Each side is built first, and empty export data is rejected. Two empty APIs compare equal |

`gorelease` would be the obvious tool and cannot be used: it resolves the base
version through the Go module proxy, this repository is private, and the proxy
answers 404 for every version of it. Reading the baseline out of the local clone
needs no credentials, which is also what keeps the check working on a fork pull
request, where no secret is available.

## Making a breaking change on purpose

1. Make the change and let the gate fail. The failure lists every incompatible
   symbol, which is the review material.
2. Say in the pull request who imports the symbol and what they have to do.
3. After it merges, `controller-api-tag.yml` will publish a patch tag, which is
   the wrong increment. Publish the minor tag by hand instead, before anything
   else lands under `controller/api`:

   ```bash
   git tag controller/api/v0.1.0
   git push upstream controller/api/v0.1.0
   ```

   The tagger reads the newest tag and increments from there, so the next
   automatic tag continues from the minor you published. See
   [RELEASE.md](../RELEASE.md) for how component tags work generally.
4. To let the merge land before step 3, set `API_COMPAT_WAIVED` in
   [`make/tool-versions.mk`](../make/tool-versions.mk) to
   `controller/api@<the baseline the gate named>`, with
   `API_COMPAT_WAIVER_OWNER` naming a person.

The waiver is bounded by the baseline it names, not by a date. It stops matching
the moment the next tag is published, so it cannot quietly outlive the release
it was reviewed for, and the gate fails if it is left behind once it no longer
applies to anything.

## What this does not cover

- **The HTTP wire contract.** `apidiff` compares Go types. A change that keeps
  the generated Go API identical and still breaks an HTTP caller — a response
  field that stops being populated, a status code that changes — is invisible
  to it. A schema diff over `openapi.yaml` is the check for that, and is
  tracked separately.
- **Behaviour.** A method that keeps its signature and changes what it does
  passes.
- **The other modules.** Nothing above them is checked, because nothing is
  promised about them. Promising something about one means adding it to
  `SUPPORTED_MODULES` in `hack/api-compat.sh` and to the table in this document,
  in the same change.

## Revisiting this

The decision rests on who imports what, and that changes. Reopen it when a
consumer outside this repository starts importing something not listed as
supported, when a package here gets its own release tag, or when
`controller/api` reaches `v1`, at which point the promise stops being a local
convention and becomes the one semantic versioning already defines.
