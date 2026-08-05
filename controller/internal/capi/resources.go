// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package capi

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capikubeconfig "sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

func ApplyClusterScaffold(ctx context.Context, k8sClient client.Client, cluster db.Cluster) error {
	namespace := ClusterNamespace(cluster.Name)
	if err := ApplyNamespace(ctx, k8sClient, namespace); err != nil {
		return err
	}

	infraCluster, err := ParseManifest(cluster.CapiInfraCluster)
	if err != nil {
		return fmt.Errorf("failed to parse capiInfraCluster: %w", err)
	}
	infraCluster.SetNamespace(namespace)
	infraCluster.SetName(cluster.Name)
	if err := k8sClient.Patch(ctx, infraCluster, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply infra cluster: %w", err)
	}

	infraGV, err := schema.ParseGroupVersion(infraCluster.GetAPIVersion())
	if err != nil {
		return fmt.Errorf("failed to parse infra cluster apiVersion %q: %w", infraCluster.GetAPIVersion(), err)
	}
	if infraGV.Group == "" {
		return fmt.Errorf("infra cluster apiVersion %q must include an API group", infraCluster.GetAPIVersion())
	}

	clusterObj := &clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Cluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: namespace,
			Labels: map[string]string{
				constants.LabelKeyNkeManagedBy: constants.LabelValueNkeManagedBy,
			},
		},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infraGV.Group,
				Kind:     infraCluster.GetKind(),
				Name:     infraCluster.GetName(),
			},
		},
	}

	if err := k8sClient.Patch(ctx, clusterObj, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply CAPI cluster: %w", err)
	}

	return nil
}

func ApplyClusterKubeconfigSecret(ctx context.Context, k8sClient client.Client, cluster db.Cluster, kubeconfigBytes []byte) error {
	namespace := ClusterNamespace(cluster.Name)
	clusterObj := &clusterv1.Cluster{}
	if err := k8sClient.Get(ctx, client.ObjectKey{
		Name:      cluster.Name,
		Namespace: namespace,
	}, clusterObj); err != nil {
		return fmt.Errorf("failed to get CAPI cluster %s/%s: %w", namespace, cluster.Name, err)
	}

	secretObj := capikubeconfig.GenerateSecret(clusterObj, kubeconfigBytes)
	secretObj.TypeMeta = metav1.TypeMeta{
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "Secret",
	}

	if err := k8sClient.Create(ctx, secretObj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// nothing to do, secret already exists
			return nil
		}
		return fmt.Errorf("creating CAPI kubeconfig secret %s/%s: %w", secretObj.Namespace, secretObj.Name, err)
	}

	return nil
}

func ParseManifest(raw []byte) (*unstructured.Unstructured, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: obj}
	if u.GetAPIVersion() == "" || u.GetKind() == "" {
		return nil, fmt.Errorf("manifest must include apiVersion and kind")
	}
	return u, nil
}

func ApplyNamespace(ctx context.Context, k8sClient client.Client, namespace string) error {
	if err := k8sClient.Patch(ctx, &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}, client.Apply, client.FieldOwner(constants.FieldManagerCWE), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply namespace %q: %w", namespace, err)
	}

	return nil
}
