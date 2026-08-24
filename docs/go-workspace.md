# The Go workspace

This repository holds nine Go modules. `go.work` at the root ties together all
of them, so one `make build` covers the whole tree and one Go toolchain version
applies across it.

`go.work`, `go.work.sum` and the `vendor/` tree are **committed**, and CI fails
when the first two go stale.

## What is in the workspace

| Module | In `go.work` | Why |
|---|---|---|
| `agent` | yes | builds from public dependencies |
| `cli` | yes | builds from public dependencies |
| `controller/api` | yes | builds from public dependencies |
| `controller/hack/capd-reboot-controller` | yes | builds from public dependencies |
| `controller/hack/nico-mock` | yes | builds from public dependencies |
| `controller/test/e2e` | yes | builds from public dependencies |
| `controller` | yes | public dependencies + one vendored INTERNAL module |
| `controller/nico-vms` | yes | builds from public dependencies |
| `os-image` | yes | builds from public dependencies |

### The vendored dependency

`controller` depends on `github.com/NVIDIA/cluster-api-provider-nico`, which is
INTERNAL visibility until it is open-sourced, so public CI runners and outside
contributors cannot fetch it. The committed `vendor/` tree (from
`go work vendor`) carries it — and every other dependency — so **building,
testing and linting need no credentials at all**.

Two things still need access to that repository, until it goes public:

- `make work-sync` — re-vendoring after a dependency change. It runs
  `go work vendor` internally.
- The freshness re-derivation inside `make work-sync` / `make work-check` —
  rebuilding the module graph fetches every `go.mod`, including that one.
  `work-check` detects when the module is unreachable, runs its static checks,
  and skips only the freshness re-derivation with a note.

Workspace members are never vendored: `vendor/modules.txt` maps `agent`,
`controller` and the rest to their live directories, so editing member source
never requires re-vendoring — only dependency changes do.

## Everyday use

```bash
make            # list the targets
make build      # build the released commands into bin/
make clean      # remove bin/
```

`make build` produces `bin/agent`, `bin/capd-reboot-controller`,
`bin/cloud-workflow-engine`, `bin/nico-mock`, `bin/nke-os-image-check`, and
`bin/nkectl`.

## Keeping the workspace up to date

`go.work.sum` records the checksums the workspace needs. It goes stale whenever
a module's requirements change — a dependency bump, a new import, a `go get`.
**Nothing updates it for you**, including Dependabot.

After any change to a `go.mod` or `go.sum`:

```bash
make work-sync
```

That does three things:

1. `go work sync` writes the workspace's selected version of each dependency
   back into every member's `go.mod`.
2. `go mod tidy` runs in each module with the workspace **off**, completing its
   `go.sum`. `go work sync` updates `go.mod` but not `go.sum`, which leaves a
   module unbuildable on its own — and these modules are built on their own, by
   their own Makefiles, by the image builds, and by every per-module CI job.
3. `go mod download all` rewrites `go.work.sum`.

**Commit `go.work`, `go.work.sum` and every `go.mod` and `go.sum` it touched, in
the same commit as the change that moved them.**

### Why `go.mod` changes too

A workspace builds every member against one unified version of each dependency,
whether or not that version is written down. `go work sync` writes it down.
Skipping it does not avoid the upgrade — it only hides it from `go.mod`, from
Dependabot, and from `THIRD_PARTY_NOTICES.md`, so the file says one thing and
the binary contains another.

To check before pushing exactly what CI will check:

```bash
make work-check
```

⚠️ `work-check` is not read-only: it performs the same refresh and then fails if
anything moved, so a failure leaves your tree edited. That edit *is* the fix —
review it and commit it.

## What CI enforces

The **Go Workspace Check** workflow runs `make work-check`, which asserts five
things. Each maps to a failure this repository can produce:

| Assertion | Failure it catches | Fix |
|---|---|---|
| `go.work` and `go.work.sum` are tracked by git | Either file left untracked. `git diff` reports nothing for an untracked path, so the rest of the check would pass in silence. | `make work-sync` and commit both |
| `go.work` lists exactly the expected modules | A module dropped from or added to the workspace by hand | `make work` and commit `go.work` |
| `go.work` carries no `replace` | A workspace-level `replace` repoints a dependency for every module while the module list still looks correct. Module replaces belong in the individual `go.mod`, where `gomoddirectives` audits them. | `make work`, or `git checkout -- go.work` |
| Every `go.mod` in the tree is classified in the `Makefile` | A **new module** added without being listed in `WORK_MODULES`. It would never be built or synced, and nothing else in CI would mention it. | add it to the list in the `Makefile`, then `make work-sync` |
| A full refresh changes nothing | The ordinary stale case: a bump landed in one module, so the others build against a version none of their `go.mod` files admits to. Skipped (with a note) where `cluster-api-provider-nico` is unreachable — see "The vendored dependency". | `make work-sync` and commit everything it changes |

The **Go Build** workflow runs `make build`. Both run on `ubuntu-latest` and
need no NVIDIA credentials, so they also run on fork and Dependabot pull
requests.

## Adding a module

1. Create it with `go mod init`.
2. Add its path to `WORK_MODULES` in the root `Makefile`.
3. If it has commands to build, add it to `CMD_MODULES` too.
4. Run `make work-sync` — it refreshes the module graph **and** re-vendors —
   then commit `go.work`, `go.work.sum`, `vendor/` and the `Makefile`.

Skipping step 2 fails `make work-check` by design, with the module named.

## Troubleshooting

**`error obtaining VCS status: exit status 128`** — you are building inside a
linked `git worktree`, where `.git` is a file rather than a directory, and Go
cannot stamp VCS metadata. It does not happen in a normal clone or in CI. Build
with stamping off:

```bash
GOFLAGS=-buildvcs=false make build
```

**`directory prefix . does not contain modules listed in go.work`** — at the
repository root, the root is not itself a module: build through the `Makefile`,
or name a module path (`go build ./agent/...`).

**`inconsistent vendoring`** — a dependency changed (in any member's `go.mod`)
without the vendor tree being refreshed. Run `make work-sync` and commit the
`vendor/` changes; it needs access to `cluster-api-provider-nico` (see "The
vendored dependency").

> ⚠️ **There is no `make vendor` target, and running it looks like it worked.**
> This document used to say `make vendor` in three places. `vendor/` is a
> directory, so make treats the name as an already-up-to-date file target and
> exits 0 with `make: Nothing to be done for 'vendor'.` — nothing is
> re-vendored, and the next build fails with the same `inconsistent vendoring`
> error you were trying to clear. The target that does the work is
> **`make work-sync`**.
