# NKE — NVIDIA Kubernetes Engine

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8.svg)](https://go.dev/dl/)

**A management plane for running GPU Kubernetes clusters on bare metal.** NKE
provisions and operates isolated Kubernetes clusters on NVIDIA infrastructure,
so platform teams can offer managed Kubernetes to their tenants without building
and running a control plane themselves.

**Who this is for:** NVIDIA Cloud Partners and platform operators who have GPU
bare metal and want to hand out production Kubernetes clusters on it, and
engineers integrating against that control plane.

> **Status: early.** This repository is being prepared for open source. Layout,
> interfaces, and build inputs are still changing, and there are no releases yet.
> Expect breaking changes.

## Overview

NKE is not a Kubernetes distribution and not a Cluster API flavour. It is a
**self-hosting factory for Kubernetes clusters**: a throwaway seed cluster
stands up a highly available, self-managing control plane, and that control
plane then provisions and lifecycle-manages many isolated tenant clusters on
demand.

The loop is **seed → pivot → mint**:

1. **Seed** — an ephemeral single-node cluster runs the controller, Temporal, and Postgres.
2. **Bring up** — a durable workflow runs `kubeadm init` against pre-provisioned hosts.
3. **Pivot** — the new cluster registers itself into its own database. Installing the controller onto it is a separate step today; it does not yet upgrade, cycle or scale itself.
4. **Tear down** the seed.
5. **Mint** tenant clusters on demand.

What distinguishes it from a generic CAPI distribution:

- **Hard tenant isolation.** Every tenant gets a dedicated `kubeadm` cluster with
  its own control plane and etcd, not a namespace in a shared one.
- **Secure by default.** TPM-attested node identity through SPIFFE/SPIRE, full
  disk encryption with TPM unlock, and etcd encryption at rest.
- **Built for egress-only sites.** Durable state, identity, and image serving are
  all local, so a site that cannot call out still works. Air-gapped installation
  is a first-class mode, not a workaround.
- **Pluggable infrastructure.** Bare metal is reached through the Cluster API
  contract, so the same control plane drives different backends.

### Components

| Directory | What it is |
|---|---|
| `controller/` | The control plane. REST API and UI, durable Temporal workflows, cluster and node inventory, and the infrastructure provider integrations. |
| `cluster-api-provider-provisioned/` | Cluster API infrastructure provider for hosts that NKE provisions before CAPI bootstraps them. |
| `agent/` | The on-node daemon. Exposes a gRPC API the controller drives to run `kubeadm`, install a CNI, and configure disk and etcd encryption. |
| `cli/` | `nkectl`, the command-line interface. Validates a site spec and drives bootstrap. |
| `controller/nico-sim/` | `nico-sim`, the NICo-compatible VM and Docker-container simulator used by development and tests. |
| `build/` | Build and publish scripts per component. |
| `docs/` | Architecture, getting started, and admin guides. |
| `os-image/` | Reference Ubuntu node OS image (spec + Packer) and `nke-os-image-check`. |

**Nine Go modules**, tied together by a workspace at the repository root, each
independently buildable. `make build` links six binaries: `agent`, `updater`,
`controller`, `nkectl`, `capd-reboot-controller` and
`nke-os-image-check`.
See [docs/go-workspace.md](docs/go-workspace.md) for how the workspace fits
together and why `vendor/` is committed.

What each binary is, and where it ends up running:

| Binary | Module | What it does | Runs on |
|---|---|---|---|
| `controller` | `controller/` | The control plane: REST API, Temporal workflows, and the Kubernetes reconcilers | The site controller cluster |
| `agent` | `agent/` | gRPC server the control plane drives to run `kubeadm`, install a CNI, and configure encryption | Every managed node |
| `updater` | `agent/` | Independent helper that replaces the node agent without depending on the agent process | Every managed node |
| `nkectl` | `cli/` | Validates a site spec and drives bootstrap | Your machine |
| `nke-os-image-check` | `os-image/` | Checks a node image against the contract in `os-image/SPEC.md` | Your machine, or an image build |
| `capd-reboot-controller` | `controller/hack/capd-reboot-controller/` | Test helper that reboots CAPD machines | Test clusters |

### Platforms

Development works on Linux and on Apple Silicon. Most of CI is Linux amd64, so
that is the platform with the most coverage. Packaging and image builds also run
on Linux arm64 runners: `deb.yml` and `agent-deb-publish.yml` build the Debian
packages on `ubuntu-24.04-arm`, and `image-nvcr.yml` builds images on
`linux-arm64-cpu16`.

There is no macOS runner, so nothing in CI exercises macOS. The Flox environment
targets `x86_64-linux`, `aarch64-linux` and `aarch64-darwin`, which leaves Intel
macOS out.

The agent is built for Linux on both architectures, `linux/amd64` and
`linux/arm64`, because that is where the agent runs. It is not built for macOS at
all.

The end-to-end path is the part that is amd64 only. `backend-e2e.yml` runs on
`linux-amd64-cpu16`, and the bundles `controller/hack/ci/prepare-capd-bundles.sh`
fetches are amd64 Linux binaries, so the CAPD end-to-end path does not run on
Apple Silicon.

Flox is offered, not required. Every target runs from whatever is on your `PATH`.

## Getting started

```bash
git clone https://github.com/dsx-ai-factory/nke.git
cd nke
```

**Build and test the agent:**

```bash
cd agent
make build
make test
make lint
```

The agent runs on cluster nodes, so `make build` cross-compiles it for Linux on
amd64 and arm64 and writes to `agent/bin/linux_*`. The tests and the lint run
against your own machine, but on macOS the binaries it produces will not.

**Build and test the controller:**

```bash
cd controller
make build          # → ./bin/controller
make test           # unit tests
make lint
```

The repository pins its toolchain with [Flox](https://flox.dev/) in an
environment at the root that covers every module. `flox activate` gives you the
versions CI uses, including the GNU Make 4 that macOS lacks. It is optional, and
every target runs from whatever is on your `PATH` without it.

For building, testing and what to run before opening a pull request, see
**[docs/development.md](docs/development.md)**. For the end-to-end suites and the
wider local setup, see **[docs/getting-started.md](docs/getting-started.md)**.
[docs/README.md](docs/README.md) indexes everything else.

The controller has its own guide in
[controller/README.md](controller/README.md). For a worked `nkectl` input, see
[cli/examples/sitespec.yaml](cli/examples/sitespec.yaml), which describes a
site, and [cli/examples/capd-cluster.yaml](cli/examples/capd-cluster.yaml),
which requests a cluster.

## Requirements

| Tool | Version |
|---|---|
| Go | 1.26+ |
| Docker | 24+ |
| GNU Make | 4.0+ |
| Flox | optional, for a reproducible repository toolchain |

Running NKE against real infrastructure additionally requires hosts already
booted on an NKE-compatible OS image with the agent installed. NKE does not
publish OS images; it publishes the recipes for building them.

## Documentation

| Document | Contents |
|---|---|
| [docs/first-cluster.md](docs/first-cluster.md) | Three paths to a running cluster, driven by `nkectl` |
| [docs/architecture.md](docs/architecture.md) | System overview — modules, data flows, deployment model |
| [docs/getting-started.md](docs/getting-started.md) | Local development setup for the controller and the agent |
| [docs/branch-protection-settings.md](docs/branch-protection-settings.md) | What `main` requires of a pull request, and which checks block a merge |

Module-specific documentation lives alongside the code, in `controller/docs/`,
`agent/docs/` and the `AGENTS.md` files.

## Contributing

Start with [CONTRIBUTING.md](CONTRIBUTING.md). Contributions are accepted by pull
request; please open an issue first for anything substantial so the approach can
be agreed before you write code.

Development quickstart:

```bash
git clone https://github.com/dsx-ai-factory/nke.git && cd nke
(cd agent && make build test)
(cd controller && make build test)
```

- Code of Conduct: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)
- Governance: [GOVERNANCE.md](GOVERNANCE.md)
- Maintainers: [MAINTAINERS.md](MAINTAINERS.md)
- Changelog: [CHANGELOG.md](CHANGELOG.md)
- Release process: [RELEASE.md](RELEASE.md)

## Security

Report vulnerabilities as described in [SECURITY.md](SECURITY.md). **Do not open
a public issue for a security report.**

## Support

See [SUPPORT.md](SUPPORT.md) for support channels and expectations.

Questions and discussion:
[GitHub Discussions](https://github.com/dsx-ai-factory/nke/discussions/categories/q-a).
Bugs, feature requests and documentation gaps:
[GitHub Issues](https://github.com/dsx-ai-factory/nke/issues).

## License

Apache License 2.0 — see [LICENSE](LICENSE).
