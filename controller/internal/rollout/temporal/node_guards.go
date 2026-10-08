// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal

import (
	"errors"
	"fmt"

	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
)

// RunWithNodeGuards brackets apply with guards exactly as the rollout executor
// brackets ApplyToNode: PreApply (and CheckPreApply) in declaration order, apply,
// then PostApply in reverse order on a disconnected context so cancellation or an
// apply failure cannot strand an open guard. ctx selects the options of the
// guard activities only; apply must set its own options on its own context.
//
// The error joins the apply error with any guard failure. A guard that opened
// is unwound even when a later guard's PreApply fails, and apply is not called
// in that case.
func RunWithNodeGuards(
	ctx workflow.Context,
	cluster rollout.Cluster,
	node rollout.Node,
	guards []rollout.NodeGuardRef,
	apply func() error,
) error {
	l := &nodeLoop{ctx: ctx, cluster: cluster, spec: rollout.RolloutSpec{NodeGuards: guards}}
	opened, guardName, preApplyErr := l.openNodeGuards(node)
	if preApplyErr != nil {
		err := fmt.Errorf("node guard %q PreApply: %w", guardName, preApplyErr)
		if unwindErr := runNodeGuardPostApply(ctx, cluster, node, opened, rollout.OutcomeFailedRestored); unwindErr != nil {
			err = errors.Join(err, unwindErr)
		}
		return err
	}

	applyErr := apply()
	outcome := rollout.OutcomeApplied
	if applyErr != nil {
		outcome = rollout.OutcomeFailedNotRestored
	}
	if postErr := runNodeGuardPostApply(ctx, cluster, node, opened, outcome); postErr != nil {
		return errors.Join(applyErr, postErr)
	}
	return applyErr
}
