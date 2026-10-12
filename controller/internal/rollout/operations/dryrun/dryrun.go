// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dryrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
)

// DryRunOperation implements every optional capability but mutates nothing
// (ApplyToNode returns OutcomeUnchanged).
type DryRunOperation struct {
	Logger *slog.Logger // nil -> slog.Default()
}

var (
	_ rollout.Operation                 = (*DryRunOperation)(nil)
	_ rollout.ApplyToNodePayloadBuilder = (*DryRunOperation)(nil)
	_ rollout.SatisfactionChecker       = (*DryRunOperation)(nil)
	_ rollout.Validator                 = (*DryRunOperation)(nil)
	_ rollout.RollbackHandler           = (*DryRunOperation)(nil)
	_ rollout.PostConditionChecker      = (*DryRunOperation)(nil)
)

const (
	OperationName = "RolloutDryRun"
)

func (op *DryRunOperation) logger() *slog.Logger {
	if op.Logger != nil {
		return op.Logger
	}
	return slog.Default()
}

func (op *DryRunOperation) Name() string { return OperationName }

func (op *DryRunOperation) ApplyToNode(
	_ context.Context, node rollout.Node, payload rollout.ApplyToNodePayload,
) (rollout.Outcome, error) {
	if node == nil {
		return rollout.OutcomeFailedRestored, errors.New("node is nil")
	}
	op.logger().Info("dry-run: apply-to-node",
		"node", node.Name(),
		"role", node.Role(),
		"endpoint", node.AgentEndpoint(),
		"artifact_bytes", len(payload),
	)
	return rollout.OutcomeUnchanged, nil
}

// BuildApplyToNodePayload returns a short synthetic payload so callers can see
// it delivered byte-identical to every ApplyToNode.
func (op *DryRunOperation) BuildApplyToNodePayload(
	_ context.Context, cluster rollout.Cluster,
) (rollout.ApplyToNodePayload, error) {
	if cluster == nil {
		return nil, errors.New("cluster is nil")
	}
	payload := fmt.Sprintf("dry-run artifact for cluster %s (%s)",
		cluster.Name(), cluster.ID())
	op.logger().Info("dry-run: build-artifact",
		"cluster", cluster.Name(),
		"bytes", len(payload),
	)
	return rollout.ApplyToNodePayload(payload), nil
}

// IsSatisfied always returns false so the per-node loop runs.
func (op *DryRunOperation) IsSatisfied(
	_ context.Context, cluster rollout.Cluster,
) (bool, error) {
	if cluster == nil {
		return false, errors.New("cluster is nil")
	}
	op.logger().Info("dry-run: is-satisfied",
		"cluster", cluster.Name(),
		"result", false,
	)
	return false, nil
}

func (op *DryRunOperation) Validate(
	_ context.Context, cluster rollout.Cluster,
) error {
	if cluster == nil {
		return errors.New("cluster is nil")
	}
	op.logger().Info("dry-run: validate",
		"cluster", cluster.Name(),
		"cp_nodes", len(cluster.ControlPlaneNodes()),
	)
	return nil
}

// Rollback is never invoked — ApplyToNode never returns a failure outcome.
func (op *DryRunOperation) Rollback(
	_ context.Context, node rollout.Node, _ rollout.ApplyToNodePayload,
) error {
	if node == nil {
		return errors.New("node is nil")
	}
	op.logger().Info("dry-run: rollback", "node", node.Name())
	return nil
}

func (op *DryRunOperation) CheckPostCondition(
	_ context.Context, cluster rollout.Cluster,
) error {
	if cluster == nil {
		return errors.New("cluster is nil")
	}
	op.logger().Info("dry-run: check-post-condition",
		"cluster", cluster.Name(),
	)
	return nil
}
