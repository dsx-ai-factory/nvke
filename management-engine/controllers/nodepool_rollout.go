// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Rollout budgets are resolved here so node-pool creation and updates use the same limits.

package controllers

import (
	"math"
	"strconv"
	"strings"

	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// nodePoolRecycleBudget resolves the smallest continuous budget against desired replicas.
// Omission or zero stops selecting further nodes for recycling.
func nodePoolRecycleBudget(rollout *internalv1.Rollout, desiredReplicas int32) (int, error) {
	if rollout == nil {
		return 0, nil
	}
	strategy := rollout.Strategy
	if strategy.Type != "RollingUpdate" || len(strategy.Disruption.Budgets) == 0 {
		return 0, invalidNVKEControllerRequest("Node pool rollout requires RollingUpdate with at least one disruption budget")
	}
	recycleBudget := int64(math.MaxInt32)
	for _, budget := range strategy.Disruption.Budgets {
		numericPart := strings.TrimSuffix(budget.Nodes, "%")
		value, err := strconv.ParseUint(numericPart, 10, 31)
		if err != nil || strconv.FormatUint(value, 10) != numericPart {
			return 0, invalidNVKEControllerRequest("Node pool rollout budget nodes must be a nonnegative integer count or a percentage from 0% to 100%")
		}
		limit := int64(value)
		if strings.HasSuffix(budget.Nodes, "%") {
			if value > 100 {
				return 0, invalidNVKEControllerRequest("Node pool rollout budget percentage must be from 0% to 100%")
			}
			limit = (int64(desiredReplicas)*int64(value) + 99) / 100
		}
		recycleBudget = min(recycleBudget, limit)
	}
	return int(recycleBudget), nil
}
