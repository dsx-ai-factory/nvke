# Key Concepts

NVKE documentation assumes familiarity with Kubernetes and its ecosystem. This page is a quick orientation — not a tutorial — covering the concepts and tools you'll encounter throughout these docs. Each section links to upstream resources if you want to go deeper.

If you already know Kubernetes, ArgoCD, and Helm, skip this page.

---

## Kubernetes

Kubernetes (K8s) is the orchestration system NVKE is built on. It schedules containerized workloads across a fleet of machines, handles failures, and provides a consistent API for managing compute, networking, and storage.

In the NVKE context, there are two kinds of Kubernetes clusters:

- **Site controller cluster** — a small, HA three-node K8s cluster that runs NVKE's own management services (workflow engines, databases, security). Operators manage this cluster; tenants don't see it.
- **Tenant clusters** — GPU-accelerated K8s clusters provisioned by NVKE for tenants to run AI workloads on.

**Learn more:** https://kubernetes.io/docs/concepts/overview/

---

## kubeadm

`kubeadm` is the upstream tool for bootstrapping Kubernetes clusters — it initializes the control plane, generates certificates, and joins nodes. NVKE uses `kubeadm` under the hood via `nvke-agent` to bring up both the site controller and tenant clusters.

You don't run `kubeadm` directly. NVKE runs it for you. But understanding what it does helps when reading bootstrap logs or troubleshooting a failed node join.

**Learn more:** https://kubernetes.io/docs/reference/setup-tools/kubeadm/

---

## Helm

Helm is the package manager for Kubernetes. Applications are packaged as *charts* — versioned bundles of Kubernetes manifests with templated configuration. NVKE's own services (CWE, Temporal, CloudNative-PG) are deployed as Helm charts from the NGC Helm registry.

After `nvkectl` bootstraps the cluster, you deploy NVKE's services onto it using ArgoCD Application manifests that point at Helm charts and supply values.

**Learn more:** https://helm.sh/docs/intro/using_helm/

---

## ArgoCD

ArgoCD is a GitOps continuous delivery tool for Kubernetes. It watches a git repository for Kubernetes manifests and keeps the cluster in sync with what's declared there. When you push a change to your deploy repository, ArgoCD detects the drift and reconciles the cluster.

NVKE uses ArgoCD to manage the site controller stack. After `nvkectl` bootstraps the cluster, you install ArgoCD and apply your deploy repository to bring up NVKE's services. Once deployed, ArgoCD owns ongoing reconciliation — you don't apply manifests by hand.

**Learn more:** https://argo-cd.readthedocs.io/en/stable/core_concepts/

---

## Temporal

Temporal is a durable workflow orchestration platform. It executes long-running, fault-tolerant workflows — like "provision a three-node cluster" or "upgrade 200 worker nodes one at a time" — and automatically retries failed steps without losing progress.

NVKE's Cloud Workflow Engine (CWE) is a Temporal worker. When you provision a tenant cluster through NVKE, CWE submits a workflow to Temporal that drives the full lifecycle. If a step fails mid-way, Temporal resumes from that point rather than starting over.

Operators can port-forward Temporal's web UI to inspect running workflows or diagnose failures.

**Learn more:** https://docs.temporal.io/concepts/what-is-temporal/

---

## CloudNative-PG (CNPG)

CloudNative-PG is a Kubernetes operator for running PostgreSQL. NVKE deploys CNPG clusters on the site controller for CWE and Temporal rather than running standalone database processes. CNPG handles HA failover, backup, and certificate management.

Database credentials are configured in your ArgoCD application manifests during the post-bootstrap deployment step. CNPG creates and manages the actual PostgreSQL instances.

**Learn more:** https://cloudnative-pg.io/documentation/

---

## Cluster API (CAPI)

Cluster API is a Kubernetes sub-project that provides declarative APIs for provisioning and managing Kubernetes clusters. Instead of running scripts to create a cluster, you create Kubernetes custom resources (`Cluster`, `Machine`, `MachineDeployment`) and a controller reconciles the desired state.

NVKE's primary CAPI infrastructure provider for production deployments is [`cluster-api-provider-nico`](https://github.com/dsx-ai-factory/cluster-api-provider-nico) (CAPNICo), which integrates with [NVIDIA Infra Controller (NICo)](https://github.com/dsx-ai-factory/infra-controller) to allocate and manage bare-metal machines. A working NICo site is a prerequisite for NVKE day-2 operations — tenant cluster creation, node add/remove, and upgrades all go through NICo.

**Learn more (CAPI):** https://cluster-api.sigs.k8s.io/introduction

**NICo documentation:** https://docs.nvidia.com/infra-controller/documentation/home

---

## SPIFFE / SPIRE

SPIFFE (Secure Production Identity Framework for Everyone) is a standard for issuing cryptographic identities to workloads. SPIRE is its reference implementation.

In NVKE, every node and every workload gets a SPIFFE identity backed by TPM attestation. Workflow engines and `nvke-agent` use these identities to mutually authenticate over mTLS — no static credentials are passed between components. When you see "SPIFFE mTLS" in these docs, it means the two endpoints have verified each other's hardware-rooted identity before exchanging data.

**Learn more:** https://spiffe.io/docs/latest/spiffe-about/overview/

---

## GitOps and the Deploy Repository

GitOps is the practice of using a git repository as the single source of truth for infrastructure state. You declare what you want in git; a controller (ArgoCD in NVKE's case) makes the cluster match it.

NVKE requires a **deploy repository** — a git repo you own and maintain, structured so ArgoCD can discover and apply its contents. After `nvkectl` bootstraps the cluster, you install ArgoCD and point it at the deploy repository to bring up the NVKE services stack. ArgoCD then owns ongoing reconciliation. See [Deploy Repository](site-bootstrap/deploy-repository.md) for the structure.

---

## What You Don't Need to Know

NVKE handles the following for you — you don't need deep expertise in these to operate NVKE:

- **kubeadm internals** — NVKE drives these via `nvke-agent`
- **Certificate lifecycle** — managed by `cert-manager` and `nvke-agent`
- **etcd operations** — managed by NVKE with built-in encryption and `etcd-manager` for compaction and snapshots
- **CNI configuration** — NVKE configures OVN-Kubernetes during site-controller bootstrap

---

Ready to continue? Start with [Architecture](architecture.md) for the component overview, or go straight to [Site Controller Bootstrap](site-bootstrap/overview.md) to stand up a site controller.
