// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/capi/nico"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func ApplyClusterScaffold(ctx context.Context, k8sClient client.Client, cluster db.Cluster, paused bool, fieldManagerName, managedByLabelValue string) error {
	namespace := constants.NamespaceNKESystem

	manifest, err := ClusterScaffoldManifest(cluster)
	if err != nil {
		return err
	}
	infraCluster, err := ParseManifest(manifest)
	if err != nil {
		return fmt.Errorf("failed to parse infra.cluster: %w", err)
	}
	infraCluster.SetNamespace(namespace)
	infraCluster.SetName(cluster.Name)
	SetPausedAnnotation(infraCluster, paused)
	if err := k8sClient.Patch(ctx, infraCluster, client.Apply, client.FieldOwner(fieldManagerName), client.ForceOwnership); err != nil {
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
				constants.LabelKeyNkeManagedBy: managedByLabelValue,
			},
		},
		Spec: clusterv1.ClusterSpec{
			Paused: ptr.To(paused),
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infraGV.Group,
				Kind:     infraCluster.GetKind(),
				Name:     infraCluster.GetName(),
			},
		},
	}
	SetPausedAnnotation(clusterObj, paused)

	if err := k8sClient.Patch(ctx, clusterObj, client.Apply, client.FieldOwner(fieldManagerName), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply CAPI cluster: %w", err)
	}

	return nil
}

// SetPausedAnnotation sets or removes the CAPI paused annotation.
func SetPausedAnnotation(obj metav1.Object, paused bool) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if paused {
		annotations[clusterv1.PausedAnnotation] = "true"
	} else {
		delete(annotations, clusterv1.PausedAnnotation)
	}
	obj.SetAnnotations(annotations)
}

// ClusterScaffold builds the infrastructure-provider manifest for a cluster.
// Each provider type has its own implementation, selected by
// GetClusterScaffold, so provider-specific logic lives in its own package
// instead of behind a switch shared by every provider.
type ClusterScaffold interface {
	GetClusterScaffoldManifest(cluster db.Cluster) (json.RawMessage, error)
}

// GetClusterScaffold selects the ClusterScaffold implementation for the
// cluster's provider type. Adding a provider means adding a case here and
// its own implementation package, not a new switch arm at each call site.
func GetClusterScaffold(cluster db.Cluster) (ClusterScaffold, error) {
	switch cluster.ProviderType {
	case db.ProviderTypeCAPI:
		return capiClusterScaffold{}, nil
	case db.ProviderTypeNico:
		return nico.New(), nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q for cluster %s", cluster.ProviderType, cluster.ID)
	}
}

// ClusterScaffoldManifest builds the infrastructure-provider manifest (CAPI
// InfraCluster or NicoCluster) for the cluster.
func ClusterScaffoldManifest(cluster db.Cluster) (json.RawMessage, error) {
	scaffold, err := GetClusterScaffold(cluster)
	if err != nil {
		return nil, err
	}
	return scaffold.GetClusterScaffoldManifest(cluster)
}

// capiClusterScaffold builds cluster scaffold manifests for clusters where
// the caller supplies the CAPI infrastructure template directly.
type capiClusterScaffold struct{}

func (capiClusterScaffold) GetClusterScaffoldManifest(cluster db.Cluster) (json.RawMessage, error) {
	provider, err := cluster.CAPIProvider()
	if err != nil {
		return nil, err
	}
	return provider.InfraCluster, nil
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
