// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
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
	return capi.ApplyClusterScaffold(ctx, a.k8sClient, cluster, false, a.cfg.FieldManagerName, a.cfg.ManagedByLabelValue)
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

	namespace := constants.NamespaceNKESystem

	infraCluster, err := capi.ParseManifest(manifest)
	if err != nil {
		return fmt.Errorf("failed to parse infra cluster manifest for cluster %s: %w", cluster.Name, err)
	}
	if err := waitForClusterMachinesDeleted(ctx, a.k8sClient, namespace, cluster.Name); err != nil {
		return err
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

	return nil
}

// waitForClusterMachinesDeleted blocks until no CAPI Machines remain for the
// cluster. Node teardown only requests Machine deletion, so Machines can still
// be finalizing once the node rows are gone and cluster teardown reaches here.
// Deleting the infra cluster while an infra machine still references it strands
// that machine: CAPD stops reconciling a DockerMachine whose DockerCluster it
// cannot find, so the finalizer is never removed, the owning Machine keeps
// waiting on its infrastructure, and the Cluster can never finish deleting.
func waitForClusterMachinesDeleted(ctx context.Context, k8sClient client.Client, namespace, clusterName string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		machines := &clusterv1.MachineList{}
		if err := k8sClient.List(ctx, machines,
			client.InNamespace(namespace),
			client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName},
		); err != nil {
			return fmt.Errorf("failed to list machines for cluster %s: %w", clusterName, err)
		}
		if len(machines.Items) == 0 {
			return nil
		}

		description := fmt.Sprintf("%d machines remaining for cluster %s", len(machines.Items), clusterName)
		activity.RecordHeartbeat(ctx, description)

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for machines of cluster %s to be deleted, %s: %w", clusterName, description, ctx.Err())
		case <-ticker.C:
		}
	}
}
