// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodescheduling contains shared node provisioning scheduler helpers.
package nodescheduling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
)

var ErrForgeResourcesTemporarilyUnavailable = errors.New("forge resources temporarily unavailable")

// ForgePlacementSpec describes the Forge networking placement to check before
// starting a node provisioning workflow.
type ForgePlacementSpec struct {
	InstanceTypeID uuid.UUID
	SubnetIDs      []uuid.UUID
	VpcPrefixIDs   []uuid.UUID
}

// Service owns shared node scheduling side effects.
type Service struct {
	cfg               config.Config
	db                db.DB
	logger            *slog.Logger
	cweTemporalClient WorkflowStarter
	forgeProvider     ForgeResourceReader
}

// WorkflowStarter is the Temporal client behavior needed by node scheduling.
type WorkflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// ForgeResourceReader is the Forge provider behavior needed by node scheduling.
type ForgeResourceReader interface {
	GetInstanceTypeByID(ctx context.Context, orgID, siteID, instanceTypeID string) (*forgesdk.InstanceType, error)
	GetInstances(ctx context.Context, orgID, siteID string) ([]*forgesdk.Instance, error)
	GetSubnetByID(ctx context.Context, orgID, siteID, subnetID string) (*forgesdk.Subnet, error)
	GetVpcPrefixByID(ctx context.Context, orgID, siteID, vpcPrefixID string) (*forgesdk.VpcPrefix, error)
}

// NewService creates a Service.
func NewService(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
	cweTemporalClient WorkflowStarter,
	forgeProvider ForgeResourceReader,
) *Service {
	return &Service{
		cfg:               cfg,
		db:                dbInstance,
		logger:            logger,
		cweTemporalClient: cweTemporalClient,
		forgeProvider:     forgeProvider,
	}
}

// RetryAfter returns the scheduler retry interval for temporary external
// unavailability such as Forge capacity or provider readiness.
func (s *Service) RetryAfter() time.Duration {
	if s.cfg.ControllerManagerInterval > 0 {
		return s.cfg.ControllerManagerInterval
	}
	return 2 * time.Minute
}

// ScheduleCAPIInfraNodes starts ProvisionNode workflows without Forge
// preflight checks.
func (s *Service) ScheduleCAPIInfraNodes(ctx context.Context, _ *slog.Logger, pendingNodes []db.Node) (reconcile.Result, error) {
	for _, pendingNode := range pendingNodes {
		if err := s.startProvisionNodeWorkflow(ctx, pendingNode); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

// ScheduleForgeNodes starts ProvisionNode workflows up to the currently
// available Forge and network capacity. When capacity or external provider
// readiness is unavailable, it records that on pending nodes and requeues.
func (s *Service) ScheduleForgeNodes(
	ctx context.Context,
	logger *slog.Logger,
	cluster db.Cluster,
	pendingNodes []db.Node,
	instanceTypeID uuid.UUID,
	forgePlacement ForgePlacementSpec,
) (reconcile.Result, error) {
	if len(pendingNodes) == 0 {
		logger.Debug("No pending nodes to schedule")
		return reconcile.Result{}, nil
	}

	forgeInstanceType, availableNetworkCapacity, err := s.GetForgeResources(ctx, cluster, ForgePlacementSpec{
		InstanceTypeID: instanceTypeID,
		SubnetIDs:      forgePlacement.SubnetIDs,
		VpcPrefixIDs:   forgePlacement.VpcPrefixIDs,
	})
	if err != nil {
		if errors.Is(err, ErrForgeResourcesTemporarilyUnavailable) {
			logger.Warn("Forge resources temporarily unavailable", "clusterID", cluster.ID, "error", err)
			return reconcile.Result{RequeueAfter: s.RetryAfter()}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get Forge resources: %w", err)
	}

	logger = logger.With(
		"clusterID", cluster.ID,
		"forgeTotalCount", forgeInstanceType.AllocationStats.Total,
		"forgeUsedCount", forgeInstanceType.AllocationStats.Used,
		"forgeUsableCount", forgeInstanceType.AllocationStats.UnusedUsable,
		"availableNetworkCapacity", availableNetworkCapacity,
	)

	numberToSchedule := min(int(*forgeInstanceType.AllocationStats.UnusedUsable), availableNetworkCapacity, len(pendingNodes))
	logger = logger.With(
		"pendingNodeCount", len(pendingNodes),
		"numberToSchedule", numberToSchedule,
	)

	schedulableNodes := pendingNodes[:numberToSchedule]
	unschedulableNodes := pendingNodes[numberToSchedule:]

	for _, pendingNode := range schedulableNodes {
		if err := s.startProvisionNodeWorkflow(ctx, pendingNode); err != nil {
			return reconcile.Result{}, err
		}

		logger.Info("Started ProvisionNode workflow",
			"workflowID", constants.ProvisionNodeWorkflowPrefix+pendingNode.ID.String(),
			"nodeID", pendingNode.ID,
		)
	}

	for _, unschedulableNode := range unschedulableNodes {
		msg := fmt.Sprintf("Forge resources unavailable: pending since %s", unschedulableNode.CreatedAt.Format(time.RFC3339))
		if err := s.updateNodeCondition(ctx, unschedulableNode, msg); err != nil {
			logger.Error("Failed to update unschedulable node condition", "error", err, "nodeID", unschedulableNode.ID)
		}
	}

	if len(unschedulableNodes) > 0 {
		return reconcile.Result{RequeueAfter: s.RetryAfter()}, nil
	}
	return reconcile.Result{}, nil
}

func (s *Service) startProvisionNodeWorkflow(ctx context.Context, pendingNode db.Node) error {
	workflowID := constants.ProvisionNodeWorkflowPrefix + pendingNode.ID.String()
	workflowOpts := client.StartWorkflowOptions{
		TaskQueue:                s.cfg.TemporalTaskQueue,
		ID:                       workflowID,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}
	if _, err := s.cweTemporalClient.ExecuteWorkflow(ctx, workflowOpts, workflows.ProvisionNode, workflows.ProvisionNodeInput{
		NodeID:                         pendingNode.ID,
		RebootNodeWaitForUptimeTimeout: s.cfg.NodeRebootMaxTimeout,
	}); err != nil {
		return fmt.Errorf("failed to start ProvisionNode workflow: %w", err)
	}
	return nil
}

// GetForgeResources returns Forge compute and network capacity for a placement.
func (s *Service) GetForgeResources(ctx context.Context, cluster db.Cluster, spec ForgePlacementSpec) (*forgesdk.InstanceType, int, error) {
	nicoProvider, err := cluster.NicoProvider()
	if err != nil {
		return nil, 0, err
	}
	forgeInstanceType, err := s.forgeProvider.GetInstanceTypeByID(ctx, nicoProvider.OrgID, nicoProvider.SiteID.String(), spec.InstanceTypeID.String())
	if err != nil {
		return nil, 0, classifyForgeProviderError(err, "failed to get Forge instance type")
	}
	if forgeInstanceType == nil || forgeInstanceType.AllocationStats == nil || forgeInstanceType.AllocationStats.UnusedUsable == nil {
		return nil, 0, fmt.Errorf("failed to get Forge instance type allocation stats")
	}

	availableNetworkCapacity, err := s.calculateAvailableNetworkCapacity(ctx, cluster, spec)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to calculate available network capacity: %w", err)
	}

	return forgeInstanceType, availableNetworkCapacity, nil
}

func (s *Service) calculateAvailableNetworkCapacity(ctx context.Context, cluster db.Cluster, spec ForgePlacementSpec) (int, error) {
	nicoProvider, err := cluster.NicoProvider()
	if err != nil {
		return 0, err
	}
	siteID := nicoProvider.SiteID.String()
	instances, err := s.forgeProvider.GetInstances(ctx, nicoProvider.OrgID, siteID)
	if err != nil {
		return 0, classifyForgeProviderError(err, "failed to list Forge instances")
	}

	if len(spec.SubnetIDs) > 0 {
		totalAvailable := 0

		for _, subnetID := range spec.SubnetIDs {
			forgeSubnet, err := s.forgeProvider.GetSubnetByID(ctx, nicoProvider.OrgID, siteID, subnetID.String())
			if err != nil || forgeSubnet == nil {
				if err != nil {
					return 0, classifyForgeProviderError(err, "failed to get subnet "+subnetID.String())
				}
				return 0, fmt.Errorf("subnet %s not found", subnetID.String())
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

			usedIPs := s.countUsedIPsInSubnet(instances, nicoProvider.VpcID, forgeSubnet, parsedSubnet)
			available := max(0, usableIPs-usedIPs)
			totalAvailable += available
		}

		return totalAvailable, nil
	}

	instanceCapacity := 0
	for _, vpcPrefixID := range spec.VpcPrefixIDs {
		forgeVpcPrefix, err := s.forgeProvider.GetVpcPrefixByID(ctx, nicoProvider.OrgID, siteID, vpcPrefixID.String())
		if err != nil || forgeVpcPrefix == nil {
			if err != nil {
				return 0, classifyForgeProviderError(err, "failed to get vpc prefix "+vpcPrefixID.String())
			}
			return 0, fmt.Errorf("vpc prefix %s not found", vpcPrefixID.String())
		}

		if forgeVpcPrefix.PrefixLength == nil {
			return 0, fmt.Errorf("vpc prefix prefixLength is not set for vpc prefix %s", vpcPrefixID.String())
		}

		totalIPs := int(math.Pow(2, float64(32-*forgeVpcPrefix.PrefixLength)))

		usedIPs := s.countUsedIPsInVPCPrefix(instances, nicoProvider.VpcID, forgeVpcPrefix)
		available := max(0, totalIPs-usedIPs)
		instanceCapacity += available / 2 // 2 IPs per instance in FNN
	}

	return instanceCapacity, nil
}

func classifyForgeProviderError(err error, message string) error {
	if isForgeCacheExpiredError(err) {
		return fmt.Errorf("%w: %s: %w", ErrForgeResourcesTemporarilyUnavailable, message, err)
	}
	return fmt.Errorf("%s: %w", message, err)
}

func isForgeCacheExpiredError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "cache expired")
}

func (s *Service) countUsedIPsInSubnet(instances []*forgesdk.Instance, vpcID uuid.UUID, forgeSubnet *forgesdk.Subnet, subnet *net.IPNet) int {
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

func (s *Service) countUsedIPsInVPCPrefix(instances []*forgesdk.Instance, vpcID uuid.UUID, forgeVpcPrefix *forgesdk.VpcPrefix) int {
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

func (s *Service) updateNodeCondition(ctx context.Context, pendingNode db.Node, message string) error {
	return s.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		node, err := tx.GetNodeForUpdate(ctx, pendingNode.ID)
		if err != nil {
			return err
		}

		if node.Conditions.Pending.Status != db.ConditionStatusTrue {
			return nil
		}
		if node.Conditions.Pending.Message == message {
			return nil
		}

		node.Conditions.Pending.Message = message
		_, err = tx.UpdateNode(ctx, &node)
		return err
	})
}
