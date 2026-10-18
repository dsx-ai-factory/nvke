# Day 0 → Day 1 → Day 2 Lifecycle

NVKE operations are organized into three phases that match what you're doing to the infrastructure: building it, configuring it, and running it.

```
Day 0            Day 1                      Day 2
──────────────   ────────────────────────   ─────────────────────────────
Bootstrap the    Install the site stack     Operate tenant clusters
management cluster and first tenant cluster   and the platform
cluster
```

## Day 0 — Bootstrap

**Goal:** Turn a set of bare, prepared nodes into a running Kubernetes cluster that will host the management plane.

`nvkectl bootstrap site` does this. It creates a temporary Kind cluster on the init node, uses it to drive `kubeadm init` and `kubeadm join` on your nodes through `nvke-agent` gRPC, and tears the temporary cluster down when done. You are left with:

- A Kubernetes cluster (`<sitename>.kubeconfig`)
- An etcd encryption key (`<sitename>-etcd-encryption-key.json`) — move this off the bootstrap host immediately

The cluster at this point is a plain kubeadm cluster. No NVKE services are installed.

**What Day 0 requires:**
- Ubuntu 24.04 or 26.04 nodes with `nvke-agent` running (see [Pre-Provisioned Nodes](site-bootstrap/pre-provisioned-nodes.md) or [Custom Base OS](site-bootstrap/custom-base-os.md))
- `nvkectl`, `nerdctl`, `kind` on the init node
- Network reach from the init node to all nodes on port 15005

**See:** [Management cluster bootstrap](site-bootstrap/overview.md) · [Your First Cluster — Real Site](first-cluster.md#real-site)

## Day 1 — Configuration

**Goal:** Turn the bare cluster into a self-managing NVKE site that can mint tenant clusters.

This phase installs everything that makes the cluster an NVKE site:

1. **Deploy the NVKE component stack** — the controller, Temporal, PostgreSQL, SPIRE, NVKE telemetry pipelines (NKTP) and CAPNICo, as the `deploy/nico` deploy path lists them. `deploy/nvke-deploy.sh` installs it from the site spec and the stack config, and from an NVKR release description when you install from a release, one step of a short list of commands; your GitOps tooling can deliver the same items. See [Install management-plane services](operate/install-management-plane-services.md) and [Deploy Path](site-bootstrap/deploy-repository.md).
2. **Configure CAPNICo** — the deploy path installed it in step 1; connect it to your NICo site with the `nico-credentials` Secret the same step rendered. Run `nvkectl get providers` to confirm `nico: CONFIGURED`.
3. **Ingest the site into NVKE** — the NVKE team registers the site and applies the etcd encryption key from Day 0. This step requires coordination with the NVKE team; contact your NVKE onboarding contact with your site's kubeconfig and the etcd encryption key file from Day 0.
4. **Create your first tenant cluster** — `nvkectl create cluster -f cluster.yaml`. See [Your First Cluster — Path C, Step 8](first-cluster.md#8-create-a-tenant-cluster).

**What Day 1 requires:**
- A working NICo site with capacity allocated for the instance types your cluster spec uses
- The site spec from Day 0, the stack config that holds the management plane's inputs, and the NVKR release description if you install from a release
- The etcd encryption key from Day 0

**See:** [Your First Cluster](first-cluster.md) · [NICo documentation](https://docs.nvidia.com/infra-controller/documentation/home)

## Day 2 — Operations

**Goal:** Keep the platform running, deliver Kubernetes clusters to tenants, and stay current.

Once the site is ingested, day-2 operations go through NVKE:

| Operation | How |
|---|---|
| Create a tenant cluster | `nvkectl create cluster -f spec.yaml` |
| List tenant clusters | `nvkectl get clusters` |
| Get a tenant kubeconfig | `nvkectl get kubeconfig <name>` |
| Add a node pool | `nvkectl create nodepool -f spec.yaml` |
| Scale a node pool | `nvkectl patch nodepool <name> --cluster <cluster> --patch 'desiredNodeCount: N'` |
| Upgrade a cluster's Kubernetes version | `nvkectl patch cluster <name> --patch 'controlPlaneKubernetesVersion: "1.34.0"'` |
| Delete a tenant cluster | `nvkectl delete cluster <name>` |
| Check infrastructure provider status | `nvkectl get providers` |

Node health and break-fix policy are handled by [NVSentinel](https://github.com/NVIDIA/NVSentinel) or your own break-fix system — NVKE provides the lifecycle APIs; cordon, drain, and SLO policy are yours to enforce. See [Node Lifecycle](tenant-clusters/node-lifecycle.md).

**What Day 2 requires:**
- A healthy NICo site with available capacity
- The management plane stack running and healthy
- CAPNICo in `CONFIGURED` state (`nvkectl get providers`)
