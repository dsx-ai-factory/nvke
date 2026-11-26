# Composable NVKE architecture with reusable CAPI providers

**Status:** accepted

**Date:** 2026-11-05

## Context

NVIDIA Kubernetes Engine (NVKE) was originally built for internal use around a
specific use case. Its architecture could assume an integrated stack tailored to
that use case; independent adoption of its components was not a requirement.

Broader adoption calls for a complete managed Kubernetes product composed of
reusable components. Operators building a managed service for customers need
tools to curate the configurations they offer and govern their lifecycle.
Adopters with existing management systems may instead need NVKE's control plane
or bootstrap capabilities without the service capabilities above them.

The architecture should support adoption at progressively lower layers, with
each layer adding capabilities over clear, standardized interfaces. Cluster API
(CAPI) provides established provider contracts on which NVKE can build reusable
cluster lifecycle components, then compose those components into a managed
service.

## Decision

We will build NVKE as three composable layers.

- **NVKE API** is the entry point for customers of the managed service,
  implemented as a stateless REST wrapper around NVKE Service. It authenticates
  callers and authorizes requests in the context of their tenant so customers
  can request, manage, and observe their clusters.
- **NVKE Service** lets operators curate cluster configurations, add-ons, and
  release channels and enforce upgrade policies. Customers choose from those
  offerings to request or update clusters, and NVKE Service translates their
  requests into CAPI resources.
- **NVKE Core** builds on the standard CAPI lifecycle for clusters and machines,
  including control plane, bootstrap, and infrastructure providers. NVKE implements
  independently usable CAPI providers, as well as extension controllers for lifecycle
  capabilities not available in CAPI core.

We will replace the existing PostgreSQL-backed NVKE controller REST API with the
NVKE API over the NVKE Service layer. This requires a transition for existing
clients and managed resources; the legacy API will be retired entirely.

The diagram illustrates these layers. Resource names and provider choices are
examples, not finalized schemas or a statement of current support.

![NVKE architecture: the NVKE API exposes customer access above NVKE Service, whose CRDs and controllers curate cluster configurations, releases, release channels, and add-ons. The Service layer materializes CAPI resources in NVKE Core, which contains CAPI controllers, providers, and extensions. nvkectl provides operator tools for NVKE Service and NVKE Core.](../assets/diagrams/capi-architecture-light.svg#gh-light-mode-only)
![NVKE architecture: the NVKE API exposes customer access above NVKE Service, whose CRDs and controllers curate cluster configurations, releases, release channels, and add-ons. The Service layer materializes CAPI resources in NVKE Core, which contains CAPI controllers, providers, and extensions. nvkectl provides operator tools for NVKE Service and NVKE Core.](../assets/diagrams/capi-architecture-dark.svg#gh-dark-mode-only)

### NVKE Core: reusable cluster lifecycle capabilities

NVKE Core will build on Kubernetes and CAPI to provide reusable lifecycle
capabilities through providers and cooperating controllers. Its scope includes
control plane orchestration, bootstrap data generation, infrastructure
integration, and extensions such as machine bootstrap identity. Consumers can
use these capabilities through Kubernetes APIs without NVKE Service
or NVKE API, subject to documented dependencies.

We will implement the standard CAPI
[control plane](https://cluster-api.sigs.k8s.io/developer/providers/contracts/control-plane)
and [bootstrap](https://cluster-api.sigs.k8s.io/developer/providers/contracts/bootstrap-config)
contracts alongside infrastructure providers. CAPI and provider controllers will
own cluster and machine lifecycle and their child resources.

| Provider role | Direction |
| --- | --- |
| Control plane | Implement NVKE control plane lifecycle through one or more providers, with `NVKEControlPlane` as an initial example. Potential hosting models include infrastructure machines joining the workload cluster as control plane nodes, backing infrastructure managed privately by the provider, and hosted control planes. |
| Bootstrap | Build one or more NVKE bootstrap providers, and use the [CAPI kubeadm bootstrap provider (CABPK)](https://cluster-api.sigs.k8s.io/tasks/bootstrap/kubeadm-bootstrap/) for supported use cases where it fits. Providers own bootstrap data generation and publication through the CAPI bootstrap Secret contract. |
| Infrastructure | Continue using infrastructure providers, such as CAPNICo for NVIDIA Infra Controller (NICo), to manage infrastructure cluster and machine resources. |

We will build extension controllers to add lifecycle capabilities alongside CAPI
core and our CAPI providers. Existing NVKE capabilities we will implement this
way include authorizing node enrollment, handling node label-based reboot and
termination requests, informing the infrastructure provider when hardware
needs repair, and delegating node drain and workload disruption decisions to
external break-fix tools (e.g. NVSentinel).

Package controllers can provide reusable add-on installation, upgrade, and
removal through declarative APIs. CAPI's
[add-on provider model](https://github.com/kubernetes-sigs/cluster-api/blob/main/docs/proposals/20220712-cluster-api-addon-orchestration.md#provider-contracts)
does not define a shared runtime contract equivalent to its control plane or
bootstrap contracts. We need not build a dedicated NVKE CAPI add-on provider;
executor selection remains a separate design choice. This execution capability
will support the managed add-on policies defined by NVKE Service.

#### Example: creating a cluster through the CAPI interface

This example shows the seven CAPI resources a caller would typically supply for
a cluster with an NVKE control plane, one worker pool, and NICo infrastructure.

![CAPI input resources: Cluster refers to NicoCluster and NVKEControlPlane. NVKEControlPlane refers to the control-plane NicoMachineTemplate. The worker MachineDeployment names the Cluster and refers to NVKEConfigTemplate and a separate worker NicoMachineTemplate.](../assets/diagrams/capi-input-resources-light.svg#gh-light-mode-only)
![CAPI input resources: Cluster refers to NicoCluster and NVKEControlPlane. NVKEControlPlane refers to the control-plane NicoMachineTemplate. The worker MachineDeployment names the Cluster and refers to NVKEConfigTemplate and a separate worker NicoMachineTemplate.](../assets/diagrams/capi-input-resources-dark.svg#gh-dark-mode-only)

The control plane provider manages bootstrap for its own machines. For workers,
CAPI core controllers clone the bootstrap and infrastructure templates into an
`NVKEConfig` and a `NicoMachine` for each worker, then creates a `Machine` that
references both. The bootstrap and infrastructure providers reconcile those objects.

### NVKE Service: operator curation and managed cluster intent

NVKE Service will build on NVKE Core to let operators define the
managed service they offer customers. Its Kubernetes CRDs and controllers will
curate approved configurations, reconcile cluster intent, and govern lifecycle
within explicitly supported NVKE compositions.

The operator catalog will include `MachineImage`, `ControlPlaneClass`,
`ClusterRelease`, `NodeProfile`, and `AddOnRelease`. Cluster and node pool
releases will specify approved images and bootstrap inputs; add-on releases will
specify approved software versions and configuration. Each release type will
have a corresponding channel concept, providing named, operator-controlled
streams of approved releases. Operators can promote releases for consumers
following a channel without changing each consumer's channel selection.
Promotion and upgrade execution are separate: eligibility, timing, and pinning
behavior will be defined separately.

NVKE Service will reconcile `NVKECluster` and its curated selections into
CAPI `Cluster` resources, NVKE control plane resources such as
`NVKEControlPlane`, worker `MachineDeployment` resources, and bootstrap and
infrastructure templates. It will create and observe these resources directly
and derive product status from their observed state.

Managed add-ons will be part of this curated service model. Operators will define
available software, compatibility, configuration limits, and lifecycle and upgrade
policies. NVKE Service will coordinate changes with cluster lifecycle and
expose desired and observed versions, upgrade status, and ongoing health.

#### Potential future integration with managed topologies

ClusterClass and managed topologies could provide a future integration layer
between the Service layer and providers. NVKE Service could resolve curated
selections into `Cluster.spec.topology`, including a supported ClusterClass and
its variables. The topology controller would reconcile the desired-state fields
derived from class templates, replacing the Service layer's direct management
of those fields. Potential advantages include reusable templates and patches,
consistent configuration, simpler node pool creation, and CAPI coordination of
control plane and worker Kubernetes upgrades.

For clusters using managed topologies, Runtime SDK
[lifecycle hooks](https://github.com/kubernetes-sigs/cluster-api/blob/main/docs/book/src/developer/runtime-extensions/implement-lifecycle-hooks.md)
could let NVKE extensions block upgrades until required add-on updates and health
checks complete, or block deletion until cleanup finishes. NVKE would still
provide the policies, package operations, and health evaluation. Adoption remains
a future option, subject to provider compatibility and the required CAPI features.

### NVKE API: customer access to the managed service

The NVKE API will expose NVKE Service to end customers through a stateless REST
interface. It will authenticate each caller and check whether they may perform
the requested operation for the tenant. It will translate authorized requests
into operations on the Service layer's Kubernetes resources and return cluster status
from those resources. NVKE Service and NVKE Core will remain responsible for
reconciliation and lifecycle behavior.

Operators can integrate or adapt this API to their environment's identity,
tenancy, and API conventions. Adopters can also build their own customer-facing
interfaces on the NVKE Service Kubernetes APIs.

### Replace the legacy REST API and transition its consumers

The PostgreSQL-backed NVKE controller REST API will be removed from the target
architecture. The NVKE API will expose the Service layer's resource model, and
existing consumers must transition to that new contract. This includes
`nvkectl` commands and external automation that currently use the legacy API.
Operator tools may also use documented Kubernetes APIs at the NVKE Service or
NVKE Core layers to support adoption at those lower layers.

Kubernetes resources in NVKE Service and NVKE Core will become the authoritative
model for cluster management. The transition must account for existing clusters
and their management state, client migration, and safe transfer of reconciliation
ownership. Resource mappings, sequencing, and any temporary compatibility
mechanisms belong in a separate transition design, with retirement of the old
API as the end state.

Initial providers may act as shims over existing NVKE controller orchestration
while satisfying CAPI's lifecycle and status contracts. Temporal and PostgreSQL
may remain temporary provider implementation details during that transition.
Our long-term goal is Kubernetes-native controllers whose desired state and
reconciliation progress live in Kubernetes resources, without Temporal or
PostgreSQL for cluster lifecycle orchestration.

## Consequences

NVKE can offer a coherent managed service while making its underlying lifecycle
capabilities independently useful. Adopters can use the complete product,
integrate through the Service layer's Kubernetes APIs, or use Core providers and
extensions directly. Those adopting lower layers take responsibility for the
curation or customer-facing behavior supplied by the layers they omit.

New feature designs should explicitly consider adopters that use NVKE Core
without NVKE Service. Reusable cluster lifecycle capabilities should live
in Core where appropriate, with the Service layer adding operator curation and
service policy. Feature placement should be an explicit design decision, so
functionality useful to Core adopters does not acquire an unnecessary dependency
on NVKE Service.

Publishing finer-grained components makes their interfaces compatibility
commitments. A monolith can change private interfaces and their callers together;
independently consumed components must preserve supported contracts or provide
versioning, deprecation, and migration paths. We take on documentation,
compatibility testing, and release coordination for resource schemas, behavior,
status, and lifecycle semantics across supported component versions. This costs
more than evolving the same boundaries privately inside a monolith and can slow
changes that cross those boundaries. We accept that cost to gain modularity,
independent adoption, and the ability to evolve implementations behind clear
interfaces.

Supported compositions require validation beyond CAPI contract compliance.
NVKE Service's support commitments apply to those compositions; reusable
provider interfaces do not imply support for arbitrary provider combinations.
Shims reduce the initial rewrite but retain operational dependencies until
replaced. API and ownership migration add transition work needed to preserve
management of existing clusters and avoid competing reconcilers.

## Options considered

- **Keep CAPI limited to infrastructure:** preserves existing orchestration but
  leaves control plane and bootstrap lifecycle outside the common provider model
  and the managed service coupled to the legacy REST API.
- **Keep the stack monolithic:** permits coordinated changes to private
  interfaces, but prevents the independent adoption and composition we want.
- **Complete the Kubernetes-native rewrite before exposing providers:** avoids
  transitional adapters but couples adoption of the contracts to replacing all
  existing orchestration.
