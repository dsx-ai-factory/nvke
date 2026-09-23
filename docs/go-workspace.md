# The Go workspace

This repository holds twelve Go modules. The root `go.work` ties them together,
so one `make build` covers the whole tree and one Go toolchain version applies.
`go.work`, `go.work.sum`, and the module manifests are committed; the generated
`vendor/` tree is not. All dependencies, including
`github.com/dsx-ai-factory/cluster-api-provider-nico`, are public. Go downloads
them into its module cache on first use, so a cold build needs network access
but no NVIDIA credentials.

| Module | In `go.work` | Role |
|---|---|---|
| `agent` | yes | node agent |
| `cli` | yes | command-line tools |
| `cluster-api-provider-provisioned` | yes | provisioned-host provider |
| `controller` | yes | cloud workflow engine |
| `controller/api` | yes | generated API module |
| `controller/deploy/charts` | yes | embedded Helm charts |
| `controller/hack/capd-reboot-controller` | yes | CAPD test helper |
| `controller/nico-sim` | yes | local NICo simulator |
| `controller/test/e2e` | yes | end-to-end tests |
| `management-engine` | yes | management services |
| `os-image` | yes | node-image tooling |
| `release` | yes | release qualification tooling |

## Everyday use

```bash
make                # list targets
make build          # build released commands into bin/
make work-check     # verify workspace and module manifests
```

`make build` produces `bin/agent`, `bin/capd-reboot-controller`,
`bin/controller`, `bin/nke-os-image-check`, `bin/nkectl`, and `bin/updater`.

## Updating dependencies

After changing a module's `go.mod` or `go.sum`, run `make work-sync`. It runs
`go work sync`, tidies every member with workspace mode disabled, and rebuilds
`go.work.sum`. Commit `go.work`, `go.work.sum`, and every changed `go.mod` and
`go.sum` together. Workspace mode selects one version of each dependency; sync
records those selected versions in each member's manifest so independent module
builds agree with workspace builds.

`make work-check` performs the same refresh and fails if tracked manifests move.
It can edit those files on failure; review the diff and run `make work-sync` to
apply the intended update. It requires a reachable public Go module proxy on a
cold cache, and CI does not silently skip the freshness check.

The check also verifies that `go.work` and `go.work.sum` are tracked,
`go.work` lists exactly the modules in the root `Makefile`, no workspace-level
`replace` is present, and every `go.mod` belongs to a listed module.

## The Go version

**`go.work` owns it.** Its `go` directive is the one authoritative statement of
which Go toolchain this repository builds and tests with, and everything else
either derives from it or is checked against it.

| Consumer | How it gets the version |
|---|---|
| CI | `go-version-file: go.work` on `actions/setup-go` |
| The root and `controller/` Makefiles | `GOTOOLCHAIN` computed from `go.work` |
| The `govulncheck` gate | `GOVULNCHECK_GO_VERSION` in `make/tool-versions.mk`, derived |
| Every member's `go.mod` `go` directive | Hand-kept, checked, and rewritable with `make go-version-sync` |
| The `golang` base images in the Dockerfiles | Hand-kept, checked |
| The Flox manifest | Hand-kept in `.flox/env/manifest.toml`, checked — a static manifest cannot derive |

The supported version is **exactly one patch release**, not a range. Every
member module's `go` directive equals `go.work`'s, so a module never claims to
build with a toolchain this repository does not test. That is stricter than Go
requires — a directive is a minimum, and a lower one would still compile — but
the three values that had accumulated here — 1.26.0, 1.26.5 and 1.26.6, under
one workspace — were not a considered compatibility statement, they were drift
nobody had a reason to notice.

`make go-version-check` asserts the whole table. It reads files only, needs no
toolchain, and runs in the **Go Build** workflow, so it covers fork and
Dependabot pull requests.

It takes the module list from the `Makefile`'s `WORK_MODULES` and
`PRIVATE_MODULES` rather than rediscovering it, so there is no second list to
drift — which is the failure the check exists to prevent. A tracked `go.mod`
on neither list is an error: a new module cannot be added and go unchecked.
`PRIVATE_MODULES` are listed but left alone, since `go.work` makes no claim
about a module outside the workspace. Run it through `make`, which supplies
those lists; the script rejects a bare invocation rather than guessing.

### Moving it

1. Edit the `go` directive in `go.work`.
2. `make go-version-sync` — rewrites every member's `go.mod` directive.
3. Update the `golang` tags and digests in the Dockerfiles, and the `go` entry
   in `.flox/env/manifest.toml`. Neither can derive, so `go-version-sync`
   reports them and exits non-zero rather than pretending it finished.
4. `make go-version-check` to confirm, then `make work-sync`.

A patch bump is that and nothing else. A **minor** bump is additionally a
decision: it changes the language version every member compiles against and the
minimum Go a consumer of `controller/api` needs, so it is made deliberately
rather than by merging a Dependabot pull request. `.github/dependabot.yml`
ignores minor and major `golang` image updates for this reason.

## Adding a module

1. Create its `go.mod` and add its path to `WORK_MODULES` in the root `Makefile`.
2. Add it to `CMD_MODULES` if it has a command built by the root `build` target.
3. Run `make work-sync` and commit the resulting workspace and module files.

## Troubleshooting

If Go reports a missing module or checksum, verify that the public module proxy
is reachable and run `make work-sync` if a dependency changed. If a linked git
worktree cannot stamp VCS metadata, use `GOFLAGS=-buildvcs=false make build`.
The nico-sim remote test host also needs access to the public module proxy.
At the repository root, the root itself is not a module: build through the
`Makefile`, or name a module path such as `go build ./agent/...`.
