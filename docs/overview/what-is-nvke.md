# What is NVKE

> **Preview:** NVKE is available as a preview. APIs, configuration and behavior can change between releases, and we do not recommend NVKE for production workloads. See [Preview status and known limitations](preview-status.md).

NVIDIA Kubernetes® Engine (NVKE) is an open-source Kubernetes management system that an AI factory operator installs to create and run Kubernetes clusters for its customers on its own NVIDIA GPU infrastructure. The operator requests each cluster through the NVKE API, and NVKE provisions the bare-metal nodes, forms the cluster and drives operations like scaling node pools, upgrading Kubernetes, replacing nodes, and renewing certificates.

NVKE works with NVIDIA Kubernetes Recipes (NVKR), its companion build system, which builds the node OS images and components that NVKE deploys. NVKE and NVKR are licensed under Apache 2.0 and accept outside contributions.

## Why NVKE

AI factories use Kubernetes to operate their GPU fleets, and as the foundation for higher-level services like model endpoints and managed inference. Offering Kubernetes to customers as a managed service means building a cluster management system with API-driven provisioning, isolated control planes for each customer, upgrades, node replacement, node identity and observability across a fleet of GPU machines. Without NVKE, an operator or Kubernetes ISV assembles that system from open-source and commercial components and maintains it through every Kubernetes release and GPU generation.

NVKE provides that system as open-source software. With NVKE, you can:

- **Launch a Kubernetes service without building a cluster management system first.** NVKE supplies cluster provisioning, lifecycle, node identity and telemetry. You build the customer-facing service on top of NVKE.
- **Build customer clusters from pinned, verifiable configurations.** NVKR builds each artifact from pinned sources and records a software bill of materials (SBOM) for it. Releases from NVKR's hosted pipeline are signed and carry build provenance, which you check against your own policy before an artifact reaches a cluster.
- **Keep control of the service.** You own the build, the registries and the supply chain. You can adopt NVKE whole, integrate parts of it into your own platform, or extend it.
- **Build on upstream Kubernetes.** NVKE uses upstream Kubernetes, kubeadm and Cluster API. The Cloud Native Computing Foundation (CNCF) [has certified](https://github.com/cncf/k8s-conformance/pull/4492) that NVKE creates conformant Kubernetes clusters, so your customers' workloads, tools and skills carry over from other Kubernetes environments unchanged. NVKE is a [Certified Kubernetes AI Platform](https://github.com/cncf/landscape/pull/5236).

## Who NVKE is for

- **AI factory operators**, including NVIDIA Cloud Partners, that offer Kubernetes to their customers, either directly or as the orchestration layer for inference, training and fine-tuning services. Most of this documentation is written for you. Start with [Plan your installation](../operate/plan-installation.md).
- **Kubernetes ISVs and platform providers** that integrate parts of NVKE or NVKR, such as CAPNICo or the NVKE telemetry pipeline, into their own products.
- **Customers of an AI factory** who receive a workload cluster from their operator. Start with [Access a workload cluster](../workload-clusters/access.md).
- **Evaluators and contributors.** Try NVKE on one Linux machine, without GPU hardware, in [Quickstart: evaluate locally with mock-NICo](../get-started/quickstart-local.md).

## What's included

- **Build the artifacts you deploy.** NVKR builds the node OS images, binaries, container images and Helm charts that NVKE deploys, into registries you control. You can also build the reference node image directly from the NVKE repository. See [Build and host node OS images](../operate/build-node-images.md) and the [NVKR repository](https://github.com/dsx-ai-factory/nvkr).
- **Stand up the management plane.** The `nvkectl` CLI bootstraps a management cluster on nodes you prepare. You then install the NVKE management plane on it. See [Bootstrap the management cluster](../operate/bootstrap-management-cluster.md).
- **Create and operate workload clusters.** Create, describe, update and delete clusters and node pools through the NVKE API or `nvkectl`. Each workload cluster has its own control plane, etcd and node pools. With NVSentinel installed, NVKE also reboots or replaces the nodes that NVSentinel reports as faulty. See [Create a workload cluster](../workload-clusters/create.md).
- **Provision bare metal through NICo.** NVKE requests machines from NVIDIA Infra Controller (NICo) through CAPNICo, the open-source Cluster API (CAPI) infrastructure provider for NICo. Any other Cluster API-based tool can use CAPNICo too. See [Configure an infrastructure provider: NICo](../operate/provider-nico.md).
- **Verify each node before it joins.** On bare metal, each node attests with its TPM through SPIFFE/SPIRE before it receives a join credential. etcd encryption at rest is available per cluster.
- **Network with OVN-Kubernetes.** Clusters use an NVIDIA-curated build of OVN-Kubernetes, which is also open source. See [Configure OVN-Kubernetes networking](../workload-clusters/ovn-kubernetes.md).
- **Observe the fleet.** The NVKE telemetry pipeline (NKTP) runs OpenTelemetry Collectors that gather a cluster's metrics, logs and traces and export them to your backend over the OpenTelemetry Protocol (OTLP). NVKE installs it on the management cluster, and it also runs on any other Kubernetes cluster. See [Observe NVKE and its clusters](../operate/observe.md).

## How NVKE works

One NVKE installation manages a fleet of workload clusters inside a single AI factory. You start by bootstrapping a **management cluster** on nodes you prepare. `nvkectl` starts a temporary **seed cluster**, uses it to form the management cluster with kubeadm, and deletes it when bootstrap succeeds. The management cluster runs the **NVKE management plane**, the API and controllers that operate clusters, node pools, and nodes.

When you create a **workload cluster**, the management plane requests machines from your **infrastructure provider**, NICo, through Cluster API. NICo provisions each machine from your node OS image, and the NVKE node agent (`nvke-agent`) on the machine runs kubeadm and installs the cluster's components. Nodes belong to **node pools** that share an instance type, a release and a Kubernetes version. Your customer receives a kubeconfig that they use to run `kubectl` with their cluster's Kubernetes API endpoint.

The diagram shows the NVKE product family, how it sits on NICo and your GPU machines, and which components talk to each other.

![NVKE product family. NVKR builds and signs artifacts and publishes them to a registry. The operator installs the management cluster's components, the NVKE management plane, CAPNICo and NVKE observability, from the charts and images in the registry. The management plane requests machines through CAPNICo, which calls NICo, and drives each workload cluster. NICo provisions NVIDIA GPU machines from the node OS image, and those machines become the workload cluster's Kubernetes nodes, each running nvke-agent, under the cluster's Kubernetes control plane. The accelerated runtime produced by AICR and NVKE observability are optional in the workload cluster. The customer uses kubectl against the control plane.](../assets/diagrams/product-family.svg)

| Stage | What you do | Read more |
|---|---|---|
| **Build** | Build the node OS image, serve its boot files over HTTP(S), and publish component artifacts to your registries. Repeat for each Kubernetes version or security fix. | [Build and host node OS images](../operate/build-node-images.md) |
| **Day 0: Bootstrap** | Prepare the management cluster's nodes, bootstrap the management cluster with `nvkectl`, and install the management plane. | [Bootstrap the management cluster](../operate/bootstrap-management-cluster.md) |
| **Day 1: Configure** | Connect the management plane to NICo, turn on API authentication, and register the node images and releases that clusters use. | [Configure an infrastructure provider: NICo](../operate/provider-nico.md), [Secure the NVKE API](../operate/api-authentication.md) |
| **Day 2: Run clusters** | Create workload clusters, give customers access, and install the accelerated runtime with [NVIDIA AI Cluster Runtime (AICR)](https://docs.nvidia.com/aicr). NVKE then carries out scaling, upgrades, node replacement and certificate renewal. | [Create a workload cluster](../workload-clusters/create.md), [Manage node pools](../workload-clusters/node-pools.md) |

## Who is responsible for what

NVIDIA provides the software, and the AI factory runs the service.

| Who | Is responsible for |
|---|---|
| NVKE and NVKR (the software) | Building artifacts from pinned sources and signing hosted Releases (NVKR). Provisioning nodes, forming clusters, node identity, encryption settings, upgrades, node replacement and certificate renewal (NVKE). |
| The operator | The infrastructure and NICo, the management cluster, building, verifying and publishing artifacts, the accelerated runtime, cluster availability, and the access customers receive. |
| The customer | Workloads, their images, namespaces and application secrets, and RBAC for their own users within the operator's limits. |

[Plan your installation](../operate/plan-installation.md) lists what you need before you install, and [Supported platforms and versions](supported-platforms.md) lists what this release supports.

## What NVKE does not do

- **Manage bare metal.** NICo discovers machines, updates firmware, configures networks and provisions hosts. NVKE requests machines from NICo through CAPNICo.
- **Ship OS images or Kubernetes binaries.** You build them, with NVKR or from the NVKE repository, and host them yourself.
- **Run your customers' workloads.** You run NVKE to operate the cluster, and the customer owns what runs in it.

## NVKE in DSX OS

NVKE is the Kubernetes management layer of DSX OS, NVIDIA's software stack for AI factories, and provides the Kubernetes clusters that DSX OS itself runs on. Each component below is a separate project:

| Layer | Component | Role |
|---|---|---|
| Infrastructure | NICo | Bare-metal machine and network lifecycle: discovery, firmware, network configuration and host provisioning. |
| Build | NVKR | Builds the artifacts NVKE deploys, and signs the Releases from its hosted pipeline. |
| Kubernetes | NVKE | Creates and operates Kubernetes clusters on NICo machines. |
| Accelerated runtime | [AICR](https://docs.nvidia.com/aicr) | Validated, version-locked combinations of the GPU Operator, Network Operator, DRA and other runtime components, installed into each workload cluster. |
| Health | NVSentinel | Detects GPU faults on each node and requests remediation from NVKE. |

## Next steps

- **Try NVKE without hardware:** [Quickstart: evaluate locally with mock-NICo](../get-started/quickstart-local.md).
- **Build your first artifacts:** [Build and host node OS images](../operate/build-node-images.md).
- **Install on your hardware:** [Plan your installation](../operate/plan-installation.md).
- **Check what preview supports:** [Preview status and known limitations](preview-status.md) and [Supported platforms and versions](supported-platforms.md).

## Trademarks

Kubernetes® is a registered trademark of The Linux Foundation in the United States and other countries. NVIDIA and the NVIDIA logo are trademarks and/or registered trademarks of NVIDIA Corporation in the U.S. and other countries. Other company and product names may be trademarks of the respective companies with which they are associated.

The Linux Foundation has registered trademarks and uses trademarks. For a list of trademarks of The Linux Foundation, please see its Trademark Usage page: https://www.linuxfoundation.org/legal/trademark-usage.
