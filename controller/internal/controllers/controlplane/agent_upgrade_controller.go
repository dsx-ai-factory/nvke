// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

// Agent upgrade controller:
// 1. Leave converged nodes unchanged. Select one eligible control-plane node.
// 2. Request installation through the agent RPC. The independent updater does
//    the installation; this controller does not wait for it to finish.
// 3. Check progress on later reconciliations. Retry failures on the same node.
// 4. Leave accepted installation alone during deletion or replacement. Do not
//    send another installation request, and do not block teardown.
// Certificate renewal keeps its Temporal workflow. Actual renewal and agent
// installation cannot overlap; a due or queued renewal does not block upgrades.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	agentUpgradeRetryDelay = 30 * time.Second
)

func (c *Controller) reconcileNodeAgentUpgrade(ctx context.Context, cluster db.Cluster) (reconcile.Result, error) {
	if cluster.ID == uuid.Nil || cluster.Conditions.Deleting.Status == db.ConditionStatusTrue ||
		(cluster.ControlPlaneConfig.NodeAgentConfig.Version == "" && cluster.NodeAgentUpgrade == nil && cluster.CertificateRenewal == nil) {
		return reconcile.Result{}, nil
	}
	if cluster.CertificateRenewal != nil {
		running, err := c.checkCertificateRenewal(ctx, cluster)
		if running || err != nil {
			return reconcile.Result{RequeueAfter: 10 * time.Second}, err
		}
	}

	selected, err := c.selectAgentUpgrade(ctx, cluster)
	if err != nil {
		return reconcile.Result{}, err
	}
	if selected == nil {
		return reconcile.Result{RequeueAfter: agentUpgradeRetryDelay}, nil
	}
	if time.Now().Before(selected.RetryAt) {
		return reconcile.Result{RequeueAfter: time.Until(selected.RetryAt)}, nil
	}
	if err := c.runNodeAgentUpgrade(ctx, cluster.ID, *selected); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
}

// selectAgentUpgrade keeps accepted work on its node. Only selection of new
// work needs a transaction; status polling does not reserve it again.
func (c *Controller) selectAgentUpgrade(ctx context.Context, cluster db.Cluster) (*db.NodeAgentUpgrade, error) {
	if active := cluster.NodeAgentUpgrade; active != nil {
		current := *active
		return &current, nil
	}
	var selected *db.NodeAgentUpgrade
	err := c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		locked, err := tx.GetClusterForUpdate(ctx, cluster.ID)
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if locked.Conditions.Deleting.Status == db.ConditionStatusTrue || locked.CertificateRenewal != nil {
			return nil
		}
		if active := locked.NodeAgentUpgrade; active != nil {
			current := *active
			selected = &current
			return nil
		}
		target := locked.ControlPlaneConfig.NodeAgentConfig.Version
		if target == "" || locked.Conditions.UpdateInProgress != nil ||
			locked.Conditions.ControlPlaneUpgrading.Status == db.ConditionStatusTrue || locked.Conditions.CPNodesRecycling.Status == db.ConditionStatusTrue {
			return nil
		}
		controlPlane := true
		nodes, err := tx.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID, ControlPlane: &controlPlane})
		if err != nil {
			return err
		}
		for _, node := range nodes {
			if agentUpgradeRemovalRequested(node) || node.Conditions.ControlPlaneRolloutRequired.Status == db.ConditionStatusTrue ||
				(node.Conditions.ReadyToJoin.Status == db.ConditionStatusTrue && node.Conditions.Joined.Status != db.ConditionStatusTrue) {
				return nil
			}
		}
		for _, node := range nodes {
			if agentUpgradeEligible(node) && node.AgentVersion != target {
				selected = &db.NodeAgentUpgrade{Source: "background", NodeID: node.ID, TargetVersion: target, Method: c.cfg.NodeAgent.UpdateMethod, Phase: "Pending"}
				locked.NodeAgentUpgrade = selected
				_, err = tx.UpdateCluster(ctx, &locked)
				return err
			}
		}
		return nil
	})
	return selected, err
}

func (c *Controller) runNodeAgentUpgrade(ctx context.Context, clusterID uuid.UUID, selected db.NodeAgentUpgrade) error {
	cluster, err := c.db.GetCluster(ctx, clusterID)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !sameAgentUpgrade(cluster.NodeAgentUpgrade, selected) || cluster.CertificateRenewal != nil || cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return nil
	}
	node, err := c.db.GetNode(ctx, selected.NodeID)
	if errors.Is(err, db.ErrNotFound) {
		return c.saveAgentUpgradeProgress(ctx, clusterID, selected, "", true, nil)
	}
	if err != nil {
		return err
	}
	if agentUpgradeRemovalRequested(node) || selected.Source == db.NodeAgentUpgradeSourceLifecycle {
		return nil
	}
	if c.nodeAgentClient == nil {
		return fmt.Errorf("node-agent client is required for agent convergence")
	}
	// The accepted target stays unchanged. After completion, the next reconcile
	// reads the current desired version instead of changing an active request.
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	version, done, callErr := c.nodeAgentClient.ConvergeNodeAgent(callCtx, node, selected.TargetVersion, selected.Method)
	cancel()
	if done && callErr == nil && version != selected.TargetVersion {
		callErr = fmt.Errorf("node agent reports %q, expected %q", version, selected.TargetVersion)
	}
	return c.saveAgentUpgradeProgress(ctx, clusterID, selected, version, done, callErr)
}

func (c *Controller) saveAgentUpgradeProgress(ctx context.Context, clusterID uuid.UUID, selected db.NodeAgentUpgrade, version string, done bool, callErr error) error {
	return c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, clusterID)
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !sameAgentUpgrade(cluster.NodeAgentUpgrade, selected) {
			return nil
		}
		// Re-read the node so a status response cannot overwrite deletion state.
		node, err := tx.GetNodeForUpdate(ctx, selected.NodeID)
		switch {
		case errors.Is(err, db.ErrNotFound):
			cluster.NodeAgentUpgrade = nil
		case err != nil:
			return err
		default:
			if version != "" {
				node.AgentVersion = version
				if _, err := tx.UpdateNode(ctx, &node); err != nil {
					return err
				}
			}
			active := cluster.NodeAgentUpgrade
			active.Phase = "Running"
			active.RetryAt = time.Time{}
			if callErr != nil {
				active.ErrorMessage = callErr.Error()
				active.RetryAt = time.Now().Add(agentUpgradeRetryDelay)
			}
			if done && callErr == nil {
				cluster.NodeAgentUpgrade = nil
			}
		}
		_, err = tx.UpdateCluster(ctx, &cluster)
		return err
	})
}

func sameAgentUpgrade(active *db.NodeAgentUpgrade, selected db.NodeAgentUpgrade) bool {
	return active != nil && active.NodeID == selected.NodeID && active.TargetVersion == selected.TargetVersion && active.Method == selected.Method && active.Source == selected.Source
}

func agentUpgradeEligible(node db.Node) bool {
	return node.IsManagedByControlPlaneController() &&
		node.Conditions.Joined.Status == db.ConditionStatusTrue &&
		node.Conditions.Provisioned.Status == db.ConditionStatusTrue &&
		node.Conditions.KubernetesReady.Status == db.ConditionStatusTrue &&
		node.Conditions.NodeAgentReady.Status == db.ConditionStatusTrue &&
		!agentUpgradeRemovalRequested(node) && node.Conditions.Rebooting.Status != db.ConditionStatusTrue
}

func agentUpgradeRemovalRequested(node db.Node) bool {
	return node.Conditions.DeleteRequested.Status == db.ConditionStatusTrue || node.Conditions.Deleting.Status == db.ConditionStatusTrue ||
		node.Conditions.MarkedForRepair.Status == db.ConditionStatusTrue || node.Conditions.MarkedForRecycling.Status == db.ConditionStatusTrue
}

func agentUpgradeBlocksNodeWork(cluster db.Cluster, nodes []db.Node) bool {
	if cluster.NodeAgentUpgrade == nil {
		return false
	}
	for _, node := range nodes {
		if node.ID == cluster.NodeAgentUpgrade.NodeID {
			return !agentUpgradeRemovalRequested(node)
		}
	}
	return false
}

// checkCertificateRenewal reads actual agent work, not Temporal or queue state.
// This also recovers an accepted renewal if its activity stopped polling it.
func (c *Controller) checkCertificateRenewal(ctx context.Context, cluster db.Cluster) (bool, error) {
	if c.nodeAgentClient == nil {
		return true, fmt.Errorf("node-agent client is required for agent convergence")
	}
	active := *cluster.CertificateRenewal
	node, err := c.db.GetNode(ctx, active.NodeID)
	if !errors.Is(err, db.ErrNotFound) {
		if err != nil {
			return true, err
		}
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		response, callErr := c.nodeAgentClient.GetKubernetesCertificateRenewalStatus(callCtx, node, &pb.GetKubernetesCertificateRenewalStatusRequest{OperationId: active.OperationID})
		cancel()
		if status.Code(callErr) == codes.Unimplemented {
			// The existing legacy activity releases its record when its script ends.
			return true, nil
		}
		if callErr != nil {
			return true, callErr
		}
		switch response.GetOperation().GetOperation().GetPhase() {
		case pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED:
		case pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING:
			return true, nil
		case pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED:
			return true, fmt.Errorf("certificate renewal status is empty")
		default:
			return true, fmt.Errorf("certificate renewal status is unknown")
		}
	}
	err = c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		locked, err := tx.GetClusterForUpdate(ctx, cluster.ID)
		if err != nil {
			return err
		}
		if locked.CertificateRenewal != nil && *locked.CertificateRenewal == active {
			locked.CertificateRenewal = nil
			_, err = tx.UpdateCluster(ctx, &locked)
		}
		return err
	})
	return false, err
}
