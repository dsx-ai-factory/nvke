// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal

import (
	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/rollout"
)

// Activity names used by workflow.ExecuteActivity.
const (
	ActivityIsSatisfied             = "rollout.is_satisfied"
	ActivityValidate                = "rollout.validate"
	ActivityClusterPreflight        = "rollout.cluster_preflight"
	ActivityNodePreflight           = "rollout.node_preflight"
	ActivityBuildApplyToNodePayload = "rollout.build_apply_to_node_payload"
	ActivityApplyToNode             = "rollout.apply_to_node"
	ActivityRollbackNode            = "rollout.rollback_node"
	ActivityCheckPostCond           = "rollout.check_post_condition"
	ActivityNodeGuardPreApply       = "rollout.node_guard_pre_apply"
	ActivityNodeGuardCheckPreApply  = "rollout.node_guard_check_pre_apply"
	ActivityNodeGuardPostApply      = "rollout.node_guard_post_apply"
	ActivityNodeGuardCheckPostApply = "rollout.node_guard_check_post_apply"
	ActivityClusterGuardPreRollout  = "rollout.cluster_guard_pre_rollout"
	ActivityClusterGuardPostRollout = "rollout.cluster_guard_post_rollout"
)

// Response types pack business errors into a *Err string and return
// (resp, nil). sdk-go#1201 strips the response on non-nil error, which
// would lose any partial-state Payload needed for cleanup. (nil, err)
// is reserved for infrastructure failures.

type IsSatisfiedRequest struct {
	OperationName string
	ClusterID     uuid.UUID
}

type IsSatisfiedResponse struct {
	Satisfied bool
	IsSatErr  string
}

type ValidateRequest struct {
	OperationName string
	ClusterID     uuid.UUID
}

type BuildApplyToNodePayloadRequest struct {
	OperationName string
	ClusterID     uuid.UUID
}

type BuildApplyToNodePayloadResponse struct {
	Payload  rollout.ApplyToNodePayload
	BuildErr string
}

type ApplyToNodeRequest struct {
	OperationName string
	ClusterID     uuid.UUID
	NodeID        uuid.UUID
	Payload       rollout.ApplyToNodePayload
}

type ApplyToNodeResponse struct {
	Outcome  rollout.Outcome
	ApplyErr string
}

type RollbackRequest struct {
	OperationName string
	ClusterID     uuid.UUID
	NodeID        uuid.UUID
	Payload       rollout.ApplyToNodePayload
}

type CheckPostConditionRequest struct {
	OperationName string
	ClusterID     uuid.UUID
}

type ClusterPreflightRequest struct {
	PreflightName string
	Params        map[string]string
	ClusterID     uuid.UUID
}

type NodePreflightRequest struct {
	PreflightName string
	Params        map[string]string
	ClusterID     uuid.UUID
	NodeID        uuid.UUID
}

type NodeGuardPreApplyRequest struct {
	GuardName string
	Params    map[string]string
	ClusterID uuid.UUID
	NodeID    uuid.UUID
}

type NodeGuardPreApplyResponse struct {
	State                  rollout.GuardState
	CheckPreApplyRequired  bool
	CheckPostApplyRequired bool
	PreApplyErr            string
}

type NodeGuardCheckPreApplyRequest struct {
	GuardName string
	Params    map[string]string
	ClusterID uuid.UUID
	NodeID    uuid.UUID
	State     rollout.GuardState
}

type NodeGuardCheckPreApplyResponse struct {
	Done     bool
	CheckErr string
}

// NodeGuardPostApplyRequest carries the State envelope returned by
// PreApply. The activity asserts State.GuardName matches GuardName.
type NodeGuardPostApplyRequest struct {
	GuardName string
	Params    map[string]string
	ClusterID uuid.UUID
	NodeID    uuid.UUID
	Outcome   rollout.Outcome
	State     rollout.GuardState
}

type NodeGuardCheckPostApplyRequest struct {
	GuardName string
	Params    map[string]string
	ClusterID uuid.UUID
	NodeID    uuid.UUID
	Outcome   rollout.Outcome
	State     rollout.GuardState
}

type NodeGuardCheckPostApplyResponse struct {
	Done     bool
	CheckErr string
}

type ClusterGuardPreRolloutRequest struct {
	GuardName string
	Params    map[string]string
	ClusterID uuid.UUID
}

type ClusterGuardPreRolloutResponse struct {
	State         rollout.GuardState
	PreRolloutErr string
}

// ClusterGuardPostRolloutRequest carries the State envelope returned
// by PreRollout. The activity asserts State.GuardName matches GuardName.
type ClusterGuardPostRolloutRequest struct {
	GuardName string
	Params    map[string]string
	ClusterID uuid.UUID
	Result    rollout.Result
	State     rollout.GuardState
}
