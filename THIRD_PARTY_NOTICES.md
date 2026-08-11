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

Regenerate with:

```bash
cd agent && make notices
```

`make notices-check` regenerates the file and fails if the result differs from
what is committed. CI runs it on every push, so the file cannot drift away from
the dependency tree without the build going red.

## Not yet covered

| Component | Why | Tracked by |
|---|---|---|
| `controller/` — the Cloud Workflow Engine | `controller/go.mod` requires two modules that are not public. `go-licenses` has to resolve every module in the graph to walk the import closure, so it cannot produce a complete or accurate list until those dependencies are removed. | The work to make `controller/` build from public dependencies |

**A partial file would be worse than none here.** A notices file that silently
omits the dependencies reached through the private modules would claim
completeness it does not have, and anyone relying on it for license compliance
would be relying on the wrong list. The row above stays open until the closure
can be walked end to end.

When `controller/` builds from public dependencies, cover it by copying
`agent/hack/generate-notices.sh`, `agent/Makefile`'s `notices` and
`notices-check` targets, and adding the module to
`.github/workflows/notices-check.yml`. The generator is repository-agnostic and
is configured entirely through the `PACKAGES`, `PLATFORMS`, `OUTPUT` and
`GO_LICENSES` environment variables.

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
