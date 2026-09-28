# The Go workspace

This repository holds eleven Go modules. The root `go.work` ties them together,
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
