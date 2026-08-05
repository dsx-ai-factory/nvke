# Tier-0 Bootstrap

## Overview

This guide walks you through deploying a self-managing tier-0 site controller cluster — the Kubernetes control plane that runs NKE's workflow engines, databases, and security services.

The process involves:

1. Preparing your [configuration](configuration.md) (ArgoCD repositories, secrets, cluster settings)
2. Provisioning nodes (via [cloud-init / Forge UI](custom-base-os.md) or [pre-provisioned nodes](pre-provisioned-nodes.md))
3. [Downloading and installing](installation.md) `tier0-bootstrap`, then running it on the init node to trigger bootstrap

**Total time**: Approximately 60–90 minutes after starting the bootstrap.

> **NKE team bringing up an NKE-operated Site Controller on Forge?**
> Use the `self-bootstrap` CLI guide instead. `self-bootstrap` wraps the steps on this page with Forge API instance creation and SSH-driven provisioning. External users should continue with this guide.

## What Happens During Bootstrap

```mermaid
sequenceDiagram
    participant Node as Bootstrap Node
    participant Kind as Ephemeral Cluster
    participant Tier0 as Tier-0 Cluster
    participant Argo as ArgoCD
    participant Git as Git Repos

    Node->>Node: Load config (YAML + env vars + CLI flags)
    Node->>Kind: Create ephemeral bootstrap cluster
    Kind->>Tier0: Provision tier-0 cluster via nkecli
    Node->>Node: Setup local storage
    Kind->>Tier0: Install ArgoCD
    Kind->>Git: Clone deploy repos, render templates, discover items
    Git-->>Tier0: Apply deploy items (templates, manifests, ArgoCD apps)
    Tier0->>Argo: Sync all applications
    Kind->>Tier0: Migrate bootstrap data to production DB
    Tier0->>Tier0: Cluster is now self-managing
```

## Configuration Flow

`tier0-bootstrap` accepts configuration from multiple sources with clear precedence:

```mermaid
flowchart LR
    A[YAML Config File] --> D[Merged Config]
    B[Environment Variables] --> D
    C[CLI Flags] --> D
    D --> E[Clone deploy repos]
    E --> F[Process deploy items in order]
    F --> G[template: render .yaml.tmpl & apply]
    F --> H[manifest: kubectl apply]
    F --> I[argocd: create ArgoCD Application]
```

See [Configuration](configuration.md) for details on setting up repositories, secrets, and deploy manifests.

## Next Steps

- [Installation](installation.md) — download and install `tier0-bootstrap`
- [Configuration](configuration.md) — full config file reference
- [Deploy Repository](deploy-repository.md) — structure your ArgoCD deploy repo
- [Pre-Provisioned Nodes](pre-provisioned-nodes.md) — bootstrap on nodes you've already set up
- [Custom Base OS](custom-base-os.md) — bootstrap on vanilla Ubuntu via cloud-init
