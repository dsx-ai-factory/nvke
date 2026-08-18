// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

type NodeConditionsUpdate struct {
	NodeID     uuid.UUID
	Conditions NodeConditions
}

type NodeConditions struct {
	Pending                 *ConditionUpdate `json:"pending,omitempty"`
	Provisioning            *ConditionUpdate `json:"provisioning,omitempty"`
	ReadyToJoin             *ConditionUpdate `json:"readyToJoin,omitempty"`
	Joined                  *ConditionUpdate `json:"joined,omitempty"`
	Provisioned             *ConditionUpdate `json:"provisioned,omitempty"`
	Deleting                *ConditionUpdate `json:"deleting,omitempty"`
	KubernetesReady         *ConditionUpdate `json:"kubernetesReady,omitempty"`
	KubernetesUnschedulable *ConditionUpdate `json:"kubernetesUnschedulable,omitempty"`
	NodeAgentReady          *ConditionUpdate `json:"nodeAgentReady,omitempty"`
	Rebooting               *ConditionUpdate `json:"rebooting,omitempty"`
}

// ListNodeIDs lists the IDs of nodes in the database, with optional filters.
func (a *Activities) ListNodeIDs(ctx context.Context, filters db.NodeFilters) ([]uuid.UUID, error) {
	nodes, err := a.db.ListNodes(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	nodeIDs := make([]uuid.UUID, len(nodes))
	for i := range nodes {
		nodeIDs[i] = nodes[i].ID
	}

	return nodeIDs, nil
}

// ListNodes lists nodes in the database, with optional filters.
func (a *Activities) ListNodes(ctx context.Context, filters db.NodeFilters) ([]db.Node, error) {
	nodes, err := a.db.ListNodes(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	return nodes, nil
}

// Get Node using NodeID
func (a *Activities) GetNode(ctx context.Context, nodeID uuid.UUID) (db.Node, error) {
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// This will not be resolved by retrying, so return a non-retryable temporal error
			return db.Node{}, temporal.NewNonRetryableApplicationError("node not found in database", "db.ErrNotFound", err)
		}
		return db.Node{}, fmt.Errorf("failed to get node: %w", err)
	}
	return node, nil
}

// Get NodePool using NodeID
func (a *Activities) GetNodePoolForNode(ctx context.Context, nodeID uuid.UUID) (db.NodePool, error) {
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// This will not be resolved by retrying, so return a non-retryable temporal error
			return db.NodePool{}, temporal.NewNonRetryableApplicationError("node not found in database", "db.ErrNotFound", err)
		}
		return db.NodePool{}, fmt.Errorf("failed to get node: %w", err)
	}

	_, nodePool, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.NodePool{}, temporal.NewNonRetryableApplicationError("nodepool not found in database", "db.ErrNotFound", err)
		}
		return db.NodePool{}, err
	}
	return nodePool, nil
}

func (a *Activities) GetClusterForNode(ctx context.Context, nodeID uuid.UUID) (db.Cluster, error) {
	node, err := a.db.GetNode(ctx, nodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.Cluster{}, temporal.NewNonRetryableApplicationError("node not found in database", "db.ErrNotFound", err)
		}
		return db.Cluster{}, fmt.Errorf("failed to get node: %w", err)
	}

	cluster, err := a.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.Cluster{}, temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
		}
		return db.Cluster{}, fmt.Errorf("failed to get cluster: %w", err)
	}

	return cluster, nil
}

// UpdateNodeConditions updates the conditions of a node in the database.
func (a *Activities) UpdateNodeConditions(ctx context.Context, update NodeConditionsUpdate) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return a.UpdateNodeConditionsWithTx(ctx, tx, update)
	})
}

// UpdateNodeConditionsWithTx updates the conditions of a node in the database, using the provided transaction.
func (a *Activities) UpdateNodeConditionsWithTx(ctx context.Context, tx db.Tx, update NodeConditionsUpdate) error {
	// Get existing node for update with lock
	node, err := tx.GetNodeForUpdate(ctx, update.NodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// This will not be resolved by retrying, so return a non-retryable temporal error
			return temporal.NewNonRetryableApplicationError("node not found in database", "db.ErrNotFound", err)
		}
		return fmt.Errorf("failed to get node: %w", err)
	}

	updateCondition(&node.Conditions.Pending, update.Conditions.Pending)
	updateCondition(&node.Conditions.Provisioning, update.Conditions.Provisioning)
	updateCondition(&node.Conditions.ReadyToJoin, update.Conditions.ReadyToJoin)
	updateCondition(&node.Conditions.Joined, update.Conditions.Joined)
	updateCondition(&node.Conditions.Provisioned, update.Conditions.Provisioned)
	updateCondition(&node.Conditions.Deleting, update.Conditions.Deleting)
	updateCondition(&node.Conditions.KubernetesReady, update.Conditions.KubernetesReady)
	updateCondition(&node.Conditions.KubernetesUnschedulable, update.Conditions.KubernetesUnschedulable)
	updateCondition(&node.Conditions.NodeAgentReady, update.Conditions.NodeAgentReady)
	updateCondition(&node.Conditions.Rebooting, update.Conditions.Rebooting)

	_, err = tx.UpdateNode(ctx, &node)
	if err != nil {
		return fmt.Errorf("failed to update node conditions: %w", err)
	}

	return nil
}

type NodeForgeDetailsUpdate struct {
	NodeID          uuid.UUID
	ForgeInstanceID string
	ForgeMachineID  string
	IP              string
}

// UpdateNodeForgeDetails updates the Forge instance ID and IP address of a node in the database.
func (a *Activities) UpdateNodeForgeDetails(ctx context.Context, update NodeForgeDetailsUpdate) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var forgeInstanceID *string
		if update.ForgeInstanceID != "" {
			forgeInstanceID = &update.ForgeInstanceID
		}
		var forgeMachineID *string
		if update.ForgeMachineID != "" {
			forgeMachineID = &update.ForgeMachineID
		}
		var ip *string
		if update.IP != "" {
			ip = &update.IP
		}

		if err := tx.UpdateNodeForgeDetails(ctx, update.NodeID, forgeInstanceID, forgeMachineID, ip); err != nil {
			return fmt.Errorf("failed to update node Forge details: %w", err)
		}

		return nil
	})
}

// ListControlPlaneNodes returns all control plane nodes for a cluster.
func (a *Activities) ListControlPlaneNodes(ctx context.Context, clusterID uuid.UUID) ([]db.Node, error) {
	controlPlane := true
	nodes, err := a.db.ListNodes(ctx, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlane,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list control plane nodes: %w", err)
	}
	return nodes, nil
}

// DeleteNode deletes a node from the database.
func (a *Activities) DeleteNode(ctx context.Context, nodeID uuid.UUID) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if err := tx.DeleteNode(ctx, nodeID); err != nil {
			return fmt.Errorf("failed to delete node from database: %w", err)
		}

		return nil
	})
}
