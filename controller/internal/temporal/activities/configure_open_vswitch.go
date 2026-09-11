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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/metrics"
)

const (
	legacyOpenVSwitchRunbook      = "configure-openvswitch"
	openVSwitchStatusPollInterval = 2 * time.Second
	openVSwitchReconciliationPath = "rpc"
	legacyOpenVSwitchRunbookPath  = "legacy_runbook"
)

// ConfigureOpenVSwitchInput identifies the node whose default-route interface
// should be moved onto an Open vSwitch bridge.
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
// observed state. Only Unimplemented selects an older execution mechanism: an
// availability or timeout error may mean the accepted operation is moving the
// live connection onto the bridge.
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
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()

	startResponse, err := a.nodeAgentClient.StartOpenVSwitchReconciliation(
		ctx,
		node,
		&nkeproto.StartOpenVSwitchReconciliationRequest{OperationId: input.OperationID},
	)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			result, fallbackErr := a.configureOpenVSwitchWithLegacyRunbook(ctx, input, node)
			return result, legacyOpenVSwitchRunbookPath, fallbackErr
		}
		return ConfigureOpenVSwitchResult{}, path, fmt.Errorf("failed to start Open vSwitch reconciliation on node %s: %w", node.Name, err)
	}
	if startResponse == nil || startResponse.GetOperation() == nil {
		return ConfigureOpenVSwitchResult{}, path, errors.New("node agent returned an empty Open vSwitch operation")
	}

	operation := startResponse.GetOperation()
	for {
		result, done, resultErr := openVSwitchTerminalResult(input.OperationID, operation)
		if resultErr != nil || done {
			result.Path = path
			return result, path, resultErr
		}
		if err := waitForOpenVSwitchStatusPoll(ctx); err != nil {
			return ConfigureOpenVSwitchResult{}, path, err
		}
		statusResponse, statusErr := a.nodeAgentClient.GetOpenVSwitchReconciliationStatus(
			ctx,
			node,
			&nkeproto.GetOpenVSwitchReconciliationStatusRequest{OperationId: input.OperationID},
		)
		if statusErr != nil {
			if ctx.Err() == nil && status.Code(statusErr) == codes.Unavailable {
				activity.GetLogger(ctx).Debug(
					"Open vSwitch status temporarily unavailable while the default route moves",
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
	operation *nkeproto.OpenVSwitchReconciliationOperation,
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
	case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING:
		return ConfigureOpenVSwitchResult{}, false, nil
	case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED:
		result := operation.GetResult()
		if result == nil {
			return ConfigureOpenVSwitchResult{}, true, errors.New("successful Open vSwitch operation has no result")
		}
		if result.GetOutcome() != nkeproto.OpenVSwitchReconciliationResult_OUTCOME_CONFIGURED &&
			result.GetOutcome() != nkeproto.OpenVSwitchReconciliationResult_OUTCOME_UNCHANGED {
			return ConfigureOpenVSwitchResult{}, true, fmt.Errorf("node agent returned Open vSwitch outcome %s", result.GetOutcome())
		}
		return ConfigureOpenVSwitchResult{
			BridgeName:      result.GetBridgeName(),
			UplinkInterface: result.GetUplinkInterface(),
			NetworkBackend:  result.GetNetworkBackend().String(),
		}, true, nil
	case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED:
		return ConfigureOpenVSwitchResult{}, true, fmt.Errorf(
			"open vSwitch operation %s failed at %s: %s",
			operationStatus.GetOperationId(),
			operationStatus.GetStep(),
			operationStatus.GetErrorMessage(),
		)
	case nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED:
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
