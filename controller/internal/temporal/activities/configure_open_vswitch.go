// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/metrics"
)

const (
	legacyOpenVSwitchRunbook = "configure-openvswitch"
)

// ConfigureOpenVSwitchInput identifies the node whose default-route interface
// should be moved onto an Open vSwitch bridge.
type ConfigureOpenVSwitchInput struct {
	NodeID uuid.UUID
}

// ConfigureOpenVSwitchResult identifies the realized bridge and the
// implementation path that reconciled it.
type ConfigureOpenVSwitchResult struct {
	BridgeName      string
	UplinkInterface string
	NetworkBackend  string
	Path            string
}

// ConfigureOpenVSwitch asks the node agent to own the host-network mutation.
// Only Unimplemented selects the legacy runbook: an availability or timeout
// error may mean netplan already moved the live connection onto the bridge, so
// starting a second implementation would make the mutation ambiguous.
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
	path := "rpc"
	heartbeatCancel := startActivityHeartbeat(ctx)
	defer heartbeatCancel()
	response, err := a.nodeAgentClient.ReconcileOpenVSwitch(ctx, node, &nkeproto.ReconcileOpenVSwitchRequest{})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			path = "legacy_runbook"
			activity.GetLogger(ctx).Info(
				"ReconcileOpenVSwitch is unavailable; falling back to legacy Open vSwitch runbook",
				"nodeID", node.ID,
				"nodeName", node.Name,
			)
			if _, runbookErr := a.ExecuteHostRunbook(ctx, ExecuteHostRunbookInput{
				HostID:      input.NodeID,
				RunbookName: legacyOpenVSwitchRunbook,
			}); runbookErr != nil {
				return ConfigureOpenVSwitchResult{}, path, fmt.Errorf("failed to execute legacy Open vSwitch runbook on node %s: %w", node.Name, runbookErr)
			}
			return ConfigureOpenVSwitchResult{Path: path}, path, nil
		}
		return ConfigureOpenVSwitchResult{}, path, fmt.Errorf("failed to reconcile Open vSwitch on node %s: %w", node.Name, err)
	}
	if response == nil {
		return ConfigureOpenVSwitchResult{}, path, errors.New("node agent returned an empty Open vSwitch response")
	}
	if response.GetOutcome() != nkeproto.ReconcileOpenVSwitchResponse_OUTCOME_CONFIGURED &&
		response.GetOutcome() != nkeproto.ReconcileOpenVSwitchResponse_OUTCOME_UNCHANGED {
		return ConfigureOpenVSwitchResult{}, path, fmt.Errorf("node agent returned Open vSwitch outcome %s for node %s", response.GetOutcome(), node.Name)
	}

	return ConfigureOpenVSwitchResult{
		BridgeName:      response.GetBridgeName(),
		UplinkInterface: response.GetUplinkInterface(),
		NetworkBackend:  response.GetNetworkBackend().String(),
		Path:            path,
	}, path, nil
}
