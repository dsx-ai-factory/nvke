// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"time"

	"github.com/google/uuid"
)

// EtcdKeyRotationState is the state of the last data-encryption-key rotation.
type EtcdKeyRotationState string

const (
	EtcdKeyRotationStateRunning   EtcdKeyRotationState = "running"
	EtcdKeyRotationStateSucceeded EtcdKeyRotationState = "succeeded"
	EtcdKeyRotationStateFailed    EtcdKeyRotationState = "failed"
)

// EtcdKeyRotationPhase identifies a resumable rotation phase without key material.
type EtcdKeyRotationPhase string

const (
	EtcdKeyRotationPhasePrepare        EtcdKeyRotationPhase = "prepare"
	EtcdKeyRotationPhaseAddSecondary   EtcdKeyRotationPhase = "add_secondary"
	EtcdKeyRotationPhasePromotePrimary EtcdKeyRotationPhase = "promote_primary"
	EtcdKeyRotationPhaseRewrite        EtcdKeyRotationPhase = "rewrite"
	EtcdKeyRotationPhaseRemoveOldKeys  EtcdKeyRotationPhase = "remove_old_keys"
	EtcdKeyRotationPhaseFinalize       EtcdKeyRotationPhase = "finalize"
)

// Valid reports whether a phase belongs to the fixed lifecycle vocabulary.
func (p EtcdKeyRotationPhase) Valid() bool {
	switch p {
	case EtcdKeyRotationPhasePrepare, EtcdKeyRotationPhaseAddSecondary,
		EtcdKeyRotationPhasePromotePrimary, EtcdKeyRotationPhaseRewrite,
		EtcdKeyRotationPhaseRemoveOldKeys, EtcdKeyRotationPhaseFinalize:
		return true
	default:
		return false
	}
}

// EtcdKeyRotationFailureReason limits diagnostics to secret-safe reason codes.
type EtcdKeyRotationFailureReason string

const (
	EtcdKeyRotationFailureOperation EtcdKeyRotationFailureReason = "operation_failed"
	EtcdKeyRotationFailureCancelled EtcdKeyRotationFailureReason = "canceled"
)

// EtcdKeyRotationStatus records the last rotation in clusters.conditions JSONB.
// WorkflowID is also its recovery owner: a failure in the prepare phase releases
// UpdateInProgress, while failures after prepare retain it until the original
// workflow is reset and successfully completes. Observers must
// not equate a failed/closed workflow with safely released mutation ownership.
// Keys, ciphertext, Secret names and free-form errors are deliberately absent.
type EtcdKeyRotationStatus struct {
	WorkflowID     string                       `json:"workflowID"`
	State          EtcdKeyRotationState         `json:"state"`
	Phase          EtcdKeyRotationPhase         `json:"phase"`
	StartedAt      time.Time                    `json:"startedAt"`
	LastProgressAt time.Time                    `json:"lastProgressAt"`
	CompletedAt    *time.Time                   `json:"completedAt,omitempty"`
	FailureReason  EtcdKeyRotationFailureReason `json:"failureReason,omitempty"`
	// RetiredNodes lists control-plane nodes an operator declared permanently
	// lost while this failed rotation held the lock. Only their removal may pass
	// the retained fence; the rotation must then converge on the survivors.
	RetiredNodes []RetiredEtcdKeyRotationNode `json:"retiredNodes,omitempty"`
}

// RetiredEtcdKeyRotationNode records one operator-declared lost node.
type RetiredEtcdKeyRotationNode struct {
	NodeID    uuid.UUID            `json:"nodeID"`
	RetiredAt time.Time            `json:"retiredAt"`
	Phase     EtcdKeyRotationPhase `json:"phase"`
}

// RetiresNode reports whether nodeID was declared lost for this rotation.
func (s *EtcdKeyRotationStatus) RetiresNode(nodeID uuid.UUID) bool {
	if s == nil {
		return false
	}
	for _, retired := range s.RetiredNodes {
		if retired.NodeID == nodeID {
			return true
		}
	}
	return false
}
