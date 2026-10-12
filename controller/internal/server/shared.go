// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func apiConditionFromDB(dbCondition db.Condition) api.Condition {
	var status api.ConditionStatus
	switch dbCondition.Status {
	case db.ConditionStatusTrue:
		status = api.ConditionStatusTrue
	case db.ConditionStatusFalse:
		status = api.ConditionStatusFalse
	case db.ConditionStatusUnknown:
		status = api.ConditionStatusUnknown
	default:
		status = api.ConditionStatusUnknown
	}

	return api.Condition{
		Status:             status,
		LastTransitionTime: dbCondition.LastTransitionTime,
		Message:            dbCondition.Message,
	}
}
