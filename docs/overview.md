# Overview

## The problem

AI infrastructure operators need to offer their tenants self-service Kubernetes — but standing up a Kubernetes cluster that works with NVIDIA GPUs, NVLink, and the full accelerated runtime stack is not a solved problem you can hand off to upstream docs. The operator has to know which Kubernetes version to run, which CNI, which GPU Operator version, which kernel and OS configuration, and how to keep all of it patched as CVEs land. Getting that wrong means a cluster that NVKE won't accept, or one that breaks when NVIDIA ships an update.

Before NVKE, operators either spent months reverse-engineering the correct configuration from upstream, or inherited a fragile set of manual steps from a previous deployment that nobody fully understood.

## What NVKE does

NVKE is a managed Kubernetes engine for AI Factories. It has two parts:

**NVKE — the engine.** A management plane that drives the full lifecycle of Kubernetes clusters: provisioning nodes, bootstrapping Kubernetes via `kubeadm`, rolling upgrades, scaling, and teardown. Operators interact with NVKE through a REST API and web console; tenants get self-service GPU clusters without touching the underlying infrastructure. NVKE handles the operational complexity so the operator's team doesn't have to.

**NKD — the definition.** A validated recipe that specifies exactly which Kubernetes version, packages, and configuration NVKE was built and tested against, plus the pipeline to reproduce that build in the operator's own registry. The operator runs the pipeline. NVIDIA owns the specification. When a CVE lands or a new GPU Operator pairing is qualified, NVIDIA publishes a new recipe and the operator re-runs the pipeline — days to update, not weeks.

## Who it's for

NVKE is for **AI factory operators** — the teams running GPU infrastructure who need to offer managed Kubernetes to their tenants. Tenants might be internal business units or external paying customers. The operator's job is to keep the infrastructure running, secure, and current; NVKE is the engine that makes that job tractable at scale.

NVKE is also the Kubernetes substrate that [DSX](https://www.nvidia.com/en-us/data-center/dsx/) runs on internally. The same engine and recipe that an external NCP deploys is what NVIDIA uses to run its own GPU fleet.

## What you build

After deploying NVKE, an operator has:

- A **site controller** — a small, HA Kubernetes cluster that runs NVKE's workflow engines, databases, and identity services
- The ability to **provision tenant clusters** on demand via API or UI, each running the validated Kubernetes stack from the NKD recipe
- A **repeatable path to stay current** — new node images from updated recipes, rolled out node by node without disrupting running workloads

---

Ready to go deeper? [Key Concepts](concepts.md) explains the tools and systems you'll encounter. [Architecture](architecture.md) shows how the components fit together.
