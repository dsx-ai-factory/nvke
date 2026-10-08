// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"errors"
)

// ErrControlPlaneMutationBlocked reports retained credential mutation ownership.
var ErrControlPlaneMutationBlocked = errors.New("control-plane changes are rejected while etcd encryption or certificate renewal is in progress; retry after the operation completes")

// ControlPlaneMutationBlocked fences membership and configuration writers while
// etcd encryption or certificate renewal owns the cluster, including failed
// renewals that retain ownership, and legacy histories.
// Callers admitting mutations must check it under the cluster row lock before
// locking node/pool rows. API requests are rejected and must be resubmitted;
// controllers and workflows leave their durable intent for a later pass.
// Cluster deletion takes precedence: once it has started, teardown is never
// fenced, and credential operations stop instead.
func ControlPlaneMutationBlocked(cluster Cluster) bool {
	if cluster.Conditions.Deleting.Status == ConditionStatusTrue {
		return false
	}
	update := cluster.Conditions.UpdateInProgress
	return update != nil && (update.Type == OperationTypeEtcdEncryption || update.Type == OperationTypeCertRenewal)
}
