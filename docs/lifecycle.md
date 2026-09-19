# Day 0 → Day 1 → Day 2 Lifecycle

NKE operations are organized into three phases that match what you're doing to the infrastructure: building it, configuring it, and running it.

```
Day 0            Day 1                      Day 2
──────────────   ────────────────────────   ─────────────────────────────
Bootstrap the    Install the site stack     Operate tenant clusters
site controller  and first tenant cluster   and the platform
cluster
```

## Day 0 — Bootstrap

**Goal:** Turn a set of bare, prepared nodes into a running Kubernetes cluster that will host the site controller.

`nkectl bootstrap site` does this. It creates a temporary Kind cluster on the init node, uses it to drive `kubeadm init` and `kubeadm join` on your nodes through `nke-agent` gRPC, and tears the temporary cluster down when done. You are left with:

- A Kubernetes cluster (`<sitename>.kubeconfig`)
- An etcd encryption key (`<sitename>-etcd-encryption-key.json`) — move this off the bootstrap host immediately

The cluster at this point is a plain kubeadm cluster. No NKE services are installed.

**What Day 0 requires:**
- Ubuntu 24.04 or 26.04 nodes with `nke-agent` running (see [Pre-Provisioned Nodes](site-bootstrap/pre-provisioned-nodes.md) or [Custom Base OS](site-bootstrap/custom-base-os.md))
- `nkectl`, `nerdctl`, `kind` on the init node
- Network reach from the init node to all nodes on port 15005

**See:** [Site Controller Bootstrap](site-bootstrap/overview.md) · [Your First Cluster — Path C](first-cluster.md#path-c--a-real-site)

## Day 1 — Configuration

**Goal:** Turn the bare cluster into a self-managing NKE site that can mint tenant clusters.

This phase installs everything that makes the cluster an NKE site:

1. **Deploy the NKE component stack** — the NKE SBOM (Software Bill of Materials) is the versioned bundle of NKE components: the controller, Temporal, PostgreSQL, SPIRE, and supporting services. Deploy it via ArgoCD or your GitOps tooling. See [Deploy Repository](site-bootstrap/deploy-repository.md).
2. **Configure CAPNICo** — install the `cluster-api-provider-nico` chart and connect it to your NICo site with a `nico-credentials` Secret. Run `nkectl get providers` to confirm `nico: CONFIGURED`.
3. **Ingest the site into NKE** — the NKE team registers the site and applies the etcd encryption key from Day 0. This step requires coordination with the NKE team; contact your NKE onboarding contact with your site's kubeconfig and the etcd encryption key file from Day 0.
4. **Create your first tenant cluster** — `nkectl create cluster -f cluster.yaml`. See [Your First Cluster — Path C, Step 7](first-cluster.md#7-create-a-tenant-cluster).

**What Day 1 requires:**
- A working NICo site with capacity allocated for the instance types your cluster spec uses
- An ArgoCD-compatible deploy repository with your environment's SBOM values
- The etcd encryption key from Day 0

**See:** [Your First Cluster](first-cluster.md) · [NICo documentation](https://docs.nvidia.com/infra-controller/documentation/home)

## Day 2 — Operations

**Goal:** Keep the platform running, deliver Kubernetes clusters to tenants, and stay current.

Once the site is ingested, day-2 operations go through NKE:

| Operation | How |
|---|---|
| Create a tenant cluster | `nkectl create cluster -f spec.yaml` |
| List tenant clusters | `nkectl get clusters` |
| Get a tenant kubeconfig | `nkectl get kubeconfig <name>` |
| Add a node pool | `nkectl create nodepool -f spec.yaml` |
| Scale a node pool | `nkectl patch nodepool <name> --cluster <cluster> --patch 'desiredNodeCount: N'` |
| Upgrade a cluster's Kubernetes version | `nkectl patch cluster <name> --patch 'controlPlaneKubernetesVersion: "1.34.0"'` |
| Delete a tenant cluster | `nkectl delete cluster <name>` |
| Check infrastructure provider status | `nkectl get providers` |

Node health and break-fix policy are handled by [NVSentinel](https://github.com/NVIDIA/NVSentinel) or your own break-fix system — NKE provides the lifecycle APIs; cordon, drain, and SLO policy are yours to enforce. See [Node Lifecycle](tenant-clusters/node-lifecycle.md).

**What Day 2 requires:**
- A healthy NICo site with available capacity
- The site controller stack running and healthy
- CAPNICo in `CONFIGURED` state (`nkectl get providers`)
