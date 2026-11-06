# Getting started

This guide walks you through setting up a local development environment
for NVKE and verifying that the modules build and test cleanly. The repository is
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
git clone https://github.com/dsx-ai-factory/nvke.git
cd nvke
```

## controller

> **`controller/` builds without NVIDIA credentials.** All of its dependencies
> are public. A cold build needs
> access to the public Go module proxy; from the repository root,
> `make build` compiles all thirteen modules. See
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
make -C controller capi-core-up           # kind cluster with Cluster API core, which the controller watches
make -C controller releases-install       # release CRDs and fixture releases
make -C controller run-local              # Postgres, Temporal, then the controller
```

Swagger is on [http://localhost:8080/api/docs/](http://localhost:8080/api/docs/). Infrastructure credentials are
configured on the Cluster API provider rather than the controller.

[first-cluster.md](first-cluster.md#api-only) covers this path in
full, and goes on to tenant clusters.

## WSL2

Ubuntu under WSL2 runs the same workflow as native Ubuntu: Flox, `make` and
Docker Engine inside the distribution, with no WSL-specific targets. These
settings differ from a default WSL2 install.

| Setting | Why |
|---|---|
| `[boot] systemd=true` in `/etc/wsl.conf` | Docker Engine and the `modules-load.d` handling in `host-up` expect systemd |
| Repository under `~`, not `/mnt/c` | The Windows mount drops executable bits and is slow for Go builds and Tilt's file watcher |
| `memory=12GB` or more, `processors=6` or more in `%UserProfile%\.wslconfig` | The Mock NICo stack needs 6 CPUs and 12 GiB free |
| `networkingMode=mirrored` in `.wslconfig` | `localhost:8080` and the Tilt UI on `localhost:10350` then open from a Windows browser |
| `fs.inotify.max_user_instances` of 1024 or more | The default is 128 on many distributions; Tilt restarts in a loop below 1024 |

**API Only** (`capi-core-up`, `releases-install`, then `run-local`, as above)
needs nothing beyond a working Docker daemon.

**Mock NICo and the local site** need `/dev/vtpmx`, and the stock WSL2 kernel is
built without `CONFIG_TCG_VTPM_PROXY`. No Ubuntu package supplies the module for
a Microsoft kernel, so `make -C controller host-up` builds it. It clones the
`linux-msft-wsl-<version>` tag that matches `uname -r`, compiles
`tpm_vtpm_proxy` out of tree against that kernel's `/proc/config.gz`, installs it
under `/lib/modules/$(uname -r)/extra` and loads it. No custom kernel and no
`wsl --shutdown` are involved.

The first run installs build dependencies with `apt` and takes about 10 minutes.
The build is cached in `/var/cache/nvke-wsl-vtpm`, and a WSL kernel update
(`wsl --update`) triggers one rebuild on the next `host-up`.

## agent (nvke-agent)

The source lives in `agent/`. The binary installed on a node is called
`nvke-agent`.

```bash
cd agent
make build             # cross-compiles for linux; a host go build on macOS
                       # produces a binary no node can run
go test ./...          # or: make test
```

`nvke-agent` and `spire-agent` must be present on each managed node before
the NVKE controller can communicate with it. The binaries are typically
pre-built into the OS image or added via cloud-init — the controller does
not install them. See `agent/AGENTS.md` for the full build, proto and test
commands.

## Next steps

- Read `docs/architecture.md` for a system overview
- Read `controller/AGENTS.md` for AI-assisted development tips
- See `CONTRIBUTING.md` for the PR workflow and code standards
