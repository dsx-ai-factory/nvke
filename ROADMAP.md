# NVKE roadmap

This is what the NVIDIA Kubernetes Engine (NVKE) team is building next. The
roadmap shows direction, not commitments. Items can move between stages, change
scope or come off the list as we learn more. We don't publish dates.

| Stage | What it means |
|---|---|
| **Coming soon** | Being built now. Expect it in an upcoming release. |
| **Working on it** | Funded and in design or early development. |
| **Exploring** | On our list, but not yet scheduled. Your feedback shapes what happens next. |

## Coming soon

| Item | What it delivers |
|---|---|
| Authorize NVKE API calls per customer | Every API request is checked against the caller's tenancy, so callers reach only their own resources. |
| Manage the management cluster's own nodes through NVKE | After bootstrap, scale, upgrade and repair the management cluster through NVKE. |
| Issue cluster certificates from your own PKI | Cluster certificates chain to your own root of trust. |
| Support Kubernetes 1.37 | New clusters on 1.37, and upgrades from 1.36. |

## Working on it

| Item | What it delivers |
|---|---|
| Support NVIDIA GB300 worker nodes | Run worker nodes on NVIDIA GB300, validated with NVKE. |
| Support NVIDIA Vera Rubin worker nodes | Run worker nodes on NVIDIA Vera Rubin, validated with NVKE. |
| Separate control-plane and worker networks | Control-plane nodes run on their own network, isolated from workloads. |
| Delete individual nodes | Describe and delete a single node. |
| Scale down node pools automatically | Pools scale down automatically and respect PodDisruptionBudgets. You can also choose which nodes go on scale-down. |
| Manage workload cluster add-ons through the NVKE API | Install, upgrade and remove workload-cluster add-ons through the NVKE API. |

## Exploring

| Item | What it delivers |
|---|---|
| Operate NVKE from AI agents with MCP and agent skills | An NVKE MCP server and published agent skills let AI assistants inspect and operate clusters, under the same per-customer authorization as the API. |
| Patch nodes in place for urgent security fixes | Apply CVE fixes without replacing every node in a pool. |
| Install the management plane with your production deployment tools | A management plane install that fits the deployment systems you already run. |
| Federate workload identity with your platform's IAM | Pods assume platform roles with short-lived, audience-scoped tokens. |
| Back up and restore workload clusters and the management plane | Recover a cluster or the management plane from a backup. |
| Update labels and taints on existing node pools | Update labels and taints after a pool exists, not only when nodes join. |
| Upgrade the management plane to new NVKE releases | Move a running management plane to a newer NVKE release through a supported upgrade path. |
| Encrypt cluster secrets with your own KMS keys | Kubernetes Secrets encrypted with keys from your own KMS. |
| Validate workload clusters at upstream Kubernetes scale limits | NVKE clusters tested against upstream scalability thresholds, such as 5,000 nodes per cluster. |

## Tell us what you need

Open a [feature request](https://github.com/dsx-ai-factory/nvke/issues/new?template=feature_request.yml).

For node OS images, packages and components, see the
[NVKR roadmap](https://github.com/dsx-ai-factory/nvkr/blob/main/ROADMAP.md).
