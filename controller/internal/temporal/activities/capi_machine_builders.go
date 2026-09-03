// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

func buildCAPIBootstrapSecret(createCtx *capiMachineCreateContext, userData string) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-bootstrap-data", createCtx.node.Name),
			Namespace: createCtx.namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: createCtx.cluster.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"value": []byte(userData),
		},
	}
}

func buildCAPIMachineForNode(node db.Node, nodeSet db.NodeSet, nodePool db.NodePool, cluster db.Cluster, namespace, bootstrapSecretName string, infrastructureRef clusterv1.ContractVersionedObjectReference) (*clusterv1.Machine, error) {
	machineVersion := nodeSet.KubernetesVersion
	if machineVersion == "" {
		machineVersion = cluster.KubernetesVersion
	}
	if machineVersion == "" {
		return nil, fmt.Errorf("kubernetes version is empty for node set %s", nodeSet.ID)
	}
	if !strings.HasPrefix(machineVersion, "v") {
		machineVersion = "v" + machineVersion
	}

	return &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        node.Name,
			Namespace:   namespace,
			Labels:      capiMachineLabels(cluster.Name, nodePool.ControlPlane),
			Annotations: capiMachineAnnotations(node),
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: cluster.Name,
			Version:     machineVersion,
			Bootstrap: clusterv1.Bootstrap{
				DataSecretName: &bootstrapSecretName,
			},
			InfrastructureRef: infrastructureRef,
		},
	}, nil
}

func capiMachineLabels(clusterName string, controlPlane bool) map[string]string {
	labels := map[string]string{
		clusterv1.ClusterNameLabel: clusterName,
	}
	if controlPlane {
		labels[clusterv1.MachineControlPlaneLabel] = ""
	}
	return labels
}

func capiMachineAnnotations(node db.Node) map[string]string {
	// CWE owns drain / kube Node removal in teardown workflows. Tell CAPI to skip its
	// drain and volume-detach waits so Machine deletion is not blocked when the
	// workload API is unreachable.
	return map[string]string{
		constants.MachineAnnotationKeyHostID:               node.ID.String(),
		clusterv1.ExcludeNodeDrainingAnnotation:            "cwe-controlled",
		clusterv1.ExcludeWaitForNodeVolumeDetachAnnotation: "cwe-controlled",
	}
}
