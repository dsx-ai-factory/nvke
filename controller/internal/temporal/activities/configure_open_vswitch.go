// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/metrics"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
)

const (
	legacyOpenVSwitchRunbook      = "configure-openvswitch"
	openVSwitchStatusPollInterval = 2 * time.Second
	openVSwitchReconciliationPath = "rpc"
	legacyOpenVSwitchRunbookPath  = "legacy_runbook"
)

// ConfigureOpenVSwitchInput identifies the node whose pinned network intent
// should be reconciled onto an Open vSwitch bridge.
type ConfigureOpenVSwitchInput struct {
	NodeID      uuid.UUID
	OperationID string
}

// ConfigureOpenVSwitchResult identifies the realized bridge and the
// implementation path that reconciled it.
type ConfigureOpenVSwitchResult struct {
	BridgeName      string
	UplinkInterface string
	NetworkBackend  string
	Path            string
}

// ConfigureOpenVSwitch starts a durable node-agent operation and polls its
// observed state. Explicit intent requires the configured RPC. Only an
// unconfigured request may fall back when its RPC is Unimplemented.
func (a *Activities) ConfigureOpenVSwitch(
	ctx context.Context,
	input ConfigureOpenVSwitchInput,
) (ConfigureOpenVSwitchResult, error) {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return ConfigureOpenVSwitchResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	clusterName := ""
	if cluster, clusterErr := a.db.GetCluster(ctx, node.ClusterID); clusterErr != nil {
		activity.GetLogger(ctx).Debug("failed to resolve cluster name for Open vSwitch metric", "clusterID", node.ClusterID, "error", clusterErr)
	} else {
		clusterName = cluster.Name
	}
	result, path, err := a.configureOpenVSwitchOnNode(ctx, input, node)
	metrics.RecordOpenVSwitchReconciliation(clusterName, node.Name, path, err)
	return result, err
}

func (a *Activities) configureOpenVSwitchOnNode(
	ctx context.Context,
	input ConfigureOpenVSwitchInput,
	node db.Node,
) (ConfigureOpenVSwitchResult, string, error) {
	path := openVSwitchReconciliationPath
	nodeSet, _, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return ConfigureOpenVSwitchResult{}, path, fmt.Errorf("failed to resolve Open vSwitch node configuration: %w", err)
	}
	desired := nodeSet.OpenVSwitch
	// Pool managed nodes use their NodeSet's configuration snapshot. Control-plane
	// nodes without a pool instead pin a snapshot via ControlPlaneConfigurationID.
	// Older nodes can lack that ID, so GetNodeContext falls back to the current
	// cluster settings. Reject explicit OVS intent here to avoid applying a newer
	// network layout to a node whose original configuration is unknown.
	if desired != nil && node.ControlPlane && !node.IsManagedByNodePool() && node.ControlPlaneConfigurationID == uuid.Nil {
		return ConfigureOpenVSwitchResult{}, path, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("control-plane node %s has no pinned configuration; explicit Open vSwitch intent requires a replacement with a configuration snapshot", node.Name),
			"OpenVSwitchConfigurationUnpinned", nil,
		)
	}
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()

	startResponse, err := a.startOpenVSwitchRPC(ctx, input.OperationID, node, desired)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			if desired == nil {
				result, fallbackErr := a.configureOpenVSwitchWithLegacyRunbook(ctx, input, node)
				return result, legacyOpenVSwitchRunbookPath, fallbackErr
			}
			err = temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("node %s requires an agent supporting StartConfiguredOpenVSwitchReconciliation for explicit uplink %s", node.Name, desired.UplinkInterface),
				"OpenVSwitchConfigurationUnsupported", err,
			)
		}
		return ConfigureOpenVSwitchResult{}, path, fmt.Errorf("failed to start Open vSwitch reconciliation on node %s: %w", node.Name, err)
	}
	if startResponse == nil || startResponse.GetOperation() == nil {
		return ConfigureOpenVSwitchResult{}, path, errors.New("node agent returned an empty Open vSwitch reconciliation status")
	}

	operation := startResponse.GetOperation()
	for {
		result, done, resultErr := openVSwitchTerminalResult(input.OperationID, operation)
		if resultErr != nil || done {
			if resultErr == nil && desired != nil {
				resultErr = validateConfiguredOpenVSwitchResult(result, desired)
			}
			result.Path = path
			return result, path, resultErr
		}
		if err := waitForOpenVSwitchStatusPoll(ctx); err != nil {
			return ConfigureOpenVSwitchResult{}, path, err
		}
		statusResponse, statusErr := a.nodeAgentClient.GetOpenVSwitchReconciliationStatus(
			ctx,
			node,
			&nvkeproto.GetOpenVSwitchReconciliationStatusRequest{OperationId: input.OperationID},
		)
		if statusErr != nil {
			if ctx.Err() == nil && status.Code(statusErr) == codes.Unavailable {
				activity.GetLogger(ctx).Debug(
					"Open vSwitch status temporarily unavailable during network reconciliation",
					"nodeID", node.ID,
					"operationID", input.OperationID,
				)
				continue
			}
			return ConfigureOpenVSwitchResult{}, path, fmt.Errorf("failed to get accepted Open vSwitch operation %s from node %s: %w", input.OperationID, node.Name, statusErr)
		}
		if statusResponse == nil || statusResponse.GetOperation() == nil {
			return ConfigureOpenVSwitchResult{}, path, errors.New("node agent returned an empty Open vSwitch reconciliation status")
		}
		operation = statusResponse.GetOperation()
	}
}

func (a *Activities) startOpenVSwitchRPC(
	ctx context.Context,
	operationID string,
	node db.Node,
	desired *db.OpenVSwitchConfig,
) (*nvkeproto.StartOpenVSwitchReconciliationResponse, error) {
	if desired == nil {
		return a.nodeAgentClient.StartOpenVSwitchReconciliation(ctx, node,
			&nvkeproto.StartOpenVSwitchReconciliationRequest{OperationId: operationID})
	}
	return a.nodeAgentClient.StartConfiguredOpenVSwitchReconciliation(ctx, node,
		&nvkeproto.StartConfiguredOpenVSwitchReconciliationRequest{
			OperationId:     operationID,
			UplinkInterface: desired.UplinkInterface,
			BridgeName:      desired.BridgeName,
		})
}

func validateConfiguredOpenVSwitchResult(result ConfigureOpenVSwitchResult, desired *db.OpenVSwitchConfig) error {
	if result.UplinkInterface == desired.UplinkInterface && result.BridgeName == desired.BridgeName {
		return nil
	}
	return temporal.NewNonRetryableApplicationError(
		fmt.Sprintf("node agent reported Open vSwitch bridge %q, uplink %q; expected bridge %q, uplink %q",
			result.BridgeName, result.UplinkInterface, desired.BridgeName, desired.UplinkInterface),
		"OpenVSwitchConfigurationMismatch", nil,
	)
}

func (a *Activities) configureOpenVSwitchWithLegacyRunbook(
	ctx context.Context,
	input ConfigureOpenVSwitchInput,
	node db.Node,
) (ConfigureOpenVSwitchResult, error) {
	activity.GetLogger(ctx).Info(
		"StartOpenVSwitchReconciliation is unavailable; falling back to legacy Open vSwitch runbook",
		"nodeID", node.ID,
		"nodeName", node.Name,
	)
	if _, err := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
		HostID:      input.NodeID,
		RunbookName: legacyOpenVSwitchRunbook,
	}); err != nil {
		return ConfigureOpenVSwitchResult{}, fmt.Errorf("failed to execute legacy Open vSwitch runbook on node %s: %w", node.Name, err)
	}
	return ConfigureOpenVSwitchResult{Path: legacyOpenVSwitchRunbookPath}, nil
}

func openVSwitchTerminalResult(
	expectedOperationID string,
	operation *nvkeproto.OpenVSwitchReconciliationOperation,
) (ConfigureOpenVSwitchResult, bool, error) {
	operationStatus := operation.GetOperation()
	if operationStatus == nil {
		return ConfigureOpenVSwitchResult{}, true, errors.New("open vSwitch operation status is empty")
	}
	if operationStatus.GetOperationId() != expectedOperationID {
		return ConfigureOpenVSwitchResult{}, true, fmt.Errorf(
			"node agent returned Open vSwitch operation ID %q, expected %q",
			operationStatus.GetOperationId(),
			expectedOperationID,
		)
	}
	switch operationStatus.GetPhase() {
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING:
		return ConfigureOpenVSwitchResult{}, false, nil
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED:
		result := operation.GetResult()
		if result == nil {
			return ConfigureOpenVSwitchResult{}, true, errors.New("successful Open vSwitch operation has no result")
		}
		if result.GetOutcome() != nvkeproto.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED &&
			result.GetOutcome() != nvkeproto.OpenVSwitchReconciliationResult_OUTCOME_UNCHANGED {
			return ConfigureOpenVSwitchResult{}, true, fmt.Errorf("node agent returned Open vSwitch outcome %s", result.GetOutcome())
		}
		return ConfigureOpenVSwitchResult{
			BridgeName:      result.GetBridgeName(),
			UplinkInterface: result.GetUplinkInterface(),
			NetworkBackend:  result.GetNetworkBackend().String(),
		}, true, nil
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED:
		// FIXME: https://github.com/dsx-ai-factory/nvke/issues/1542 — report retryability for OVS failures.
		// Explicit OVS retries return the saved failure without rerunning setup.
		// Keep operation IDs stable until the agent supports safe recovery.
		return ConfigureOpenVSwitchResult{}, true, fmt.Errorf(
			"open vSwitch operation %s failed at %s: %s",
			operationStatus.GetOperationId(),
			operationStatus.GetStep(),
			operationStatus.GetErrorMessage(),
		)
	case nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED:
		return ConfigureOpenVSwitchResult{}, true, fmt.Errorf(
			"open vSwitch operation %s returned invalid phase %s",
			operationStatus.GetOperationId(),
			operationStatus.GetPhase(),
		)
	default:
		return ConfigureOpenVSwitchResult{}, true, fmt.Errorf(
			"open vSwitch operation %s returned unknown phase %d",
			operationStatus.GetOperationId(),
			operationStatus.GetPhase(),
		)
	}
}

func waitForOpenVSwitchStatusPoll(ctx context.Context) error {
	timer := time.NewTimer(openVSwitchStatusPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
