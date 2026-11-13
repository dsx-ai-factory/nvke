// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	// initStatusProbeTimeout bounds each node-agent kubeadm status read made while
	// choosing or checking the initial control-plane node, so one unreachable host
	// cannot hold up the rest.
	initStatusProbeTimeout = 15 * time.Second

	// errTypeControlPlaneAlreadyInitialized is the Temporal error type KubeadmInit
	// returns when another control-plane node of the cluster was already initialized.
	errTypeControlPlaneAlreadyInitialized = "ControlPlaneAlreadyInitialized"
)

// mayHaveRunKubeadm reports whether node has reached the point where kubeadm
// init or join could have run on it. Nodes before ReadyToJoin have no kubeadm
// state, and their agents may not be reachable yet.
func mayHaveRunKubeadm(node db.Node) bool {
	return node.Conditions.ReadyToJoin.Status == db.ConditionStatusTrue ||
		node.Conditions.Joined.Status == db.ConditionStatusTrue
}

// startedKubeadmInit reports whether node's agent says kubeadm init ran there,
// whatever its outcome.
func (a *Activities) startedKubeadmInit(ctx context.Context, node db.Node) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, initStatusProbeTimeout)
	defer cancel()

	status, err := a.nodeAgentClient.GetKubeadmStatus(probeCtx, node)
	if err != nil {
		return false, err
	}
	return strings.ToLower(status.GetKubeadmPhase()) == kubeadmPhaseInit, nil
}

// controlPlaneNodeWithKubeadmInit returns the node among nodes whose agent says
// kubeadm init ran there. A rerun of ProvisionCluster must continue on that node
// rather than initialize a second one. Nodes whose agents cannot be reached are
// skipped here; KubeadmInit refuses to proceed while one of them is unaccounted
// for.
func (a *Activities) controlPlaneNodeWithKubeadmInit(ctx context.Context, nodes []db.Node) (db.Node, bool) {
	logger := activity.GetLogger(ctx)

	for _, node := range nodes {
		if !mayHaveRunKubeadm(node) {
			continue
		}
		activity.RecordHeartbeat(ctx)
		started, err := a.startedKubeadmInit(ctx, node)
		if err != nil {
			logger.Warn("Could not read kubeadm status while choosing the initial control plane node",
				"nodeID", node.ID, "nodeName", node.Name, "error", err)
			continue
		}
		if started {
			return node, true
		}
	}
	return db.Node{}, false
}

// requireNoOtherInitializedControlPlaneNode returns an error unless initialNode
// is the only control-plane node of the cluster that kubeadm init may have run
// on. Initializing a second node would create a separate cluster, with its own
// CA and etcd, behind the same control-plane endpoint.
//
// A node that has joined, or whose agent reports kubeadm init, ends the
// workflow with a non-retryable error. A node whose agent cannot be reached
// returns a retryable error: it may be the node that was initialized, so the
// workflow waits until its agent answers or the node is deleted.
func (a *Activities) requireNoOtherInitializedControlPlaneNode(ctx context.Context, clusterID uuid.UUID, initialNode db.Node) error {
	controlPlane := true
	nodes, err := a.db.ListNodes(ctx, db.NodeFilters{ClusterID: &clusterID, ControlPlane: &controlPlane})
	if err != nil {
		return fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	for _, node := range nodes {
		if node.ID == initialNode.ID || !mayHaveRunKubeadm(node) {
			continue
		}
		if node.Conditions.Joined.Status == db.ConditionStatusTrue {
			return temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("control plane node %s has already joined the cluster, so kubeadm init must not run on node %s", node.Name, initialNode.Name),
				errTypeControlPlaneAlreadyInitialized, nil,
			)
		}
		started, err := a.startedKubeadmInit(ctx, node)
		if err != nil {
			return fmt.Errorf("cannot confirm that kubeadm init has not run on control plane node %s; "+
				"restore its node agent or delete the node: %w", node.Name, err)
		}
		if started {
			return temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("kubeadm init already ran on control plane node %s, so it must not run on node %s", node.Name, initialNode.Name),
				errTypeControlPlaneAlreadyInitialized, nil,
			)
		}
	}
	return nil
}
