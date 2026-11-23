# Adopt the site cluster the NVKE controller runs on

**Status:** proposed
**Date:** 2026-11-23

## Context

`nvkectl bootstrap site` builds a site cluster from a temporary seed cluster.
The NVIDIA Kubernetes Engine (NVKE) controller in the seed cluster creates the
site cluster through its own REST API, so the only NVKE record of the site
cluster is in the seed cluster. Once the seed cluster is deleted, the site
cluster record is gone with it as well. An NVKE controller installed on the site cluster
therefore cannot manage anything on the cluster it runs on, not even a worker
node pool.

The NVKE controller provisions control-plane machines
through an infrastructure provider and runs `kubeadm init`. It changes a control
plane by replacing nodes, for upgrades, rollouts, rebalancing and remediation.
On a site cluster that runs NVIDIA Infra Controller (NICo), a
replacement control-plane node would come from the NICo running on that same
cluster, so the control plane would depend on a service it hosts. NICo machines
cannot yet boot without the NICo DHCP and API services
([dsx-ai-factory/infra-controller#7568](https://github.com/dsx-ai-factory/infra-controller/issues/7568)).

## Decision

We will add `POST /clusters/adopt` to the NVKE controller behind `clusterAdoptionEnabled` feature flag, which is off by default.
It records the cluster the controller runs on as an NVKE cluster, without running
`kubeadm`. It only supports adopting itself: the controller reads it with its
own service account and RBAC permissions, and adopts at most one cluster.

The request carries what NVKE can not read from the cluster:

```json
{
  "name": "mgmt",
  "nico": {"siteID": "<site ID>", "vpcID": "<VPC ID>", "orgID": "<org ID>"},
  "cniPlugin": "ovn-kubernetes",
  "etcdEncrypted": true
}
```

| Field | Meaning |
| --- | --- |
| `name` | Required. The cluster's name in NVKE. |
| `nico.siteID`, `nico.vpcID`, `nico.orgID` | Required. The NICo site and VPC that NVKE creates worker machines in. Workers in that VPC must reach the cluster's API server. |
| `cniPlugin`, `cniVersion` | Optional. Default `ovn-kubernetes` and its default version. They must match the CNI the cluster runs: each worker installs that CNI's packages, and on OVN-Kubernetes NVKE configures Open vSwitch and the OVN zone label on it. |
| `etcdEncrypted` | Required. Whether the cluster encrypts Secrets in etcd. |

The controller reads everything else from the cluster during the request:

- the control-plane nodes, which are the Nodes with the control-plane role
  label. The control plane `desiredSize` is deduced from this as well.
- from `kube-system/kubeadm-config`, the Kubernetes version and the pod and
  service CIDRs.

The request doesn't contain control-plane machine settings because NVKE
doesn't build control-plane machines for an adopted cluster. The controller's
access to the cluster through its service account is read-only. 
The response is the `Cluster`, with status 201 on success.

The endpoint returns a 400 error when:

- the cluster has no control-plane node, or a control-plane node has no
  InternalIP
- `kubeadm-config` has no pod CIDR, which kubeadm allows when the CNI assigns
  pod addresses itself
- a cluster is already adopted
- a Cluster API `Cluster` of the same name exists.

It returns 404 while `clusterAdoptionEnabled` is false.

The controller records the cluster and its control-plane nodes atomically.
The provisioning workflow then creates the cluster's Cluster API `Cluster`
and infrastructure cluster. Adopted control-plane nodes don't get a CAPI `Machine`. 
As on any cluster it provisions, the workflow also installs node-local DNS and 
labels the system namespaces for Pod Security Admission, and it retries each step until it
succeeds. Those two steps reach the cluster through the adopted nodes' agents,
so the cluster stays provisioning until the operator has moved the nodes' SPIRE
agents.

The operator keeps the adopted control plane. NVKE will not upgrade, scale,
replace, roll out, remediate or delete adopted control-plane nodes, or change
them in place. NVKE enforces this in three ways:

- **At the API.** `PATCH` refuses control-plane changes, and `DELETE` refuses
  the cluster and its adopted nodes.
- **With existing settings.** Adopt turns off control-plane recycling, automatic
  scale-down and node rebalancing (`enableCPNodeRecycling`,
  `autoScaleDownDisabled` and `nodeRebalanceDisabled`), and the API refuses to
  turn them back on.
- **At the source of remediation.** NVKE does not copy a recycling request from
  an adopted node's Kubernetes Node
  ([nodestatus/conditions.go](https://github.com/dsx-ai-factory/nvke/blob/22574416f4a3c3aaaf4cb6c877fd142d869e4f4e/controller/internal/controllers/tenant/nodestatus/conditions.go#L19-L22)),
  so it never marks an adopted node for recycling.

NVKE still reaches adopted nodes through their agents:

- to read the cluster's kubeconfig, which it publishes and uses as its own
  client for the cluster
- to mint join tokens, which also re-uploads the control-plane certificates for
  kubeadm
- to upgrade nvke-agent.

Once a cluster is adopted, NVKE can create and manage worker node pools on it as on any other cluster.

## Consequences

A site cluster gets an NVKE record without being rebuilt, whether it is
new or its seed cluster is already gone. Worker capacity comes from NICo node
pools, so a NICo outage stalls worker provisioning but not the control plane.

An adopted cluster is a partial NVKE cluster:

- Control-plane upgrades and maintenance stay with the operator's own tooling.
- The cluster and its adopted nodes cannot be deleted through the API. To undo a
  wrong adoption, such as one with the wrong NICo site or VPC, which no cluster
  can change, a follow-up lets `DELETE` remove only NVKE's records and leave the
  cluster running. The operator then adopts again.
- NVKE cannot reach adopted nodes until the operator moves each node's SPIRE
  agent to the site cluster's SPIRE server. For each node, one at a time,
  the operator:
  1. points the node's SPIRE agent at that server, with its trust bundle
  2. applies a `ClusterStaticEntry` on the site cluster for nvke-agent, with the
     SPIFFE ID `spiffe://<trust domain>/host/<node ID>`, where the node ID is
     the one NVKE assigned at adoption, parented to the node's SPIRE agent. NVKE
     creates the same object from the `Machine` for nodes it builds
     ([machine_spire_entry_controller.go](https://github.com/dsx-ai-factory/nvke/blob/22574416f4a3c3aaaf4cb6c877fd142d869e4f4e/controller/internal/controllers/capi/machine_spire_entry_controller.go#L259)),
     but an adopted node has no `Machine`
  3. restarts nvke-agent, which reads its identity at startup.

## An adopted cluster in NVKE and Cluster API

Adoption creates these records and objects:

| Component | NVKE | Cluster API |
| --- | --- | --- |
| Cluster | A cluster record, marked adopted | A `Cluster` and its `NicoCluster` |
| Control-plane nodes | One node record per control-plane node, marked adopted | None |
| Worker node pools | Node pool and node records, as for any cluster | A `Machine` and `NicoMachine` for each worker NVKE provisions |

Compared with a cluster that NVKE creates:

| | Regular cluster | Adopted cluster |
| --- | --- | --- |
| Created by | NVKE, through `POST /clusters`, which provisions control-plane machines and runs `kubeadm init` | The operator. `POST /clusters/adopt` records it |
| Kubernetes upgrades | NVKE, through `PATCH /clusters/{clusterID}` with `controlPlaneKubernetesVersion`, which replaces control-plane nodes and runs `kubeadm upgrade apply` for the add-ons | The operator, outside NVKE. `PATCH` refuses `controlPlaneKubernetesVersion` |
| Control-plane size | Changed with `PATCH /clusters/{clusterID}` | Fixed at adoption. `PATCH` refuses a change |
| Certificate renewal, etcd key rotation, enabling etcd encryption or OIDC | NVKE | The operator |
| Control-plane node replacement, rollout, rebalancing and remediation | NVKE | None. The operator repairs its nodes |
| Recorded Kubernetes version | Follows NVKE upgrades | Stays at its value at adoption |
| Deletion | `DELETE /clusters/{clusterID}` tears it down | Refused |
| Worker node pools | NVKE | NVKE |

## Future work: full self-management

Adoption is the first step toward NVKE managing the site cluster's control
plane like any other cluster's. That needs NICo machines that can run the
control plane of the site hosting NICo, which in turn needs them to boot without
the NICo DHCP and API services
([dsx-ai-factory/infra-controller#7568](https://github.com/dsx-ai-factory/infra-controller/issues/7568)).
Once that works, NVKE could replace the adopted control-plane nodes with NICo
ones and clear the cluster's adopted marker. That design is a separate ADR, to
be written after the infra-controller change. It covers lifting the
adopted-cluster guards, managing etcd membership, and the operator supplying
the cluster's current etcd encryption key in `etcd-encryption-key-<cluster ID>`.

## Options considered

- **Adopt the running cluster and leave its control plane to the operator
  (chosen):** gives the site cluster an NVKE record and worker node pools
  without making its control plane depend on the NICo it hosts.
- **Read the cluster through a kubeconfig the operator supplies:** could adopt
  any cluster, but adoption only ever targets the controller's own cluster. It
  would also accept a kubeconfig for another cluster, and the operator would
  have to handle an admin credential.
- **Pivot the seed cluster's records to the site cluster:** keeps the full
  NVKE model, but needs the seed cluster to still exist, and NVKE would then
  replace control-plane nodes through the NICo on the same cluster.
- **Adopt the cluster and upgrade its control plane in place through
  nvke-agent:** gives NVKE control-plane upgrades without replacing nodes, but
  adds a second upgrade path for a single cluster, while operators already
  upgrade these nodes with their own tooling.
