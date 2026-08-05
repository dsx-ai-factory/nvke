// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package server

import (
	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/db"
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
