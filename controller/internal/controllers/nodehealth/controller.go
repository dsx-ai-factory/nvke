// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nodehealth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"go.temporal.io/sdk/client"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/kube"
)

const (
	nodeAgentProbeConcurrency = 256

	// clusterReconcileConcurrency bounds how many clusters in a single site are
	// reconciled in parallel. Each cluster reconcile can itself fan out up to
	// nodeAgentProbeConcurrency node-agent probes, so this is kept small to cap
	// the multiplicative load on the CWE process, the DB, and tenant apiservers.
	clusterReconcileConcurrency = 8
)

type Controller struct {
	cfg                    config.Config
	logger                 *slog.Logger
	db                     db.DB
	cweTemporalClient      client.Client
	nodeAgentClient        agent.Client
	workloadClientProvider *kube.DBClusterTenantClientProvider
}

func NewController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
	cweTemporalClient client.Client,
	nodeAgentClient agent.Client,
	tenantClientProvider *kube.DBClusterTenantClientProvider,
) *Controller {
	return &Controller{
		cfg:                    cfg,
		logger:                 logger.With("controller", "node-health"),
		db:                     dbInstance,
		cweTemporalClient:      cweTemporalClient,
		nodeAgentClient:        nodeAgentClient,
		workloadClientProvider: tenantClientProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context, site db.Site) error {
	if c.cfg.NodeHealthCheckDisabled {
		c.logger.Debug("Node health checks disabled, skipping", "siteID", site.ID.String())
		return nil
	}

	clusters, err := c.db.ListClusters(ctx, db.ClusterFilters{SiteID: &site.ID})
	if err != nil {
		return fmt.Errorf("failed to list clusters for site %s: %w", site.ID, err)
	}

	// Clusters are independent, so reconcile them concurrently. Each cluster's
	// own timeout (NodeHealthCheckTimeout) is applied inside reconcileCluster,
	// so the site-level wall-clock ceiling stays close to a single cluster's
	// timeout rather than the sum across clusters.
	group := new(errgroup.Group)
	group.SetLimit(clusterReconcileConcurrency)

	for _, cluster := range clusters {
		group.Go(func() error {
			if err := c.reconcileCluster(ctx, cluster); err != nil {
				// Log errors but continue processing the other clusters.
				c.logger.Error("Failed to reconcile node health for cluster",
					"error", err,
					"siteID", site.ID.String(),
					"clusterID", cluster.ID.String(),
					"clusterName", cluster.Name)
			}
			return nil
		})
	}
	_ = group.Wait()

	return nil
}

func (c *Controller) reconcileCluster(ctx context.Context, cluster db.Cluster) error {
	clusterCtx, cancel := context.WithTimeout(ctx, c.cfg.NodeHealthCheckTimeout)
	defer cancel()

	nodes, err := c.db.ListNodes(clusterCtx, db.NodeFilters{ClusterID: &cluster.ID})
	if err != nil {
		return fmt.Errorf("failed to list nodes for cluster %s: %w", cluster.ID, err)
	}
	if len(nodes) == 0 {
		c.logger.Debug("No nodes to check for cluster", "clusterID", cluster.ID.String(), "clusterName", cluster.Name)
		return nil
	}

	tenantClient, err := c.workloadClientProvider.ClientForDBCluster(clusterCtx, cluster)
	if err != nil {
		if errors.Is(err, kube.ErrTenantClientNotReady) {
			c.logger.Debug("Tenant client not ready, skipping node health for cluster", "error", err, "clusterID", cluster.ID.String(), "clusterName", cluster.Name)
			return nil
		}

		return c.markNodesUnreachable(clusterCtx, nodes, fmt.Errorf("failed to create workload client: %w", err))
	}

	kubeNodes, err := listKubeNodes(clusterCtx, tenantClient)
	if err != nil {
		return c.markNodesUnreachable(clusterCtx, nodes, fmt.Errorf("failed to list Kubernetes nodes: %w", err))
	}

	agentProbes := c.probeNodeAgents(clusterCtx, nodes)

	if err := c.updateNodeConditions(clusterCtx, nodes, kubeNodes, agentProbes); err != nil {
		return fmt.Errorf("failed to update node conditions: %w", err)
	}

	if err := c.processNodeRemediation(clusterCtx, nodes, kubeNodes); err != nil {
		return fmt.Errorf("failed to start node remediation workflows: %w", err)
	}

	c.logger.Debug("Successfully reconciled node health for cluster",
		"clusterID", cluster.ID.String(),
		"clusterName", cluster.Name,
		"nodesProcessed", len(nodes))

	return nil
}

func listKubeNodes(ctx context.Context, tenantClient ctrlclient.Client) (map[string]*corev1.Node, error) {
	kubeNodeList := &corev1.NodeList{}
	if err := tenantClient.List(ctx, kubeNodeList); err != nil {
		return nil, err
	}

	kubeNodes := make(map[string]*corev1.Node, len(kubeNodeList.Items))
	for i := range kubeNodeList.Items {
		kubeNode := &kubeNodeList.Items[i]
		kubeNodes[kubeNode.Name] = kubeNode
	}

	return kubeNodes, nil
}

func (c *Controller) probeNodeAgents(ctx context.Context, nodes []db.Node) map[string]nodeAgentProbe {
	probes := make(map[string]nodeAgentProbe, len(nodes))
	var probesMu sync.Mutex

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(nodeAgentProbeConcurrency)

	for _, node := range nodes {
		group.Go(func() error {
			agentVersion, err := c.nodeAgentClient.GetAgentVersion(groupCtx, node)
			probe := nodeAgentProbe{err: err}
			if err == nil {
				probe.agentVersion = agentVersion
			}

			probesMu.Lock()
			probes[node.Name] = probe
			probesMu.Unlock()

			return nil
		})
	}
	_ = group.Wait()

	return probes
}

// updateNodeConditions records the live health gathered for the cluster: each
// node's conditions are derived from its Kubernetes Node object and node-agent
// probe. Each node is updated in its own transaction so a failure on one node
// doesn't roll back the others and each row lock is held only briefly. Nodes
// deleted between listing and updating are skipped.
func (c *Controller) updateNodeConditions(ctx context.Context, nodes []db.Node, kubeNodes map[string]*corev1.Node, agentProbes map[string]nodeAgentProbe) error {
	var errs []error
	for _, node := range nodes {
		kubeNode := kubeNodes[node.Name]
		agentProbe := agentProbes[node.Name]

		err := c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
			lockedNode, err := tx.GetNodeForUpdate(ctx, node.ID)
			if err != nil {
				return err
			}

			applyNodeHealth(&lockedNode, kubeNode, agentProbe)
			if agentProbe.agentVersion != "" {
				lockedNode.AgentVersion = agentProbe.agentVersion
			}

			_, err = tx.UpdateNode(ctx, &lockedNode)
			return err
		})
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				continue // Node was deleted between listing and updating.
			}
			errs = append(errs, fmt.Errorf("failed to update node %s: %w", node.ID, err))
		}
	}

	return errors.Join(errs...)
}

// markNodesUnreachable is the degraded path: the cluster could not be read this
// cycle, so every node's live conditions are set to Unknown (ratcheted
// recycle/repair conditions are deliberately left untouched). It always returns
// cause so the caller still surfaces the underlying failure.
func (c *Controller) markNodesUnreachable(ctx context.Context, nodes []db.Node, cause error) error {
	var errs []error
	for _, node := range nodes {
		err := c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
			lockedNode, err := tx.GetNodeForUpdate(ctx, node.ID)
			if err != nil {
				return err
			}

			markNodeHealthUnknown(&lockedNode, cause.Error())

			_, err = tx.UpdateNode(ctx, &lockedNode)
			return err
		})
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				continue // Node was deleted between listing and updating.
			}
			errs = append(errs, fmt.Errorf("failed to update node %s: %w", node.ID, err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w; additionally failed to update node conditions: %w", cause, err)
	}

	return cause
}

func (c *Controller) processNodeRemediation(ctx context.Context, nodes []db.Node, kubeNodes map[string]*corev1.Node) error {
	var errs []error
	for _, node := range nodes {
		kubeNode := kubeNodes[node.Name]
		if kubeNode == nil {
			continue
		}

		if err := c.startNodeRemediationWorkflowsIfNecessary(ctx, node, kubeNode); err != nil {
			c.logger.Error("Failed to process node remediation", "error", err, "nodeID", node.ID.String(), "nodeName", node.Name)
			errs = append(errs, err)
			continue
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to process node remediation(s): %w", errors.Join(errs...))
	}

	return nil
}
