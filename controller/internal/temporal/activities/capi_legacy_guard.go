// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/constants"
)

// rejectUnmigratedClusterForNode stops CAPI activities from operating in nke-system
// against a cluster whose objects are still in cluster-<name>: a delete would
// report success and leak the instance, an apply would write a second copy.
// Deleted when the migration controller is no longer needed.
func (a *Activities) rejectUnmigratedClusterForNode(ctx context.Context, nodeID uuid.UUID) error {
	cluster, err := a.GetClusterForNode(ctx, nodeID)
	if err != nil {
		return err
	}

	return a.rejectUnmigratedCluster(ctx, cluster.Name)
}

// rejectUnmigratedCluster stops CAPI activities from operating in nke-system
// against a cluster whose objects are still in cluster-<name>: a delete would
// report success and leak the instance, an apply would write a second copy.
// Deleted when the migration controller is no longer needed.
func (a *Activities) rejectUnmigratedCluster(ctx context.Context, clusterName string) error {
	namespace := capi.LegacyClusterNamespace(clusterName)

	err := a.k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: clusterName}, &clusterv1.Cluster{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to look for a legacy CAPI Cluster at %s/%s: %w", namespace, clusterName, err)
	}

	return fmt.Errorf(
		"cluster %s has not been adopted into %s: its CAPI objects are still in %s, where this operation would not reach them",
		clusterName, constants.NamespaceNKESystem, namespace,
	)
}
