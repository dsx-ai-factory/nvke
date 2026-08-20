# Getting started

This guide walks you through setting up a local development environment
for nke and verifying that both modules build and test cleanly.

## Prerequisites

| Tool | Version | Install |
|---|---|---|
| Go | 1.26+ | https://go.dev/dl/ |
| Docker | 24+ | https://docs.docker.com/get-docker/ |
| Flox (optional) | latest | https://flox.dev/ — used for reproducible controller toolchain |

## Clone the repository

```bash
git clone https://github.com/NVIDIA/nke.git
cd nke
```

## controller (Cloud Workflow Engine)

> **`controller/` builds outside the NVIDIA network**, as of #76, which replaced
> the internal carbide SDK with the public
> `github.com/NVIDIA/ncx-infra-controller-rest`. The one internal dependency
> left, `cluster-api-provider-nico`, is carried in the committed `vendor/` tree,
> so the commands in this section need no credentials. From the repository root,
> `GOPROXY=off make build` compiles all nine modules and proves it. See
> [AGENTS.md](../AGENTS.md).

The controller uses [Flox](https://flox.dev/) to pin its toolchain. For
day-to-day work, activate the Flox environment first:

```bash
cd controller
flox activate
```

### Build

```bash
make build          # → ./bin/cloud-workflow-engine
```

### Lint

```bash
make lint           # golangci-lint (5 min timeout)
make fmt            # gofmt + goimports
```

### Test

```bash
make test           # unit tests with coverage
```

### Code generation

If you change `api/openapi.yaml`, regenerate Go bindings:

```bash
flox activate -- make generate
```

Never hand-edit `*.gen.go` files — they will be overwritten.

### Run locally

Start dependencies:

```bash
# PostgreSQL
docker run -d --name=nke-postgres -p 5432:5432 -e POSTGRES_PASSWORD=postgres postgres:17.4

# Temporal (dev mode)
temporal server start-dev
```

Then run CWE:

```bash
FWE_API_KEY=<your-NGC-api-key> make run
# UI:      http://localhost:8080
# Swagger: http://localhost:8080/api/docs
```

Without an NGC account, run the `test` Forge environment instead. It signs a
self-signed JWT rather than authenticating against NGC, so it needs no
`FWE_API_KEY`:

```bash
FORGE_ENVIRONMENT=test make run
```

## agent (nke-agent)

The source lives in `agent/`. The binary installed on a node is called
`nke-agent`.

```bash
cd agent
go build ./cmd/...
go test ./...          # or: make test
```

> The tests assume a Linux filesystem. Several fail on macOS, where `/var` is
> a symlink to `/private/var` and the kubeadm file checks reject it.

`nke-agent` and `spire-agent` must be present on each managed node before
the NKE controller can communicate with it. The binaries are typically
pre-built into the OS image or added via cloud-init — the controller does
not install them. See `agent/AGENTS.md` for the full build, proto and test
commands.

## Next steps

- Read `docs/architecture.md` for a system overview
- Read `controller/AGENTS.md` for AI-assisted development tips
- See `CONTRIBUTING.md` for the PR workflow and code standards
