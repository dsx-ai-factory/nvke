# Agent guide — NVIDIA Kubernetes Engine (nke)

Guidance for AI coding agents (Claude Code, Codex, Copilot Workspace, Cursor)
working in this repository. Humans should read [CONTRIBUTING.md](CONTRIBUTING.md);
this file adds the constraints an agent needs before it starts editing.

`AGENTS.md` is canonical. The `CLAUDE.md` next to each one is a one-line
`@AGENTS.md` import shim for Claude Code, which does not read `AGENTS.md`
directly — keep the shims content-free so no harness sees different
instructions.

## Always read

- [README.md](README.md) — what NKE is and how the pieces fit together
- [CONTRIBUTING.md](CONTRIBUTING.md) — branch naming, commit format, review flow
- **The `AGENTS.md` of the module you are editing.** This is a monorepo and each
  module has its own toolchain:
  - [controller/AGENTS.md](controller/AGENTS.md) — Flox, code generation, database, Temporal
  - [agent/AGENTS.md](agent/AGENTS.md) — proto generation, packaging, gRPC conventions
  - [cli/AGENTS.md](cli/AGENTS.md) — nkectl, verification, doc-comment conventions
  - [os-image/AGENTS.md](os-image/AGENTS.md) — reference node OS image spec, Packer, connectivity checker
  - [controller/nico-sim/AGENTS.md](controller/nico-sim/AGENTS.md) — local NICo simulator with VM and container backends

## Read if needed

- [docs/architecture.md](docs/architecture.md) — modules, data flows, deployment model
- [docs/getting-started.md](docs/getting-started.md) — local development setup
- [SECURITY.md](SECURITY.md) · [GOVERNANCE.md](GOVERNANCE.md) · [RELEASE.md](RELEASE.md) · [MAINTAINERS.md](MAINTAINERS.md)

## What this project is

NKE is a management plane for GPU Kubernetes on bare metal. A seed cluster
stands up a highly available control plane, which then creates GPU tenant
clusters on demand. The main components are:

- **`controller/`** — the Cloud Workflow Engine (CWE). Orchestrates cluster
  lifecycle through Temporal workflows, backed by PostgreSQL, with an OpenAPI
  REST surface.
- **`agent/`** — the node agent. A gRPC server running as a systemd service on
  every node. Drives `kubeadm`, installs packages and bundles, and authenticates
  through SPIFFE/SPIRE mTLS.
- **`cluster-api-provider-provisioned/`** — the Cluster API infrastructure
  provider for hosts that NKE provisions before CAPI bootstraps them.

## Read this before you try to build

**Every module builds outside the NVIDIA network.** From a clean checkout, with
no credentials and no network:

```bash
GOPROXY=off make build
```

compiles all eleven modules and links six binaries. `GOPROXY=off` is not a
suggestion to copy — it is how you can tell nothing is being fetched.

This changed in #76, which replaced the internal carbide SDK
(`nvmetal/cloud-api-sdk-go`, reached through the Forge provider) with the public
`github.com/NVIDIA/ncx-infra-controller-rest`. Before that, `controller/` needed
only `agent/` built from public dependencies.

**One internal dependency remains**, and it does not block you:
`github.com/dsx-ai-factory/cluster-api-provider-nico` is an internal GitHub repository,
and it is carried in the committed `vendor/` tree. Building, testing and linting
all read the vendor tree, so they work without access to it. **Only `go work
vendor` resolves it for real** — so re-vendoring after a dependency bump needs
that access, and `make work-check` probes for it and says so when it is
skipping.

If a task asks you to build or test `controller/` and the modules do not
resolve, **say so and stop**. Do not vendor the private modules, do not add a
`replace` pointing at a local checkout, and do not stub the SDK to make the
build pass. Removing those dependencies is tracked work with its own design.

## Directory layout

| Path | Contents | Edit? |
|---|---|---|
| `agent/cmd/`, `agent/internal/`, `agent/pkg/` | Node agent implementation | Yes |
| `cli/` | nkectl CLI implementation and e2e fixtures | Yes |
| `cluster-api-provider-provisioned/` | Provisioned Cluster API infrastructure provider | Yes |
| `controller/internal/` | CWE server, controllers, Temporal workflows and activities, database layer | Yes |
| `controller/api/openapi.yaml` | OpenAPI 3.0 spec — **the source of truth for the REST API** | Yes |
| `controller/profiles/` | OS and config profiles | Ask first — being moved out of this repo |
| `build/` | Build scripts (`agent.sh`) | Yes |
| `docs/` | Repository-level documentation | Yes |
| `os-image/` | Reference node OS image spec, Packer recipe, `nke-os-image-check` | Yes |
| `controller/api/*.gen.go` | oapi-codegen output | **No — regenerate** |
| `cluster-api-provider-provisioned/api/v1alpha1/zz_generated.deepcopy.go` | controller-gen output | **No — regenerate** |
| `cluster-api-provider-provisioned/config/crd/bases/*.yaml` | controller-gen output | **No — regenerate** |
| `cluster-api-provider-provisioned/config/rbac/role.yaml` | controller-gen output | **No — regenerate** |
| `agent/pkg/agent/proto/*.pb.go` | buf/protoc output | **No — regenerate** |
| `agent/THIRD_PARTY_NOTICES.md` | go-licenses output | **No — regenerate** |
| `controller/deploy/charts/` | Helm charts | Yes, but bump the chart version |

## What agents may do

- Implement or fix logic under `agent/internal/`, `agent/pkg/`,
  `controller/internal/`, and `os-image/`.
- Change the REST API by editing `controller/api/openapi.yaml`, **then running
  `make generate`** so the generated Go matches. The generator version is
  pinned in `api/oapi-codegen.mod`; never invoke `oapi-codegen` directly.
- Change the gRPC API by editing `agent/pkg/agent/proto/node_agent.proto`,
  **then running `make protos`** and committing the regenerated `*.pb.go` in the
  same commit.
- Change the provisioned provider API by editing its Go types and markers,
  **then running `make generate manifests`** in
  `cluster-api-provider-provisioned/`.
- Add unit tests, table-driven cases, and mock implementations of the existing
  interfaces.
- Improve documentation under `docs/`, the module `docs/` directories, and the
  root markdown files.
- Refactor within a package when the change is covered by tests.
- Update dependencies and run `go mod tidy`, committing `go.mod` and `go.sum`
  together.

## What agents must not do

- **Do not commit credentials, secrets, API keys, tokens, kubeconfigs,
  certificates, or `.env` files.** Nothing resembling a credential belongs in
  this repository, in test fixtures, or in example manifests or profiles. This
  repository is being prepared to go public and its full history goes with it.
  Use placeholder values such as `REDACTED` or `example-token`.
  `DB_CONNECTION_STRING` and the SPIFFE socket path are supplied by the
  environment at runtime — never inline a real value. If you find a real secret
  already committed, **stop and report it** rather than pushing a fix that
  contains it.
- **Do not edit generated output by hand.** `controller/api/*.gen.go`, the
  provisioned provider's deepcopy, CRD, and controller role files,
  `agent/pkg/agent/proto/*.pb.go`, and `agent/THIRD_PARTY_NOTICES.md` are
  produced by their module generation targets. Change the source types,
  markers, OpenAPI spec, `.proto` file, or `go.mod`, and regenerate.
- **Do not rewrite license headers in bulk.** Every source file carries
  `SPDX-License-Identifier: Apache-2.0`, matching the repository's LICENSE.
  Match the header of the file you are editing; `make license` adds it for new
  files. A pull request that relicenses files as a side effect will be
  rejected.
- **Do not weaken linters to make a change pass.** The `.golangci.yml` files are
  the project's standard. Fix the code instead. Do not add `//nolint` without a
  reason on the same line.
- **Do not add `TODO:` comments in `controller/`** — they fail the linter. Use
  `FIXME:` with a tracking link, which is the established convention. A
  tracking link means a GitHub issue on this repository, not an internal
  tracker.
- **Do not commit build output** — `bin/`, `release/`, `agent/build/deb_*`, or
  `controller/.local/`. All of it is already in `.gitignore`; keep it that way.
- **Do not change the release or CI workflows** in `.github/workflows/` as part
  of an unrelated change. They gate every merge.
- Do not add a new dependency without saying why in the pull request.

## Coding conventions

- **Errors:** wrap with `fmt.Errorf` and `%w`. The standard library only —
  `github.com/pkg/errors` is not used here.
- **Logging:** `log/slog`, structured. Not `fmt.Printf`, not `log`.
- **gRPC:** return `status.Errorf` with an appropriate code from RPC methods.
- **Testability:** the agent takes its side effects through interfaces —
  `CommandRunner`, `FileWriter`, `NodeStatusPersister`. Use them so tests can
  substitute mocks.
- **Tests:** table-driven with descriptive case names, colocated as `*_test.go`,
  using `github.com/stretchr/testify`.
- **Imports** are grouped stdlib, third-party, then this module, separated by
  blank lines.
- **Line wrapping:** prefer single-line Go function declarations, using 150
  characters as a guideline rather than a hard limit. Keep one or two parameters
  inline unless the resulting line is exceptionally long; use multiline
  parameter blocks when they materially improve readability. When wrapping, put
  every parameter on its own line.
- Exported types and functions carry doc comments starting with the identifier
  name.

### Preferred and deprecated patterns

```go
// Preferred: structured logging with slog.
slog.Error("kubeadm init failed", "node", name, "err", err)

// Deprecated: unstructured output. It is unsearchable in aggregated logs.
fmt.Printf("kubeadm init failed on %s: %v\n", name, err)
```

```go
// Preferred: wrap with the standard library so errors.Is and errors.As work.
if err := s.Runner.Run(ctx, "kubeadm", args...); err != nil {
    return fmt.Errorf("running kubeadm %s: %w", args[0], err)
}

// Deprecated: github.com/pkg/errors is not a dependency of this repository.
return errors.Wrap(err, "running kubeadm")
```

```go
// Preferred: take side effects through the interface, so tests can mock them.
type NodeAgentServer struct {
    Runner utils.CommandRunner
}
func (s *NodeAgentServer) reset(ctx context.Context) error {
    return s.Runner.Run(ctx, "kubeadm", "reset", "-f")
}

// Deprecated: calling exec directly makes the method untestable without a
// real kubeadm on the machine.
func (s *NodeAgentServer) reset(ctx context.Context) error {
    return exec.CommandContext(ctx, "kubeadm", "reset", "-f").Run()
}
```

```go
// Preferred: FIXME with a tracking link. TODO: fails the linter in controller/.
// FIXME: https://github.com/dsx-ai-factory/nke/issues/<N> — drop once the provider interface lands.

// Deprecated: an unbounded TODO with no owner and no ticket.
// TODO: clean this up later
```

```yaml
# Preferred: change the spec, then run `make generate`.
# controller/api/openapi.yaml
    nodePoolName:
      type: string
      minLength: 1
```

```go
// Deprecated: hand-editing the generated model to add the same field.
// controller/api/models.gen.go  <- regenerated output, your edit will be lost
```

## Tools

Each module pins its own tooling and downloads it on demand. Use `make`, not the
underlying binary, so versions stay pinned.

Flox is optional, but prefer it when it is available: if `flox` is on `PATH`
and you are not already inside the environment (`FLOX_ENV` is unset), run
one-shot commands as `flox activate -- make <target>` so tool versions match
CI (a `golangci-lint` older than the pin silently misses findings CI reports).
Without Flox, plain `make` works from whatever is on `PATH`.

| Tool | Module | Used for |
|---|---|---|
| Flox | all modules | Optional reproducible toolchain, pinned at the repository root. |
| `oapi-codegen` | `controller/` | Go server, client and models from the OpenAPI spec |
| `addlicense` | `controller/` | License headers (`make license`) |
| `buf` | `agent/` | Proto formatting and generation |
| `go-licenses` | `agent/` | `THIRD_PARTY_NOTICES.md` (`make notices`) |
| `golangci-lint` | both | Linting, per each module's `.golangci.yml` |

## Verification — run before submitting a pull request

In `agent/`:

```bash
make fmt
make vet
make check-protos     # fails if *.pb.go is stale
make lint             # this is the CI gate
make test
make notices-check    # fails if THIRD_PARTY_NOTICES.md has drifted
```

In `controller/`:

```bash
make generate     # only if you changed openapi.yaml
make license
make fmt
make lint
make test
```

In `cluster-api-provider-provisioned/`:

```bash
make generate-check
go test ./...
```

`make lint` in `agent/` is the check CI runs on every push, so treat a failure
there as a blocked merge. `make notices-check` regenerates the file and fails on
any difference — if it reports a change, commit the regenerated file rather than
reverting it.

Commits follow [Conventional Commits](https://www.conventionalcommits.org/).
Branches follow `<type>/<short-description>`, for example `fix/kubeadm-reset-timeout`.

Most checks are built through a `pull-request/<number>` branch rather than the
`pull_request` event, because the self-hosted runners do not pick up
`pull_request`. `copy-pr-bot` creates that branch. Expect those checks to appear
on it rather than on your own branch.

⚠️ **This is not a fork-only mechanism, despite what this file used to say.**
The bot gates on **author vetting**, not on whether the pull request comes from
a fork. Same-repository pull requests from unvetted authors are not mirrored
either, and receive none of the checks that depend on it — `golangci-lint`,
CodeQL, `govulncheck`, `gitleaks`, `grype`, `actionlint`, `shellcheck`, the
image build and the notices gate.

**Absence of a check does not look like failure.** The seven workflows on the
real `pull_request` event still report, so the pull request page shows a column
of green ticks with the other checks simply missing. If you are reviewing, check
that the checks you care about are *present*, not just passing:

```bash
gh pr checks <number> --repo dsx-ai-factory/nke --json name,workflow
```

Which checks block a merge is a repository setting, not a file. Read it live:

```bash
gh api repos/dsx-ai-factory/nke/rules/branches/main --jq '[.[].type]'
```

## Skills

Procedural workflows live under [.claude/skills/](.claude/skills/) — opening a
pull request and regenerating generated artifacts. They are plain markdown;
agents other than Claude Code should read them as documents when performing
those tasks.

## See also

- [docs/architecture.md](docs/architecture.md) — the design to understand before
  changing how the controller and the agent talk to each other: the provisioning
  data flow from `POST /api/clusters` through Temporal and Forge to a node
  registering itself over SPIFFE mTLS, and the site-controller deployment model
- [controller/AGENTS.md](controller/AGENTS.md) — the full CWE loop: Flox, Temporal
  workflows, the Bun/PostgreSQL layer, environment variables, and the local stack
- [agent/AGENTS.md](agent/AGENTS.md) — proto generation, the state file, Debian
  packaging, gRPC reflection, and the mTLS story
- [docs/getting-started.md](docs/getting-started.md) — the local development setup
  for the modules
