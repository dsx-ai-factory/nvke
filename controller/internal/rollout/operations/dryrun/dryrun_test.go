// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package dryrun

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/rollout"
)

func TestDryRunOperationRejectsNilInputs(t *testing.T) {
	op := &DryRunOperation{}
	ctx := context.Background()

	outcome, err := op.ApplyToNode(ctx, nil, nil)
	assert.Equal(t, rollout.OutcomeFailedRestored, outcome)
	require.EqualError(t, err, "node is nil")

	_, err = op.BuildApplyToNodePayload(ctx, nil)
	require.EqualError(t, err, "cluster is nil")

	_, err = op.IsSatisfied(ctx, nil)
	require.EqualError(t, err, "cluster is nil")

	require.EqualError(t, op.Validate(ctx, nil), "cluster is nil")
	require.EqualError(t, op.Rollback(ctx, nil, nil), "node is nil")
	require.EqualError(t, op.CheckPostCondition(ctx, nil), "cluster is nil")
}
