// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"errors"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	kubeVirtInfraGroup            = "infrastructure.cluster.x-k8s.io"
	kubeVirtMachineKind           = "KubevirtMachine"
	kubeVirtMachineTemplateKind   = "KubevirtMachineTemplate"
	kubeVirtClusterKind           = "KubevirtCluster"
	kubeVirtMachineNameLabel      = "capk.cluster.x-k8s.io/kubevirt-machine-name"
	kubeVirtMachineNamespaceLabel = "capk.cluster.x-k8s.io/kubevirt-machine-namespace"
)

var errKubeVirtRequiresTLSJWT = errors.New("KubeVirt VM nodes with SPIFFE or TLS/JWT require TLS/JWT with external mTLS: " +
	"a VM runs no SPIRE agent")

// IsKubeVirtMachineRef reports whether a Machine's infrastructure is a CAPK
// KubevirtMachine.
func IsKubeVirtMachineRef(ref clusterv1.ContractVersionedObjectReference) bool {
	return ref.APIGroup == kubeVirtInfraGroup && ref.Kind == kubeVirtMachineKind
}

func isKubeVirtObject(obj *unstructured.Unstructured, kind string) bool {
	gv, err := schema.ParseGroupVersion(obj.GetAPIVersion())
	return err == nil && gv.Group == kubeVirtInfraGroup && obj.GetKind() == kind
}

// isKubeVirtNodePool reports whether nodePool's machine template is a
// KubevirtMachineTemplate.
func isKubeVirtNodePool(nodePool db.NodePool) bool {
	if nodePool.Capi == nil {
		return false
	}
	template, err := parseManifest(nodePool.Capi.InfraMachineTemplate)
	return err == nil && isKubeVirtObject(template, kubeVirtMachineTemplateKind)
}

func validateKubeVirtNodeAgentProfile(profile db.Node) error {
	authenticated := profile.SpiffeEnabled || profile.NodeAgentTLSJWTEnabled
	if authenticated && (!profile.NodeAgentTLSJWTEnabled || !profile.NodeAgentExternalMTLSEnabled) {
		return errKubeVirtRequiresTLSJWT
	}
	return nil
}
