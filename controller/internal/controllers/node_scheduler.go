// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controllers

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	forgesdk "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
)

// NodeScheduler is responsible for scheduling (provisioning) nodes:
type NodeScheduler struct {
	cfg               config.Config
	db                db.DB
	logger            *slog.Logger
	cweTemporalClient client.Client
	forgeProvider     *forge.ForgeProvider
}

type ForgePlacementSpec struct {
	InstanceTypeID uuid.UUID
	SubnetIDs      []uuid.UUID
	VpcPrefixIDs   []uuid.UUID
}

func NewNodeScheduler(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
	cweTemporalClient client.Client,
	forgeProvider *forge.ForgeProvider,
) *NodeScheduler {
	return &NodeScheduler{
		cfg:               cfg,
		db:                dbInstance,
		logger:            logger,
		cweTemporalClient: cweTemporalClient,
		forgeProvider:     forgeProvider,
	}
}

// - List pending nodes
//   - For each pending node record
//   - Query forge for available resources
//   - If and only if there are available resources, create a provision node workflow.
func (a *NodeScheduler) Schedule(ctx context.Context, site db.Site) error {
	nodePools, err := a.db.ListNodePools(ctx, db.NodePoolFilters{SiteID: &site.ID})
	if err != nil {
		return fmt.Errorf("failed to list node pools: %w", err)
	}

	for _, nodePool := range nodePools {
		if err := a.scheduleNodePoolNodes(ctx, site, nodePool); err != nil {
			// Log errors but continue scheduling other nodes.
			a.logger.Error("Failed to schedule node pool nodes", "error", err, "nodePoolID", nodePool.ID)
			continue
		}
	}

	if err := a.scheduleControlPlaneNodes(ctx, site); err != nil {
		a.logger.Error("Failed to schedule control plane nodes", "error", err)
		return err
	}

	return nil
}

func (a *NodeScheduler) scheduleCapiInfraNodes(ctx context.Context, _ *slog.Logger, pendingNodes []db.Node) error {
	for _, pendingNode := range pendingNodes {
		workflowID := constants.ProvisionNodeWorkflowPrefix + pendingNode.ID.String()
		workflowOpts := client.StartWorkflowOptions{
			TaskQueue:                a.cfg.TemporalTaskQueue,
			ID:                       workflowID,
			WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
			WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		}
		if _, err := a.cweTemporalClient.ExecuteWorkflow(ctx, workflowOpts, workflows.ProvisionNode, workflows.ProvisionNodeInput{
			NodeID:                         pendingNode.ID,
			RebootNodeWaitForUptimeTimeout: a.cfg.NodeRebootMaxTimeout,
		}); err != nil {
			return fmt.Errorf("failed to start ProvisionNode workflow: %w", err)
		}
	}

	return nil
}

func (a *NodeScheduler) scheduleForgeNodes(
	ctx context.Context,
	logger *slog.Logger,
	cluster db.Cluster,
	pendingNodes []db.Node,
	instanceTypeID uuid.UUID,
	forgePlacement ForgePlacementSpec,
) error {
	if len(pendingNodes) == 0 {
		logger.Debug("No pending nodes to schedule")
		return nil
	}

	forgeInstanceType, availableNetworkCapacity, err := a.getForgeResources(ctx, cluster, ForgePlacementSpec{
		InstanceTypeID: instanceTypeID,
		SubnetIDs:      forgePlacement.SubnetIDs,
		VpcPrefixIDs:   forgePlacement.VpcPrefixIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to get Forge resources: %w", err)
	}

	logger = logger.With(
		"clusterID", cluster.ID,
		"forgeTotalCount", forgeInstanceType.AllocationStats.Total,
		"forgeUsedCount", forgeInstanceType.AllocationStats.Used,
		"forgeUsableCount", forgeInstanceType.AllocationStats.UnusedUsable,
		"availableNetworkCapacity", availableNetworkCapacity,
	)

	// Prefer to check forge usable count. Starting a node provision when forge has
	// Unused but not Usable instances results in several failed workflows, before the node
	// actually becomes usable. Which is not necessarily a problem, but it is untidy.
	numberToSchedule := min(int(*forgeInstanceType.AllocationStats.UnusedUsable), availableNetworkCapacity, len(pendingNodes))
	logger = logger.With(
		"pendingNodeCount", len(pendingNodes),
		"numberToSchedule", numberToSchedule,
	)

	// Split pending nodes into schedulable and blocked
	schedulableNodes := pendingNodes[:numberToSchedule]
	unschedulableNodes := pendingNodes[numberToSchedule:]

	// Provision the schedulable nodes
	for _, pendingNode := range schedulableNodes {
		workflowID := constants.ProvisionNodeWorkflowPrefix + pendingNode.ID.String()

		workflowOpts := client.StartWorkflowOptions{
			TaskQueue: a.cfg.TemporalTaskQueue,
			ID:        workflowID,
			// If a workflow is already running for this node, use it
			WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
			// Only allow starting a new workflow if the previous one failed
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		}

		we, err := a.cweTemporalClient.ExecuteWorkflow(ctx, workflowOpts, workflows.ProvisionNode, workflows.ProvisionNodeInput{
			NodeID:                         pendingNode.ID,
			RebootNodeWaitForUptimeTimeout: a.cfg.NodeRebootMaxTimeout,
		})
		if err != nil {
			return fmt.Errorf("failed to start ProvisionNode workflow: %w", err)
		}

		logger.Info("Started ProvisionNode workflow",
			"workflowID", we.GetID(),
			"runID", we.GetRunID(),
			"nodeID", pendingNode.ID,
		)
	}

	// Update pending node condition for unschedulable nodes
	for _, unschedulableNode := range unschedulableNodes {
		msg := fmt.Sprintf("Forge resources unavailable: pending since %s", unschedulableNode.CreatedAt.Format(time.RFC3339))
		if err := a.updateNodeCondition(ctx, unschedulableNode, msg); err != nil {
			logger.Error("Failed to update unschedulable node condition", "error", err, "nodeID", unschedulableNode.ID)
		}
	}

	return nil
}

func (a *NodeScheduler) scheduleNodePoolNodes(ctx context.Context, site db.Site, nodePool db.NodePool) error {
	// Note scheduling all the nodes in a node pool at once is not necessary, but it offers some benefits
	// - When we are initially provisioning a large node pool it results in far fewer queries for cluster and nodePool.
	// - Because there is no locking on the usable forge nodes, if we were to try to schedule more pending nodes than usable forge instances
	//   we could end up with a large number of failed workflows. While that is not a problem, it is untidy, and this reduces that.

	logger := a.logger.With(
		"nodePoolID", nodePool.ID,
	)

	if nodePool.AutoProvisionDisabled {
		logger.Debug("Node pool has auto-provision disabled, skipping node pool nodes scheduling")
		return nil
	}

	cluster, err := a.db.GetCluster(ctx, nodePool.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster for node pool: %w", err)
	}

	pendingNodes, err := a.db.ListNodes(ctx, db.NodeFilters{NodePoolID: &nodePool.ID, Pending: pointerTo(true)})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	if capi.ShouldUseCAPIForNodeProvisioning(a.cfg.EnableForgeCapi, cluster) {
		return a.scheduleCapiInfraNodes(ctx, logger, pendingNodes)
	}

	if cluster.SiteID == uuid.Nil {
		logger.Warn("Cluster has zero site ID, skipping forge-based scheduling", "clusterID", cluster.ID)
		return nil
	}

	if cluster.SiteID != site.ID {
		return fmt.Errorf("cluster %s does not belong to site %s", cluster.ID, site.ID)
	}

	return a.scheduleForgeNodes(ctx, logger, cluster, pendingNodes, nodePool.InstanceTypeID, ForgePlacementSpec{
		SubnetIDs:    nodePool.SubnetIDs,
		VpcPrefixIDs: nodePool.VpcPrefixIDs,
	})
}

func pointerTo[T any](v T) *T {
	return &v
}

func (a *NodeScheduler) getForgeResources(ctx context.Context, cluster db.Cluster, spec ForgePlacementSpec) (*forgesdk.InstanceType, int, error) {
	forgeInstanceType, err := a.forgeProvider.GetInstanceTypeByID(ctx, cluster.OrgID, cluster.SiteID.String(), spec.InstanceTypeID.String())
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get Forge instance type: %w", err)
	}
	if forgeInstanceType == nil || forgeInstanceType.AllocationStats == nil || forgeInstanceType.AllocationStats.UnusedUsable == nil {
		return nil, 0, fmt.Errorf("failed to get Forge instance type allocation stats")
	}

	availableNetworkCapacity, err := a.calculateAvailableNetworkCapacity(ctx, cluster, spec)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to calculate available network capacity: %w", err)
	}

	return forgeInstanceType, availableNetworkCapacity, nil
}

// calculateAvailableNetworkCapacity calculates the number of instances that can be created in Forge based on IP availability (in subnets or VPC prefixes based on FNN enablement)
func (a *NodeScheduler) calculateAvailableNetworkCapacity(ctx context.Context, cluster db.Cluster, spec ForgePlacementSpec) (int, error) {
	siteID := cluster.SiteID.String()
	instances, err := a.forgeProvider.GetInstances(ctx, cluster.OrgID, siteID)
	if err != nil {
		return 0, fmt.Errorf("failed to list Forge instances: %w", err)
	}

	if len(spec.SubnetIDs) > 0 {
		totalAvailable := 0

		for _, subnetID := range spec.SubnetIDs {
			forgeSubnet, err := a.forgeProvider.GetSubnetByID(ctx, cluster.OrgID, siteID, subnetID.String())
			if err != nil || forgeSubnet == nil {
				return 0, fmt.Errorf("failed to get subnet %s: %w", subnetID.String(), err)
			}

			if !forgeSubnet.Ipv4Prefix.IsSet() || forgeSubnet.PrefixLength == nil {
				return 0, fmt.Errorf("subnet ipv4Prefix or prefixLength is not set for subnet %s", subnetID.String())
			}

			cidr := fmt.Sprintf("%s/%d", *forgeSubnet.Ipv4Prefix.Get(), *forgeSubnet.PrefixLength)
			_, parsedSubnet, err := net.ParseCIDR(cidr)
			if err != nil {
				return 0, fmt.Errorf("failed to parse forge subnet %s: %w", subnetID.String(), err)
			}

			totalIPs := int(math.Pow(2, float64(32-*forgeSubnet.PrefixLength)))
			usableIPs := max(0, totalIPs-3)

			usedIPs := a.countUsedIPsInSubnet(instances, cluster.VpcID, forgeSubnet, parsedSubnet)
			available := max(0, usableIPs-usedIPs)
			totalAvailable += available
		}

		return totalAvailable, nil
	}

	instanceCapacity := 0
	for _, vpcPrefixID := range spec.VpcPrefixIDs {
		forgeVpcPrefix, err := a.forgeProvider.GetVpcPrefixByID(ctx, cluster.OrgID, siteID, vpcPrefixID.String())
		if err != nil || forgeVpcPrefix == nil {
			return 0, fmt.Errorf("failed to get vpc prefix %s: %w", vpcPrefixID.String(), err)
		}

		if forgeVpcPrefix.PrefixLength == nil {
			return 0, fmt.Errorf("vpc prefix prefixLength is not set for vpc prefix %s", vpcPrefixID.String())
		}

		totalIPs := int(math.Pow(2, float64(32-*forgeVpcPrefix.PrefixLength)))

		usedIPs := a.countUsedIPsInVPCPrefix(instances, cluster.VpcID, forgeVpcPrefix)
		available := max(0, totalIPs-usedIPs)
		instanceCapacity += available / 2 // 2 IPs per instance in FNN
	}

	return instanceCapacity, nil
}

func (a *NodeScheduler) countUsedIPsInSubnet(instances []*forgesdk.Instance, vpcID uuid.UUID, forgeSubnet *forgesdk.Subnet, subnet *net.IPNet) int {
	usedCount := 0
	vpcIDStr := vpcID.String()
	if forgeSubnet.Id == nil {
		return usedCount
	}
	targetSubnetID := *forgeSubnet.Id

	for _, instance := range instances {
		if instance.GetVpcId() != vpcIDStr {
			continue
		}

		for _, iface := range instance.GetInterfaces() {
			subnetRef := iface.SubnetId.Get()
			if subnetRef == nil || *subnetRef != targetSubnetID {
				continue
			}

			for _, ip := range iface.GetIpAddresses() {
				if subnet.Contains(net.ParseIP(ip)) {
					usedCount++
				}
			}
		}
	}

	return usedCount
}

func (a *NodeScheduler) countUsedIPsInVPCPrefix(instances []*forgesdk.Instance, vpcID uuid.UUID, forgeVpcPrefix *forgesdk.VpcPrefix) int {
	usedCount := 0
	vpcIDStr := vpcID.String()
	if forgeVpcPrefix.Id == nil {
		return usedCount
	}
	targetPrefixID := *forgeVpcPrefix.Id

	for _, instance := range instances {
		if instance.GetVpcId() != vpcIDStr {
			continue
		}

		for _, iface := range instance.GetInterfaces() {
			vpcPrefixRef := iface.VpcPrefixId.Get()
			if vpcPrefixRef == nil || *vpcPrefixRef != targetPrefixID {
				continue
			}
			usedCount += 2 // 2 IPs per instance in FNN
		}
	}

	return usedCount
}

func (a *NodeScheduler) updateNodeCondition(ctx context.Context, pendingNode db.Node, message string) error {
	return a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		// Re-read under a row lock so we only mutate Pending.Message on the latest
		// persisted state.
		node, err := tx.GetNodeForUpdate(ctx, pendingNode.ID)
		if err != nil {
			return err
		}

		if node.Conditions.Pending.Status != db.ConditionStatusTrue {
			return nil
		}
		if node.Conditions.Pending.Message == message {
			// If the message is the same as the current message, do not update the condition
			return nil
		}

		node.Conditions.Pending.Message = message
		_, err = tx.UpdateNode(ctx, &node)
		return err
	})
}
