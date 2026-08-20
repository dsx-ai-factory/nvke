# Architecture overview

nke (NVIDIA Kubernetes Engine) consists of two independently deployable
Go modules that together orchestrate the full lifecycle of Kubernetes
clusters on NVIDIA infrastructure.

```
┌─────────────────────────────────────────────────────┐
│                     nke monorepo                    │
│                                                     │
│  ┌──────────────────┐   ┌────────────────────────┐  │
│  │   controller/    │   │       agent/           │  │
│  │                  │   │                        │  │
│  │  Cloud Workflow  │   │  Runs on each node;    │  │
│  │  Engine (CWE)    │   │  registers w/ control  │  │
│  │                  │   │  plane, handles kubelet│  │
│  │  - REST API      │   │  bootstrap & SPIFFE    │  │
│  │  - Temporal WFs  │   │  attestation           │  │
│  │  - PostgreSQL DB │   │                        │  │
│  └────────┬─────────┘   └──────────┬─────────────┘  │
│           │                        │                │
└───────────┼────────────────────────┼────────────────┘
            │                        │
     ┌──────▼──────┐          ┌──────▼──────┐
     │   Temporal  │          │  Forge API  │
     │  (workflow  │          │  (bare-metal│
     │  engine)    │          │  lifecycle) │
     └─────────────┘          └─────────────┘
```

## controller (Cloud Workflow Engine)

| Component | Description |
|---|---|
| `api/` | OpenAPI 3.0 spec + oapi-codegen generated server/client |
| `internal/server/` | HTTP handlers, authentication middleware |
| `internal/temporal/workflows/` | Cluster provision/teardown/reboot workflows |
| `internal/temporal/activities/` | Individual steps called by workflows |
| `internal/db/` | Bun ORM + PostgreSQL migrations |
| `internal/providers/forge/` | Forge bare-metal API client (cached) |
| `profiles/` | OS image and Kubernetes config profiles (YAML) |
| `preprovisioned/` | Cluster API infrastructure provider for hosts that already exist (see [the design](../controller/docs/preprovisioned-capi-bootstrap.md)) |

Key data flows:

1. Client calls `POST /api/clusters` → CWE starts a `ProvisionCluster` Temporal workflow.
2. Workflow orchestrates Forge API calls to provision bare-metal nodes, then bootstraps Kubernetes via kubeadm.
3. `nke-agent` on each provisioned node registers itself with CWE over mTLS (SPIFFE/SPIRE).
4. CWE marks the cluster `Ready` and returns the kubeconfig.

## nke-agent

Runs as a systemd service on each managed node. Responsibilities:

- Bootstraps kubelet (downloads kubeconfig, starts kubelet)
- Attests its identity via SPIRE (TPM or join-token)
- Registers node readiness back to CWE
- Handles node lifecycle events (drain, reboot, teardown)

## Deployment

Both modules are deployed via Helm charts onto a *site controller* cluster —
a dedicated Kubernetes cluster that manages the NKE tenant fleet for a given
site. ArgoCD on the site controller keeps chart versions in sync with
`nke-site-deploy`.

## Further reading

- `controller/docs/remote-colossus-tilt.md` — local dev on remote hardware
- `controller/AGENTS.md` — AI agent development guide for CWE
- `agent/docs/DESIGN.md` — agent architecture and API surface
- `agent/docs/SECURITY.md` — agent security context
- `agent/AGENTS.md` — AI agent development guide for the node agent
