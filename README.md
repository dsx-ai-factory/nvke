# NVIDIA Kubernetes Engine (NVKE)

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.27.2-00ADD8.svg)](go.work)

**Run a managed Kubernetes service on your own NVIDIA GPU infrastructure.**
NVIDIA Kubernetes® Engine (NVKE) provides the management plane that platform teams install to create and
operate Kubernetes clusters for their customers. Each workload cluster has its
own Kubernetes control plane, etcd and node pools. You operate the NVKE management
plane; your customers use their clusters through the Kubernetes API and `kubectl`.

> **Preview:** NVKE is available as a preview. APIs, configuration and behavior
> can change between releases, and we do not recommend NVKE for production
> workloads. See [Preview status and known limitations](docs/overview/preview-status.md).

[Get started](#getting-started) · [Documentation](docs/README.md) ·
[Contribute](CONTRIBUTING.md) · [Get help](SUPPORT.md)

## Getting started

| Your goal | Start here | What you get |
|---|---|---|
| **Evaluate without GPU hardware** | [Local quickstart](docs/get-started/quickstart-local.md) | A development management plane and a workload cluster on simulated machines. No NVIDIA account is required. |
| **Install on your infrastructure** | [Plan your installation](docs/operate/plan-installation.md) | The prerequisites and sequence for preparing nodes, bootstrapping a management cluster and installing its services. |
| **Build or contribute** | [Build from source](#build-from-source) | The NVKE binaries and the contributor development loop. |

The local quickstart runs on a **Linux host or VM, amd64 or arm64**, with at least
**6 CPUs, 12 GiB of free memory and 100 GiB of free disk**. It requires Docker,
virtual TPM support and the tools listed in the guide. Follow its setup through
workload-cluster creation and confirm the cluster's nodes report `Ready`.
On macOS, use the [local site guide](docs/get-started/local-site.md), which runs
the evaluation inside a Colima virtual machine.

For a real installation, you supply the machines, networking, node images and
registries, and configure access to **NVIDIA Infra Controller (NICo)**. NICo is
the sole compatible infrastructure provider at preview. The
[platform and version reference](docs/overview/supported-platforms.md) lists
validated combinations and pinned dependencies.

## What NVKE does

- **Creates independent workload clusters.** Request clusters through the NVKE
  API or `nvkectl`; NVKE provisions machines through NICo and forms Kubernetes
  clusters with their own control planes.
- **Manages workload cluster lifecycle.** Create, scale and delete node pools,
  upgrade Kubernetes and replace nodes. See the [node pool guide](docs/workload-clusters/node-pools.md)
  for the manual steps and preview limits on replacement and upgrades.
- **Establishes node identity.** Nodes attest through SPIFFE/SPIRE using their
  TPM before receiving join credentials. Encryption of etcd data is available
  per cluster.
- **Connects builds to deployment.** Its companion project,
  [NVIDIA Kubernetes Recipes (NVKR)](https://github.com/dsx-ai-factory/nvkr),
  builds node images and NVKE components from pinned inputs. NVKE deploys those
  artifacts; NVKR owns their build and release evidence.

The preview needs explicit operator configuration: API authentication is off
and the API uses plain HTTP after installation. Configure
[API authentication and protected access](docs/operate/api-authentication.md)
before admitting callers. The management cluster does not upgrade, scale or
replace itself, and there is no management-plane upgrade or backup/restore
procedure. FDE node images are for evaluation and provide no data-at-rest
confidentiality. Read the [known limitations](docs/overview/preview-status.md)
before choosing an evaluation or installation path.

## Overview

The diagram shows the NVKE product family, how it sits on NICo and your GPU machines, and which components talk to each other.

![NVKE product family. NVKR builds and signs artifacts and publishes them to a registry. The operator installs the management cluster's components, the NVKE management plane, CAPNICo and NVKE observability, from the charts and images in the registry. The management plane requests machines through CAPNICo, which calls NICo, and drives each workload cluster. NICo provisions NVIDIA GPU machines from the node OS image, and those machines become the workload cluster's Kubernetes nodes, each running nvke-agent, under the cluster's Kubernetes control plane. The accelerated runtime produced by AICR and NVKE observability are optional in the workload cluster. The customer uses kubectl against the control plane.](docs/assets/diagrams/product-family.svg)

NVKE is not a Kubernetes distribution and not a Cluster API flavour. It is a
**self-hosting factory for Kubernetes clusters**: a throwaway seed cluster
stands up a management cluster, and the management plane on
it then provisions and lifecycle-manages many isolated workload clusters
on demand.

An installation has two kinds of Kubernetes cluster:

- The **management cluster** runs the NVKE management plane and infrastructure
  controllers. `nvkectl` forms it using a temporary seed cluster; installing the
  management-plane services is a separate step.
- **Workload clusters** run customer applications. The management plane creates
  and operates them through NICo, Cluster API and the node agent.

Read [What is NVKE](docs/overview/what-is-nvke.md) for the operator workflow and
[Architecture](docs/architecture.md) for the implementation and data flows.

## Build from source

Use **Go 1.27.2**, the version pinned in [go.work](go.work), and **GNU Make 4 or later**. Public Go modules must be
reachable on the first build. [Flox](https://flox.dev/) is optional and supplies
the repository's development tools; on macOS, it also supplies GNU Make 4.
See [Development](docs/development.md) for toolchain setup.

```bash
git clone https://github.com/dsx-ai-factory/nvke.git
cd nvke
make build
```

This builds the workspace and writes seven binaries to `bin/`: `agent`,
`updater`, `controller`, `nvkectl`, `capd-reboot-controller`,
`nvke-os-image-check` and `management-engine`. Building does not start a
management plane or create a cluster. The node agent runs on Linux.

The [workspace guide](docs/go-workspace.md) explains the modules and outputs.
For local services and test environments, see [Local development setup](docs/getting-started.md).
Before submitting a change, follow [CONTRIBUTING.md](CONTRIBUTING.md) and run
`make preflight`; it selects checks for the modules you changed.

## Documentation

| Task | Guide |
|---|---|
| Prepare nodes and bootstrap the management cluster | [Bootstrap the management cluster](docs/operate/bootstrap-management-cluster.md) |
| Install NVKE services | [Install management-plane services](docs/operate/install-management-plane-services.md) |
| Build node images and supply release artifacts | [Get NVKE artifacts from NVKR](docs/operate/build-node-images.md) |
| Create and manage customer clusters | [Create a workload cluster](docs/workload-clusters/create.md), [Manage node pools](docs/workload-clusters/node-pools.md) |
| Give a customer access | [Access a workload cluster](docs/workload-clusters/access.md) |
| Inspect cluster health and telemetry | [Observe NVKE and its clusters](docs/operate/observe.md) |

The [documentation index](docs/README.md) links the remaining guides and references.

## Contributing and support

Contributions, bug reports and documentation improvements are welcome.
Start with [CONTRIBUTING.md](CONTRIBUTING.md); open an issue before substantial
work so maintainers can agree on the approach.

- **Questions:** [GitHub Discussions](https://github.com/dsx-ai-factory/nvke/discussions/categories/q-a).
- **Bugs and feature requests:** [GitHub Issues](https://github.com/dsx-ai-factory/nvke/issues).
- **Security vulnerabilities:** follow [SECURITY.md](SECURITY.md) and report privately.
- **Community:** [Code of Conduct](CODE_OF_CONDUCT.md), [Governance](GOVERNANCE.md),
  [Maintainers](MAINTAINERS.md) and [Support](SUPPORT.md).
- **Releases:** [Changelog](CHANGELOG.md) and [release process](RELEASE.md).

## Trademarks

Kubernetes® is a registered trademark of The Linux Foundation in the United
States and other countries. NVIDIA and the NVIDIA logo are trademarks and/or
registered trademarks of NVIDIA Corporation in the U.S. and other countries.
Other company and product names may be trademarks of the respective companies
with which they are associated.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
