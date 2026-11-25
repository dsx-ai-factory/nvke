// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// CAPK's API, named rather than imported because its Go module pulls in
// KubeVirt's own module tree.
const (
	KubeVirtInfraGroup      = "infrastructure.cluster.x-k8s.io"
	KubeVirtInfraAPIVersion = "v1alpha1"
	KubeVirtMachineKind     = "KubevirtMachine"
	KubeVirtClusterKind     = "KubevirtCluster"

	// KubeVirtMachineNameLabel and KubeVirtMachineNamespaceLabel are what CAPK
	// labels a VM's virt-launcher pod with.
	KubeVirtMachineNameLabel      = "capk.cluster.x-k8s.io/kubevirt-machine-name"
	KubeVirtMachineNamespaceLabel = "capk.cluster.x-k8s.io/kubevirt-machine-namespace"

	// NodeIDLabel is what the controller labels a VM's pod with, so a node
	// agent's Service and ClusterSPIFFEID can select one node's pod even when
	// the node name has been reused.
	NodeIDLabel = "nvke.nvidia.com/node-id"
)

// IsKubeVirtMachineRef reports whether a Machine's infrastructure is a CAPK
// KubevirtMachine, whose node is reached through a load balancer rather than
// at its own IP.
func IsKubeVirtMachineRef(ref clusterv1.ContractVersionedObjectReference) bool {
	return ref.APIGroup == KubeVirtInfraGroup && ref.Kind == KubeVirtMachineKind
}
