# release/

> **Plan only.** This directory defines the shape of NKE release
> qualification, and `qualify plan` resolves a real one from the repository's
> tags. Nothing runs it: there is no `run`, no capacity provider and no
> published record, so a green CI run means a plan was well formed, not that
> anything was qualified. See [Status](#status).

Release tooling: the orchestration that takes a candidate set of already-built
artifacts, qualifies it, and — on success — publishes the immutable record of
what was qualified together.

## What this is not

`build/` produces the artifacts for one component. `release/` never builds
anything. It consumes artifacts that already exist, addressed by digest, and it
is an error for anything under this directory to compile source into a shipped
artifact. That separation is what makes the qualification meaningful: a release
promotes the exact bytes that were tested, and tooling that can rebuild is
tooling that can substitute.

"Addressed by digest" is not as simple as it sounds, and the record's schema
reflects that. `oras push` writes `org.opencontainers.image.created` into the
manifest with wall-clock push time, so for anything published by a fresh push
rather than copied — the agent, the CLI, the chart — the manifest digest
identifies a *publication event*, not bytes. Republish identical content and
the digest changes. A container image is different: its manifest digest is its
content identity, and `crane copy` preserves it. So the schema carries two
fields. `digest` is what you pull by and is unambiguous within one registry;
`contentDigest` is the layer, exists only where there is a single one, and is
the only thing that still means anything once an artifact has been mirrored.

Per-component publication stays where it is. `agent/v*`, `os-image/v*`,
`controller/v*` and `cli/v*` tags continue to drive their own workflows exactly
as [`RELEASE.md`](../RELEASE.md) describes. `release/` orchestrates *over* those
publications; it does not replace them.

## Why there is a go.mod here

An earlier revision of this file argued against one. The argument was that
`.github/workflows/release.yml` reads the directory prefix off a release tag
and checks it against `go.work`, so a module here would make `release/vX.Y.Z` a
valid release — a fourth independently versioned thing in a repository whose
versioning model has three categories. That does not hold up.

The tag trigger is `*/v[0-9]*`, and a glob does not cross a slash, so only
top-level modules are reachable: `agent`, `cli`, `controller`, `os-image`,
`management-engine` and `cluster-api-provider-provisioned`. Two of those six
are already not product categories, and `management-engine` has carried zero
tags without anyone concluding it ships. Being taggable is not being a product.
Tagging is a deliberate act, and the three-category model lives in the
versioning proposal rather than in the set of directories a workflow can
resolve.

The alternative was real but self-defeating. `make work-check` accounts for
every `go.mod` against two lists, and the second, `PRIVATE_MODULES`, exists for
modules held out of the workspace. A module on it is never built or synced,
which would forfeit exactly the `lint`, `test`, `govulncheck` and license
coverage that made Go worth choosing over a shell and Python pair.

The module costs nothing at the dependency boundary: `cobra`, `sigs.k8s.io/yaml`
and `golang.org/x/mod` are already in the committed `vendor/` tree, so adding it
left that tree byte-for-byte unchanged.

## Layout

| Path | Contents |
|---|---|
| [`qualification.yaml`](qualification.yaml) | The release-wide policy: components in scope, the streams they run against, and the named scopes over those streams |
| [`cmd/qualify/`](cmd/qualify/) | The binary. Five verbs are designed; `plan` is the one that exists |
| [`internal/plan/`](internal/plan/) | Turning a trigger into a run list: dependency resolution, scope expansion, and what an earlier run already settled |
| [`internal/policy/`](internal/policy/) | Reading and validating the `qualification.yaml` files |
A component says how it is qualified in its own file, next to the code:
[`controller/qualification.yaml`](../controller/qualification.yaml),
[`cli/qualification.yaml`](../cli/qualification.yaml),
[`os-image/qualification.yaml`](../os-image/qualification.yaml). `agent/` has
none, because the agent is built from the same checkout as the OS image and
baked into it, so it is never deployed alone and a run has nothing to select.

## Streams, scopes and combinations

Three words carry the whole matrix, and they are worth keeping straight.

A **stream** is one configuration, named by the capabilities it provides —
`{provider: nico, arch: amd64, kube: "1.34"}` — and never by the infrastructure
that satisfies them. Which runner, site or subnet a capability resolves to is
an environment knob belonging to the e2e suite's own config. That boundary is
what keeps the suite runnable by hand and by a second organisation, and keeps
this tool from becoming a config translator.

A **scope** is a named selection over the streams, matched on capabilities.
`full` carries no selector so it matches everything; `active` narrows to the
current minor. The set of supported Kubernetes minors is not declared anywhere
— it is whichever minors have streams — so adding one means adding streams and
nothing else.

A **combination** is one resolved set of dependency versions. A candidate is
qualified against one or more combinations, and each combination against every
stream in the scope. Today each dependency carries the single selector
`latest`, so there is exactly one combination: a controller bump holds the node
image and CLI at their last-qualified versions, which is what makes a failure
name one category instead of three that all moved at once.

`latest` takes the first of three tiers that has a candidate: the highest
release, then the highest *qualified* candidate, then the highest candidate at
all. The third tier is what lets the framework start — on day one nothing has
been qualified, so the tiers above it are empty — and the result is visibly
weaker rather than silently so, because the record says what each held version
had been proven against.

```console
$ go run ./release/cmd/qualify plan --component controller --scope full
{
  "component": "controller",
  "version": "v0.1.1-rc.8",
  "scope": "full",
  "combinations": [
    {
      "id": "os-image-v0.1.0-rc.4_cli-v0.1.0-rc.7",
      "dependencies": [
        { "name": "os-image", "version": "v0.1.0-rc.4" },
        { "name": "cli",      "version": "v0.1.0-rc.7" }
      ],
      "streams": [
        { "name": "nico-amd64-kube-1.34" },
        { "name": "nico-arm64-kube-1.34" }
      ]
    }
  ]
}
```

A stream a previous run settled stays in the file carrying `skip`, rather than
being dropped. `passed` and `failed` are both skipped, because rerunning the
same candidate proves nothing either way; `error` and `unschedulable` are not,
because the answer is still unknown. Keeping them means a run covering four of
six streams says so instead of looking like a smaller matrix.

## The model

An NKE release is three independently versioned categories — **Controller**,
**Node** (agent and OS image, which ship together), and **CLI** — plus an
immutable **compatibility record** identifying the exact combination that was
tested and qualified together.

The categories already release on their own cadence and always have; the tag
history shows coordinated releases carrying three different version numbers.
What has been missing is a published answer to *"what was tested together?"*.
The compatibility record is that answer, and producing it is this directory's
reason to exist.

```
  candidate combination            qualification              published
  (digests, per category)    ->    (layered evidence)   ->    compatibility
                                                              record
```

## Qualification layers

Four layers, each doing only what its substrate can afford. Only layers 2 and 3
gate a release; the others exist to make those two cheap and honest.

| Layer | What runs | Substrate | Gates |
|---|---|---|---|
| 1 — Continuous | `controller/test/e2e` emulated lanes | Docker, nico-sim, KVM | Code into `main` |
| 2 — Pre-flight | The same suite, digest-pinned to the candidate | Docker | Candidate, before hardware |
| 3 — Qualification | Lifecycle and conformance suites | Real infrastructure | RC into GA |
| 4 — Governance | Upstream intent drift check | None | Coverage decisions stay explicit |

The rule that keeps the layers from growing into each other: **a check belongs
where its expectation comes from.** "The controller should behave this way" is a
property of the code and belongs in layer 1. "This release pins version X" is a
property of the release and belongs in layer 3, sourced from the SBOM rather
than from a constant in test code.

Layer 2 is the interesting one, and it is the only layer that does not exist
yet. It runs the existing emulated suite against the candidate's *actual*
artifacts, which today it cannot do — the lane rebuilds the controller from the
working tree and reads the Kubernetes version from a constant. Closing that is
a compatibility gate on the artifact set for the cost of an ordinary CI run,
and it catches the failure most likely to bite a release: components that were
never run together.

## Status

`plan` is real and the policy files are gate-covered. **No qualification lane
is wired in** — a green run means a plan was well formed, not that anything was
qualified. In dependency order:

- [x] Stand the tool up as a Go module in the workspace, so it is linted,
      tested and vulnerability-scanned like everything else
- [x] Write the qualification policy: components, streams and scopes here, and
      dependencies in each component's own file
- [x] Implement `qualify plan` — dependency resolution over the three `latest`
      tiers, scope expansion, and skip from prior outcomes
- [x] Validate the policy files in the merge gate. `TestRepositoryPolicyFilesAreValid`
      loads every one of them, so a selector naming a capability no stream
      declares fails a pull request instead of silently selecting nothing.
- [ ] Publish a record. The SADD has `publish` write one per stream and
      `aggregate` fold them into a central artifact in a registry. Whatever
      shape that takes has to keep the `digest` versus `contentDigest` split
      described above: a manifest digest identifies a publication event, so it
      is not stable across a copy, and the layer digest is what survives one.
- [ ] Implement `run`: the capacity provider, the test client, and the CTRF
      artifacts a run leaves behind
- [ ] Teach the pre-flight lane to accept pre-built artifacts (upstream of this
      directory: the build guard, an agent bundle checksum, and a configurable
      Kubernetes version)
- [ ] Add the streams that need real hardware. Today's two are container-backed,
      so `qualification.yaml` declares no `gpu` capability at all rather than
      claiming one no lane can back.
- [ ] Implement `aggregate` and `generate`
