# Third-Party Notices

nke is a monorepo of independently built and independently released components.
Each one links a different set of third-party Go modules, so the notices are
generated **per component** rather than merged into a single file. Merging them
would attach every module's attribution to every artifact, which misstates what
each artifact actually contains.

This file is the index. It records which components have generated notices,
where those notices are, and which do not yet — and why.

## Covered components

| Component | Notices | Generated from | Enforced by |
|---|---|---|---|
| `agent/` — the node agent | [agent/THIRD_PARTY_NOTICES.md](agent/THIRD_PARTY_NOTICES.md) | the import closure of `./cmd/...` for `linux/amd64` and `linux/arm64` | the `Third-Party Notices` workflow, on every push |
| `controller/` — the Cloud Workflow Engine | [controller/THIRD_PARTY_NOTICES.md](controller/THIRD_PARTY_NOTICES.md) — **156 modules** | the import closure of `./cmd/worker` for `linux/amd64`, read from the committed `vendor/` tree | the `Third-Party Notices` workflow, on every push |
| `cli/` — the `nkectl` CLI | [cli/THIRD_PARTY_NOTICES.md](cli/THIRD_PARTY_NOTICES.md) — **24 modules** | the import closure of `./cmd/nkectl` for `linux/amd64` | the `Third-Party Notices` workflow, on every push |
| `os-image/` — the `nke-os-image-check` binary | [os-image/THIRD_PARTY_NOTICES.md](os-image/THIRD_PARTY_NOTICES.md) — **9 modules** | the import closure of `./cmd/nke-os-image-check` for `linux/amd64` | the `Third-Party Notices` workflow, on every push |

Regenerate with:

```bash
cd agent      && make notices
cd controller && make notices
cd cli        && make notices
cd os-image   && make notices
```

⚠️ **The controller, nkectl and os-image generators need the workspace on** (do
not set `GOWORK=off`), for two different reasons. The controller and nkectl read
the committed `vendor/` tree, which is a workspace-level construct — that is what
lets them run without downloading anything at all.
`os-image` requires `github.com/dsx-ai-factory/nke/agent`, which resolves only through
the workspace's `use` directive; with the workspace off, `go-licenses` fails with
`go.mod file not found` before it collects anything.

`make notices-check` regenerates the file and fails if the result differs from
what is committed. CI runs it on every push, so the file cannot drift away from
the dependency tree without the build going red.

## Not yet covered

**Nothing.** Every binary this repository redistributes now carries generated
attribution, and CI regenerates and compares all four on every push.

`capd-reboot-controller` is built by `make build` but shipped nowhere — no
image, no artifact, no package. If it starts shipping, it needs a row above and
a `notices` target of its own.

⚠️ **This section listed two open gaps until 2026-08-16**, describing a
`controller/THIRD_PARTY_NOTICES.md` that existed but was unverified, and a
missing one for `nkectl`. Both are closed below. **A partial or wrong file
would be worse than none** — it asserts a completeness it does not have — so
the rows stayed open until each closure could be walked end to end *and* a
check enforced it.

### How the controller row was closed

It was open for a long time with the reason *"`controller/go.mod` requires two
modules that are not public"*. That stopped being true when
[#76](https://github.com/dsx-ai-factory/nke/pull/76) removed them, leaving one
module that was internal at the time, `cluster-api-provider-nico`, and is public
now. Two mechanical problems kept it open after that, and both are worth writing
down because neither is obvious:

**`go-licenses` requires its rootDir to contain the vendor tree.** This
repository vendors once at the workspace root, above every module, so running
the generator from `controller/` failed outright with `rootDir /…/controller
should contain dir /…/vendor/k8s.io/…`. The generator now walks up to whichever
directory owns `vendor/` and names packages relative to it, so one
implementation serves every module.

**`go-licenses` must be built with the same toolchain that `go list` runs
under.** On a mismatch it reports every standard-library package as *"does not
have module info"* and produces nothing — which reads like a broken generator
rather than a version skew. `controller/Makefile` pins it.

⚠️ **A file existing is not the same as a file being right.** A
`controller/THIRD_PARTY_NOTICES.md` was imported from GitLab before this, and it
attributed modules that appear in no `go.sum` here and are not vendored, while
missing others. **A wrong notices file is worse than a missing one, because it
looks compliant.** The check in CI is what makes the difference.

## Scope of a generated file

Each generated file lists every Go module reachable from that component's
released commands, on every platform it ships for, with the full text of each
license. It excludes:

- The Go standard library.
- Packages belonging to the module itself.
- Test-only and build-time dependencies that the released commands do not
  reach, because they are not distributed.

## See also

- [NOTICE](NOTICE) — the attribution notice for this repository, as required by
  the Apache License, Version 2.0
- [LICENSE](LICENSE) — the license this repository is distributed under
