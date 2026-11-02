# Architecture overview

NVKE (NVIDIA Kubernetes Engine) orchestrates the lifecycle of Kubernetes clusters
on NVIDIA infrastructure. The repository is a Go workspace of thirteen modules. Three
of them run in production: the NVKE controller, deployed as a Helm chart,
the node agent, installed on each node as a Debian package and run by systemd,
and the Cluster API infrastructure provider, deployed into the same cluster as
the controller. The rest are a CLI, the generated API module, the API authentication library, the management engine's
CRD types, and test and development tooling.

```
              ┌──────────────────────────────────────────────┐
              │   site controller cluster                    │
              │                                              │
   client ───▶│  NVKE controller (controller/)                │
  REST/HTTP   │                                              │
              │   REST API ──▶ Postgres ──▶ outbox processor │
              │                  ▲                │          │
              │   reconcilers ───┘                │          │
              │   (controller-runtime)            ▼          │
              └───────────────────────────────┬──────────────┘
                          │                   │
                          │ gRPC, dialed by   │ starts workflows
                          │ the controller    ▼
                          │            ┌─────────────┐
                          │            │  Temporal   │
                          │            └──────┬──────┘
                          │                   │ Forge API
                          ▼                   ▼
              ┌──────────────────────┐  ┌─────────────┐
              │  nvke-agent (agent/)  │  │    Forge    │
              │  gRPC server on each │  │ (bare-metal │
              │  node, systemd unit  │  │  lifecycle) │
              └──────────────────────┘  └─────────────┘
```

## The thirteen modules

| Module | Binary | What it is |
|---|---|---|
| `controller/` | `controller` | The NVKE controller: REST API, Temporal workflows, Postgres, reconcilers. Source is `cmd/worker/` |
| `agent/` | `agent` | The node agent, a gRPC server that runs on every managed node |
| `cluster-api-provider-provisioned/` | `manager` | Cluster API infrastructure provider for hosts NVKE provisions before CAPI bootstraps them |
| `cli/` | `nvkectl` | The command line client |
| `controller/api/` | none | The OpenAPI spec and the generated server and client types |
| `controller/deploy/charts/` | none | The controller and postgresql Helm charts, embedded so `nvkectl` installs the charts of its own commit |
| `auth/` | none | The NVKE API authentication library, `auth/authn`, used by the controller |
| `controller/nico-sim/` | `nico-sim`, `nico-simctl` | Forge-compatible VM and Docker-container simulator used by tests and development |
| `controller/test/e2e/` | none | The end to end suite |
| `controller/hack/capd-reboot-controller/` | `capd-reboot-controller` | Test helper for CAPD reboot behaviour |
| `management-engine/` | none | The management engine, including NVKECluster and NodePool |
| `os-image/` | `nvke-os-image-check` | OS image checks |
| `release/` | `qualify` | Release qualification: what to qualify, what ran, and what is qualified. Builds nothing that ships |

`go.work` is the list of record. See [go-workspace.md](go-workspace.md) for how
the workspace fits together and how public dependencies are resolved.

## controller

| Component | Description |
|---|---|
| `api/` | OpenAPI 3.0 spec and the oapi-codegen server and client |
| `internal/server/` | HTTP handlers, authentication middleware |
| `internal/temporal/workflows/` | Cluster provision, teardown and reboot workflows |
| `internal/temporal/activities/` | The individual steps workflows call |
| `internal/temporal/outbox_processor.go` | Polls the outbox table and starts workflows |
| `internal/controllers/` | The controller-runtime reconcilers |
| `internal/db/` | Bun ORM and the PostgreSQL migrations |
| `profiles/` | OS image and Kubernetes config profiles, as YAML |

### How a cluster request becomes work

The API does not call Temporal. It writes a row, and something else picks it up.
That indirection is deliberate, and it is the part most people get wrong when
they first read the handlers.

1. A client calls `POST /api/clusters`.
2. The handler opens one database transaction and writes both the cluster record
   and a row in the `workflow_outbox` table, through
   `CreateWorkflowOutboxEntry` in `internal/server/clusters.go`. Either both land
   or neither does, so a committed cluster always has its work queued.
3. `OutboxProcessor` polls that table, starts the matching Temporal workflow,
   and marks the row processed (`internal/temporal/outbox_processor.go`).
4. The workflow creates Cluster API resources. The configured infrastructure
   provider provisions or adopts the nodes, then the controller bootstraps Kubernetes with
   kubeadm.
5. The controller dials `nvke-agent` on each node over gRPC to drive the node side of
   the bootstrap (`internal/agent/client.go`).
6. The cluster reaches `Ready` and the kubeconfig becomes available.

Workflow activities queue further work the same way, through
`CreateWorkflowOutboxEntry`, rather than starting workflows directly.

### The reconcilers

Alongside the workflow engine, the controller runs controller-runtime
reconcilers under `internal/controllers/`. They cover node pools, control plane
and node scheduling, certificate renewal, tenant access and secrets, TPM and
SPIFFE identity for CAPI machines, and Teleport tokens.

This is worth stating plainly, because the combination is unusual. One process
runs a REST API backed by Postgres, a Temporal worker, and a set of Kubernetes
reconcilers. Reading only the workflows, or only the reconcilers, gives an
incomplete picture of what reacts to a change.

### Configuration

The controller reads its settings once, at startup, from `nvke-controller-config`,
a ConfigMap that the Helm chart renders from its values and mounts at the path
`CONTROLLER_CONFIG_PATH` names. `internal/controllerconfig` loads and validates
it, and an invalid file stops the controller from starting rather than falling
back to defaults. The settings the file does not carry, such as the database
connection string and the API authentication file, come from environment
variables (`internal/config/config.go`). The chart refuses an `env` entry that
sets a variable the file replaced; `files/controller-config-env.yaml` in the
chart lists each one with the key that replaces it.

The `oidc` section configures how workload cluster API servers authenticate:
the shared provider the management plane uses (`oidc.managementOIDCAuth`) and
up to five fleet-wide providers (`oidc.providers`). The create handler copies
the fleet-wide list into the cluster record (`clusters.oidc_providers`), and
`kubeadm init`, later control-plane joins and the `enableOidc` rollout render
the API server's authentication configuration from that copy, never from the
live file. [Configure OIDC for workload clusters](workload-clusters/oidc.md)
describes both, and the identities the API server refuses from a fleet-wide
provider.

## nvke-agent

The agent runs as a systemd service on each managed node. It is a **gRPC
server**: it listens, and the controller dials it
(`agent/cmd/agent/main.go`, `controller/internal/agent/client.go`). It does not
call home, and the controller has no node-registration endpoint for it to call.

Responsibilities:

- Runs `kubeadm init` and `kubeadm join` on the node, and installs a CNI
- Bootstraps the kubelet
- Attests its identity through SPIRE, by TPM or join token
- Handles node lifecycle operations such as drain, reboot and teardown

`make build` in `agent/` cross-compiles for Linux on amd64 and arm64, because
that is where the agent runs.

## Deployment

The controller is deployed as a Helm chart
(`controller/deploy/charts/controller`) onto a *site controller*
cluster, a dedicated Kubernetes cluster that manages the NVKE tenant fleet for a
site.

The agent has no chart. It ships as a Debian package, installed on each node and
run by systemd.

The Cluster API infrastructure provider is a third deployed piece. It is a
`manager` binary with its own container image and kustomize configuration under
`cluster-api-provider-provisioned/config/`, and a `metadata.yaml` that makes it
installable by `clusterctl`. It owns the `ProvisionedCluster` and
`ProvisionedHostClaim` types, and it reconciles hosts that NVKE has already
provisioned so that Cluster API can bootstrap them.

### Machine agent connectivity

When creating or reapplying a Machine, NVKE publishes the node's persisted
transport mode in `nvke.nvidia.com/agent-connection`: `plaintext`, `spiffe`,
`spiffe-jwt`, `external-mtls`, or `external-mtls-jwt`. The host UUID is published
separately in `nvke.nvidia.com/host-id`. Operator-supplied Machine metadata cannot
override the connection annotation.

## Further reading

- `docs/go-workspace.md` — the workspace and dependency-management workflow
- `controller/docs/authenticating-to-the-api.md` — API authentication
- `docs/design/byo-os.md` — proposal: bring your own OS image
- `controller/AGENTS.md` — AI agent development guide for the controller
- `agent/docs/DESIGN.md` — node agent architecture and API surface
- `agent/docs/SECURITY.md` — node agent security context
- `agent/AGENTS.md` — AI agent development guide for the node agent
