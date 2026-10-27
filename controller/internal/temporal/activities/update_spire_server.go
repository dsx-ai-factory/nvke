// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// ReconfigureNodeSpireAgentInput names the node whose SPIRE agent is pointed
// at the SPIRE server Address:Port.
type ReconfigureNodeSpireAgentInput struct {
	NodeID  uuid.UUID
	Address string
	Port    int
}

// ReconfigureNodeSpireAgentResult is empty; the activity succeeds or errors.
type ReconfigureNodeSpireAgentResult struct{}

// ReconfigureNodeSpireAgent points one node's SPIRE agent at a SPIRE server
// through its node agent, which returns once the SPIRE agent holds an SVID from
// that server or gives up.
func (a *Activities) ReconfigureNodeSpireAgent(ctx context.Context, input ReconfigureNodeSpireAgentInput) (ReconfigureNodeSpireAgentResult, error) {
	if input.Port < 1 || input.Port > 65535 {
		return ReconfigureNodeSpireAgentResult{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf("invalid SPIRE server port %d", input.Port), "InvalidInput", nil)
	}
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return ReconfigureNodeSpireAgentResult{}, temporal.NewNonRetryableApplicationError("node not found in database", "db.ErrNotFound", err)
		}
		return ReconfigureNodeSpireAgentResult{}, fmt.Errorf("failed to get node: %w", err)
	}

	response, err := a.nodeAgentClient.ReconfigureSpireAgent(ctx, node, &nvkeproto.ReconfigureSpireAgentRequest{
		ServerAddress: input.Address,
		ServerPort:    uint32(input.Port),
	})
	if err != nil {
		return ReconfigureNodeSpireAgentResult{}, fmt.Errorf("failed to reconfigure SPIRE agent on node %s: %w", node.Name, err)
	}

	switch response.GetOutcome() {
	case nvkeproto.ReconfigureSpireAgentResponse_OUTCOME_APPLIED,
		nvkeproto.ReconfigureSpireAgentResponse_OUTCOME_UNCHANGED:
		return ReconfigureNodeSpireAgentResult{}, nil
	case nvkeproto.ReconfigureSpireAgentResponse_OUTCOME_FAILED:
		return ReconfigureNodeSpireAgentResult{}, fmt.Errorf("SPIRE agent on node %s did not get an SVID from the new server: %s", node.Name, response.GetErrorMessage())
	case nvkeproto.ReconfigureSpireAgentResponse_OUTCOME_UNSPECIFIED:
		return ReconfigureNodeSpireAgentResult{}, fmt.Errorf("node agent returned an unspecified SPIRE agent outcome for node %s", node.Name)
	default:
		return ReconfigureNodeSpireAgentResult{}, fmt.Errorf("node agent returned unknown SPIRE agent outcome %d for node %s", response.GetOutcome(), node.Name)
	}
}

// ClearSpireServerUpdateInProgressInput identifies the cluster whose
// spire_server_update lock is released. A non-empty FailureMessage records the
// failure on the cluster; an empty one clears any prior UpdateFailed.
type ClearSpireServerUpdateInProgressInput struct {
	ClusterID      uuid.UUID
	FailureMessage string
}

// ClearSpireServerUpdateInProgressResult is empty; the activity succeeds or errors.
type ClearSpireServerUpdateInProgressResult struct{}

// ClearSpireServerUpdateInProgress releases the spire_server_update lock the
// PATCH /clusters/{clusterID}/spire-server handler took, recording a failure or
// clearing UpdateFailed. It never clears another operation's lock.
func (a *Activities) ClearSpireServerUpdateInProgress(ctx context.Context, input ClearSpireServerUpdateInProgressInput) (ClearSpireServerUpdateInProgressResult, error) {
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, input.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		uip := cluster.Conditions.UpdateInProgress
		if uip == nil || uip.Type != db.OperationTypeSpireServerUpdate {
			return nil
		}
		db.ClearUpdateInProgress(&cluster)
		if input.FailureMessage != "" {
			db.SetUpdateFailed(&cluster, input.FailureMessage)
		} else {
			db.ClearUpdateFailed(&cluster)
		}

		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("failed to clear SPIRE server update progress: %w", err)
		}
		return nil
	})
	if err != nil {
		return ClearSpireServerUpdateInProgressResult{}, err
	}
	return ClearSpireServerUpdateInProgressResult{}, nil
}
