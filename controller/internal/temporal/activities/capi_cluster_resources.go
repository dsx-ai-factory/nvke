// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/NVIDIA/nke/controller/internal/capi"
)

type ApplyCAPIClusterResourcesInput struct {
	ClusterID uuid.UUID
}

func (a *Activities) ApplyCAPIClusterResources(ctx context.Context, input ApplyCAPIClusterResourcesInput) error {
	cluster, err := a.db.GetCluster(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}
	manifest, err := capi.ClusterScaffoldManifest(cluster)
	if err != nil {
		return err
	}
	if capi.IsEmptyManifest(manifest) {
		return nil
	}

	return capi.ApplyClusterScaffold(ctx, a.k8sClient, cluster)
}

type ApplyCAPIClusterKubeconfigSecretInput struct {
	ClusterID                 uuid.UUID
	InitialControlPlaneNodeID uuid.UUID
}

func (a *Activities) ApplyCAPIClusterKubeconfigSecret(ctx context.Context, input ApplyCAPIClusterKubeconfigSecretInput) error {
	cluster, err := a.db.GetCluster(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}
	manifest, err := capi.ClusterScaffoldManifest(cluster)
	if err != nil {
		return err
	}
	if capi.IsEmptyManifest(manifest) {
		return nil
	}

	node, err := a.db.GetNode(ctx, input.InitialControlPlaneNodeID)
	if err != nil {
		return fmt.Errorf("failed to get initial control plane node: %w", err)
	}
	if node.IP == "" {
		return fmt.Errorf("initial control plane node %s has no IP address", node.ID)
	}

	kubeconfigBytes, err := a.nodeAgentClient.GetKubeconfig(ctx, node)
	if err != nil {
		return fmt.Errorf("failed to fetch admin kubeconfig from initial control plane node: %w", err)
	}
	if kubeconfigBytes == "" {
		return fmt.Errorf("node agent returned an empty kubeconfig for node %s", node.ID)
	}

	return capi.ApplyClusterKubeconfigSecret(ctx, a.k8sClient, cluster, []byte(kubeconfigBytes))
}

type DeleteCAPIClusterResourcesInput struct {
	ClusterID uuid.UUID
}

func (a *Activities) DeleteCAPIClusterResources(ctx context.Context, input DeleteCAPIClusterResourcesInput) error {
	cluster, err := a.db.GetCluster(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}
	manifest, err := capi.ClusterScaffoldManifest(cluster)
	if err != nil {
		return err
	}
	if capi.IsEmptyManifest(manifest) {
		return nil
	}

	namespace := capi.ClusterNamespace(cluster.Name)

	infraCluster, err := capi.ParseManifest(manifest)
	if err != nil {
		return fmt.Errorf("failed to parse infra cluster manifest for cluster %s: %w", cluster.Name, err)
	}
	infraCluster.SetNamespace(namespace)
	infraCluster.SetName(cluster.Name)
	if err := a.k8sClient.Delete(ctx, infraCluster); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete infra cluster %s: %w", infraCluster.GetName(), err)
	}
	if err := waitForObjectDeleted(ctx, a.k8sClient, infraCluster); err != nil {
		return fmt.Errorf("failed waiting for infra cluster %s deletion: %w", infraCluster.GetName(), err)
	}

	clusterObj := &clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Cluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: namespace,
		},
	}
	if err := a.k8sClient.Delete(ctx, clusterObj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete capi cluster %s: %w", clusterObj.Name, err)
	}
	if err := waitForObjectDeleted(ctx, a.k8sClient, clusterObj); err != nil {
		return fmt.Errorf("failed waiting for capi cluster %s deletion: %w", clusterObj.Name, err)
	}

	namespaceObj := &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
	if err := a.k8sClient.Delete(ctx, namespaceObj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete cluster namespace %s: %w", namespace, err)
	}
	if err := waitForObjectDeleted(ctx, a.k8sClient, namespaceObj); err != nil {
		return fmt.Errorf("failed waiting for cluster namespace %s deletion: %w", namespace, err)
	}

	return nil
}
