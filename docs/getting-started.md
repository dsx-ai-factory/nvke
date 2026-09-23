# Getting started

This guide walks you through setting up a local development environment
for nke and verifying that the modules build and test cleanly. The repository is
a Go workspace of several modules, listed in `go.work`. The repository README
says what each one is.

## Prerequisites

| Tool | Version | Install |
|---|---|---|
| Go | 1.26+ | https://go.dev/dl/ |
| Docker | 24+ | https://docs.docker.com/get-docker/ |
| Flox (optional) | latest | https://flox.dev/ — pins the whole toolchain, repository-wide |
| GNU Make | 4.0+ | macOS ships 3.81; `brew install make` gives you `gmake` |

A newer Go than 1.26 is fine. `make` pins `GOTOOLCHAIN` to the version `go.work`
declares and fetches it if needed, so builds do not depend on which Go is on your
`PATH`. Building with `go` directly, outside `make`, does — and a toolchain newer
than the declared one panics at startup with a `strcase.UnicodeVersion` mismatch.

## Clone the repository

```bash
git clone https://github.com/dsx-ai-factory/nke.git
cd nke
```

## controller (Cloud Workflow Engine)

> **`controller/` builds outside the NVIDIA network**, as of #76, which replaced
> the internal carbide SDK with the public
> `github.com/NVIDIA/ncx-infra-controller-rest`. The one internal dependency
> left, `cluster-api-provider-nico`, is carried in the committed `vendor/` tree,
> so the commands in this section need no credentials. From the repository root,
> `GOPROXY=off make build` compiles all eleven modules and proves it. See
> [AGENTS.md](../AGENTS.md).

The repository uses [Flox](https://flox.dev/) to pin its toolchain, in one
environment at the root that serves every module. Activate it from anywhere in
the tree:

```bash
flox activate
```

It is optional. Every target runs from whatever is on your `PATH` without it, and
a target that needs a missing tool says which one and how to install it.

### Build

```bash
make -C controller build          # → controller/bin/controller
```

### Lint

```bash
make -C controller lint           # golangci-lint (20 min timeout)
make -C controller fmt            # gofumpt + goimports
```

### Test

```bash
make -C controller test           # unit tests with coverage
```

### Code generation

If you change `controller/api/openapi.yaml`, regenerate Go bindings:

```bash
make -C controller generate
```

Never hand-edit `*.gen.go` files — they will be overwritten.

### Run locally

```bash
kind create cluster                       # CWE watches a cluster
make -C controller run-local              # Postgres, Temporal, then CWE
```

Swagger is on <http://localhost:8080/api/docs/>. Infrastructure credentials are
configured on the Cluster API provider rather than CWE.

[first-cluster.md](first-cluster.md#path-a--the-api-alone) covers this path in
full, and goes on to tenant clusters.

## agent (nke-agent)

The source lives in `agent/`. The binary installed on a node is called
`nke-agent`.

```bash
cd agent
make build             # cross-compiles for linux; a host go build on macOS
                       # produces a binary no node can run
go test ./...          # or: make test
```

`nke-agent` and `spire-agent` must be present on each managed node before
the NKE controller can communicate with it. The binaries are typically
pre-built into the OS image or added via cloud-init — the controller does
not install them. See `agent/AGENTS.md` for the full build, proto and test
commands.

## Next steps

- Read `docs/architecture.md` for a system overview
- Read `controller/AGENTS.md` for AI-assisted development tips
- See `CONTRIBUTING.md` for the PR workflow and code standards
