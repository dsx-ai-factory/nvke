// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal_test

import (
	"context"
	"fmt"

	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
)

// Param keys for FailingNodeGuard / FailingClusterGuard.
const (
	// ParamFailPreApplyOn: node name on which PreApply errors.
	ParamFailPreApplyOn = "fail_preapply_on"
	// ParamFailPostApplyOn: node name on which PostApply errors.
	ParamFailPostApplyOn = "fail_postapply_on"
	// ParamPreApplyEmitPayload="1": pair the PreApply error with a
	// non-nil Payload to exercise the partial-state contract.
	ParamPreApplyEmitPayload = "preapply_emit_payload"
	// ParamFailPreRollout="1": FailingClusterGuard.PreRollout errors.
	ParamFailPreRollout = "fail_prerollout"
	// ParamFailPostRollout="1": FailingClusterGuard.PostRollout errors.
	ParamFailPostRollout = "fail_postrollout"
	// ParamPreRolloutPayload="1": pair the PreRollout error with a
	// non-nil Payload (cluster-guard partial-state path).
	ParamPreRolloutPayload = "prerollout_payload"
)

// FailingNodeGuard exercises every NodeGuard failure path via Params.
// Zero value is a no-op. Name() is set by the factory so one test
// suite can register the fixture under multiple consumer-specific keys.
type FailingNodeGuard struct {
	name                string
	failPreApplyOn      string
	failPostApplyOn     string
	preApplyEmitPayload bool
}

// NewFailingNodeGuardFactory builds a FailingNodeGuard reporting Name == name.
func NewFailingNodeGuardFactory(name string) rollout.NodeGuardFactory {
	return func(params map[string]string) (rollout.NodeGuard, error) {
		return &FailingNodeGuard{
			name:                name,
			failPreApplyOn:      params[ParamFailPreApplyOn],
			failPostApplyOn:     params[ParamFailPostApplyOn],
			preApplyEmitPayload: params[ParamPreApplyEmitPayload] == "1",
		}, nil
	}
}

func (g *FailingNodeGuard) Name() string { return g.name }

// PreApply errors when node.Name matches ParamFailPreApplyOn.
func (g *FailingNodeGuard) PreApply(
	_ context.Context, _ rollout.Cluster, node rollout.Node,
) (rollout.GuardState, error) {
	if g.failPreApplyOn != "" && g.failPreApplyOn == node.Name() {
		if g.preApplyEmitPayload {
			return rollout.GuardState{
					GuardName: g.name,
					Payload:   []byte("partial-state-token"),
				},
				fmt.Errorf("rollout/testing: failing-node-guard PreApply forced fail on %s (with payload)", node.Name())
		}
		return rollout.StatelessGuardState(g.name),
			fmt.Errorf("rollout/testing: failing-node-guard PreApply forced fail on %s", node.Name())
	}
	return rollout.StatelessGuardState(g.name), nil
}

// PostApply errors when node.Name matches ParamFailPostApplyOn.
func (g *FailingNodeGuard) PostApply(
	_ context.Context, _ rollout.Cluster, node rollout.Node,
	_ rollout.Outcome, _ rollout.GuardState,
) error {
	if g.failPostApplyOn != "" && g.failPostApplyOn == node.Name() {
		return fmt.Errorf("rollout/testing: failing-node-guard PostApply forced fail on %s", node.Name())
	}
	return nil
}

// FailingClusterGuard is the cluster-scope sibling of FailingNodeGuard.
type FailingClusterGuard struct {
	name              string
	failPreRollout    bool
	failPostRollout   bool
	preRolloutPayload bool
}

// NewFailingClusterGuardFactory builds a FailingClusterGuard reporting Name == name.
func NewFailingClusterGuardFactory(name string) rollout.ClusterGuardFactory {
	return func(params map[string]string) (rollout.ClusterGuard, error) {
		return &FailingClusterGuard{
			name:              name,
			failPreRollout:    params[ParamFailPreRollout] == "1",
			failPostRollout:   params[ParamFailPostRollout] == "1",
			preRolloutPayload: params[ParamPreRolloutPayload] == "1",
		}, nil
	}
}

func (g *FailingClusterGuard) Name() string { return g.name }

// PreRollout errors when ParamFailPreRollout is set.
func (g *FailingClusterGuard) PreRollout(_ context.Context, _ rollout.Cluster) (rollout.GuardState, error) {
	if g.failPreRollout {
		if g.preRolloutPayload {
			return rollout.GuardState{
					GuardName: g.name,
					Payload:   []byte("partial-state-token"),
				},
				fmt.Errorf("rollout/testing: failing-cluster-guard PreRollout forced fail (with payload)")
		}
		return rollout.StatelessGuardState(g.name),
			fmt.Errorf("rollout/testing: failing-cluster-guard PreRollout forced fail")
	}
	return rollout.StatelessGuardState(g.name), nil
}

// PostRollout errors when ParamFailPostRollout is set.
func (g *FailingClusterGuard) PostRollout(
	_ context.Context, _ rollout.Cluster, _ rollout.Result, _ rollout.GuardState,
) error {
	if g.failPostRollout {
		return fmt.Errorf("rollout/testing: failing-cluster-guard PostRollout forced fail")
	}
	return nil
}
