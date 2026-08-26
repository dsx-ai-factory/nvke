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
3. **Pivot** — the new cluster registers itself into its own database and takes over managing itself.
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
| `cli/` | `nkectl`, the command-line interface. Validates a site inventory and drives bootstrap. |
| `controller/nico-vms/` | VM lifecycle service used by the development and test environments. |
| `build/` | Build and publish scripts per component. |
| `docs/` | Architecture, getting started, and admin guides. |
| `os-image/` | Reference Ubuntu node OS image (spec + Packer) and `nke-os-image-check`. |

**Ten Go modules**, tied together by a workspace at the repository root, each
independently buildable. `make build` links six binaries: `agent`,
`cloud-workflow-engine`, `nkectl`, `capd-reboot-controller`, `nico-mock` and
`nke-os-image-check`.
See [docs/go-workspace.md](docs/go-workspace.md) for how the workspace fits
together and why `vendor/` is committed.

## Getting started

```bash
git clone https://github.com/NVIDIA/nke.git
cd nke
```

**Build and test the agent:**

```bash
cd agent
make build
make test
make lint
```

**Build and test the controller:**

```bash
cd controller
make build          # → ./bin/cloud-workflow-engine
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
| [docs/architecture.md](docs/architecture.md) | System overview — modules, data flows, deployment model |
| [docs/getting-started.md](docs/getting-started.md) | Local development setup for the controller and the agent |
| [docs/branch-protection-settings.md](docs/branch-protection-settings.md) | Admin guide to the required GitHub branch protection rules |

Module-specific documentation lives alongside the code, in `controller/docs/`,
`agent/docs/` and the `AGENTS.md` files.

## Contributing

Start with [CONTRIBUTING.md](CONTRIBUTING.md). Contributions are accepted by pull
request; please open an issue first for anything substantial so the approach can
be agreed before you write code.

Development quickstart:

```bash
git clone https://github.com/NVIDIA/nke.git && cd nke
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
[GitHub Discussions](https://github.com/NVIDIA/nke/discussions/categories/q-a).
Bugs, feature requests and documentation gaps:
[GitHub Issues](https://github.com/NVIDIA/nke/issues).

## License

Apache License 2.0 — see [LICENSE](LICENSE).
