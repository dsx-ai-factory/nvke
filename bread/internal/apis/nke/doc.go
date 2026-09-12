// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nke is the hub (internal) version of the nke API group: the tenant kinds NKECluster,
// NodePool and NKENode, each with every field the controller needs.
//
// # One package, one access matrix
//
// This package is the single source of truth. Each field carries, per role, the access that role
// has to it; the roles are not nested views of one another.
//
//   - `+nke:tenant=rw` / `+nke:tenant=r`: on the tenant API, writable or read-only for tenants.
//   - `+nke:operator=rw` / `+nke:operator=r`: on the operator API, writable or read-only for
//     operators.
//   - no marker: controller-only. Stored on the internal projection, never served to either role.
//
// A field with no marker, or one under `status.internal`, earns its place only when the
// controller cannot recompute it from spec, status, the object's owner references and one read
// of the backend. Which cluster a pool is bound to is an owner reference (uid included, so a
// recreated cluster under the same name is never adopted), not a field; which revision the nodes
// are at is read from the backend each reconcile and reported in status, not cached. The one
// hidden field left is the NKENode replacement chain.
//
// Two rules hold across the three kinds. Every tenant-owned spec field is operator read-only:
// operators see tenant intent (UC-004, UC-006) but do not edit it in their own right. When an
// operator must act on a tenant's behalf ("admin level access" in the SRD open items) they
// impersonate the tenant through the tenant API, so the write is validated by tenant rules and
// audited as operator-on-behalf-of-tenant. And no field is writable by both roles at once, which
// is what keeps every write attributable.
//
// Access is enforced twice. A field absent from a role's projection is refused by strict decoding
// (a 400). A field present but read-only for that role is refused by admission: the serving side
// rejects a write from a principal of role R that changes a field R may only read. The generator
// that emits the two projections and the admission rules from these markers is a separate change.
//
// # Operator intent
//
// Operators do not write status. Status is the controller's account of the world; an operator
// request (take this machine for repair, re-run validation, resize or upgrade a control plane) is
// intent, and it lives outside these three kinds. The design is in the air; the suggestion on the
// table (bread/docs/PROPOSAL-OPERATOR-WRITES.md on feat/bread) is: an imperative verb on the
// operator API where the operation is one call (`POST nkenodes/{n}/repair`, `DELETE` to
// withdraw), backed by an operator-owned stored record so the request has a lifecycle, listable
// across orgs without walking namespaces; a `ValidationRun` record for re-runs, carrying the
// per-check report; and an operator-owned control-plane kind for UC-009. The controller mirrors
// outcomes into the tenant-visible status here (`NKENode.status.repairRequest`, the `*Validated`
// conditions). Nothing in this package depends on which of those lands; no field here carries
// `+nke:operator=rw`.
//
// # What is a field and what is a placeholder
//
// A live field is one the SRD (NKE DSX Managed K8s, PLC-L1) calls for and NKE implements today,
// or that the Kubernetes API convention requires (conditions, observedGeneration). Everything
// else the SRD calls for but NKE does not yet implement is a PLACEHOLDER: a commented-out field
// carrying the SRD identifier (UC-nnn, NKE-DS9-nnn). Placeholders record a loosely held shape so
// the name is not invented twice; each is revisited with its implementation and needs no
// discussion before then. There is no autoscaling, no per-node kubelet tuning and no tenant
// taints: none is in the SRD or in NKE.
//
// # How the live fields are actuated today
//
// NKE's current control-plane REST API is not the model for this shape; it is what this layer
// calls until the CAPI-backed path replaces it. The mapping is recorded so the implementer knows
// where each field lands, not so the shape follows it.
//
//	NKECluster.metadata.namespace            the org: Cluster.orgID
//	NKECluster.spec.networking               Cluster.network.podCIDR / serviceCIDR (create-only)
//	NKECluster.status.kubernetesVersion      ControlPlaneStatus.kubernetesVersion
//	NKECluster.status.endpoint               the kube-vip address (Cluster.controlPlaneVIP), port 6443
//	NKECluster.status.certificateAuthorityData
//	                                         the CA the tenantaccess controller publishes today
//	NKECluster.status.conditions Ready       provisioned / provisioning / deleting / controlPlaneUpgrading
//	NodePool.spec.clusterName                NodePool.clusterID
//	NodePool.spec.instanceType               NodePool.instanceTypeID (a name here, a UUID there)
//	NodePool.spec.machineImage.name          NodePool.osProfile
//	NodePool.spec.replicas                   NodePool.desiredNodeCount
//	NodePool.spec.template.metadata.labels   NodePool.nodeLabels
//	NodePool.spec.rollout...maxSurge         NodePool.maxSurge, recycleBudget
//	NodePool.status.upToDateReplicas         nodes whose nodeSetID == NodePool.currentNodeSetID
//	NKENode.spec.retire                      DELETE /nodes/{id} with machineHealthIssue.summary = reason
//	NKENode.status.conditions Ready          NodeConditions.kubernetesReady
//	NKENode.status.conditions Healthy        NodeConditions.nodeAgentReady and not markedForRepair
//	NKENode.status.conditions Retiring       NodeConditions.deleting or markedForRecycling
//	NKENode.status.internal.replaces / replacedBy
//	                                         auto-mode node recycling
//
// Cluster inputs NKE takes today that are deliberately not fields here: cniPlugin and cniVersion
// belong to the core-runtime recipe (NKE-DS9-007) and arrive with the Release; etcdEncrypted and
// enableOidc are mandated (NKE-DS9-014, NKE-DS9-003), so they are not toggles; controlPlaneConfig,
// teleportEnabled, sshKeyGroupIDs and the capi* fields are operator-plane (UC-009) or provider
// configuration; siteID, vpcID and controlPlaneVIP are machine-provider concerns one layer down
// (machine-provider requirement 22). Pool inputs not carried: systemReservedMemory,
// kubeReservedMemory, configProfile, diskSelectionStrategy (operator or recipe), and
// infinibandPartitionID / nvLinkLogicalPartitionID (UC-022, NKE-DS9-019, P1 topology). Node
// fields not carried: forgeInstanceID, forgeMachineID, ip and nodeSetID are provider identity
// the CAPI objects hold; this API links to them by owner reference, not by copying them.
//
// The credential the tenant uses against the cluster's API server is not minted by this API.
// Tenants authenticate to the workload cluster with OIDC (UC-014, NKE-DS9-003); status carries
// the endpoint and CA so a client can assemble a kubeconfig, and the issuer details are a
// placeholder until the identity design lands. A credentials subresource is not decided.
//
// +kubebuilder:object:generate=true
// +k8s:deepcopy-gen=package
// +groupName=nke
package nke

// GroupName is the API group this package is the hub version of. It is a dotless group: the
// tenant API server serves it itself and needs no CRD, so the dot rule for CRD groups does not
// apply; the stored shape uses internal.nke.nvidia.com.
const GroupName = "nke"
