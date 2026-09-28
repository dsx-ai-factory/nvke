# Development

This page is the working reference for building, testing and checking your work
before you open a pull request. `CONTRIBUTING.md` covers the process around a
change: issues, branches, commit format and sign-off.

`make help` at the repository root lists every target, grouped, and each module
has its own. Neither list is repeated here, because a copy of a target list goes
stale and the generated one cannot.

```bash
make help                 # the root targets
make -C controller help   # any module's targets
make show-versions        # every tool version CI pins
```

## Getting the tools

You need Go, and on macOS you need GNU Make 4, because the version Apple ships is
3.81 and the root `Makefile` refuses to run under it. `brew install make` gives
you `gmake`.

Everything else is optional until a target asks for it, and a target that needs a
missing tool says so and tells you how to get it.

The repository also ships a [Flox](https://flox.dev/) environment at the root that
pins the whole toolchain, including GNU Make, at the versions CI uses:

```bash
brew install flox        # or see https://flox.dev/docs/install-flox/install
flox activate            # works from the root or from any module
```

**Flox is offered, not required.** Every target runs from whatever is on your
`PATH`. Use Flox if you want the pinned set without thinking about it, and skip it
if you would rather manage the tools yourself.

## The local loop

```bash
make build                # compile every module, binaries into bin/
make test                 # every module's own test suite
make -C controller lint   # one module, when that is all you changed
```

Each module answers the same four verbs, so `make -C <module> {build,test,lint,fmt}`
works for `agent`, `cli`, `controller`, `controller/nico-sim`,
and `os-image`.

For the controller specifically, `make -C controller generate` regenerates the
OpenAPI client, server and models from `api/openapi.yaml`. **It is the one most
easily forgotten.** The generated files are committed, so changing the spec
without regenerating leaves them stale and CI will catch it.

## Before opening a pull request

```bash
make preflight
```

That works out which modules you changed, then lints and tests those, and adds
the workspace and licence-header checks when your change touched what they cover.
It prints what it is about to run, and what it is not covering, before it runs
anything.

Scope it yourself when you want to:

```bash
make preflight MODULES=cli
make preflight PREFLIGHT_BASE=upstream/main
```

The full set, if you would rather run everything:

```bash
make build test           # compile and test every module
make work-check           # workspace and module manifests agree
make license-header-check # every source file carries the Apache-2.0 header
```

Run `make work-sync` and commit everything it changes whenever you touch a
`go.mod`. Dependencies are public and downloaded into the Go module cache as
needed; no NVIDIA-internal repository access is required.

## What you cannot reproduce locally

The merge gate runs all of its members on every pull request whatever you
touched, because a called workflow ignores its own `paths:` filter. See
[the workflow guide](../.github/workflows/README.md). So a local run narrows what
can newly fail, not what runs.

These are the checks where a local answer will differ from CI, and why:

| check | why it differs |
|---|---|
| the CAPD end-to-end suite | `make test-e2e-backend-ci` refuses to start unless the host is Linux on amd64. On a Mac it stops before it reaches Docker. |
| the gofail suite | `make -C controller e2e-gofail` needs PostgreSQL, an envtest API server and a Temporal dev server. |
| CodeQL, Scorecard, dependency review | these need GitHub-side infrastructure and have no local form. |
| version strings | a CI checkout is shallow and carries no tags, so `git describe` there returns a commit where your full clone returns a tag. Both are correct. |

## Documentation

`docs/` holds the longer-form guides. [`go-workspace.md`](go-workspace.md) is the
one to read first if a workspace or module check fails.
