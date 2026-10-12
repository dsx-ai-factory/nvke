// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
)

var ErrNotFound = errors.New("record not found")

const (
	ConditionStatusTrue    ConditionStatus = "True"
	ConditionStatusFalse   ConditionStatus = "False"
	ConditionStatusUnknown ConditionStatus = "Unknown"
)

type ConditionStatus string

func ConditionStatusFromBool(status bool) ConditionStatus {
	if status {
		return ConditionStatusTrue
	}
	return ConditionStatusFalse
}

func BoolFromConditionStatus(status ConditionStatus) bool {
	return status == ConditionStatusTrue
}

type Condition struct {
	Status             ConditionStatus `json:"status,omitempty"`
	Message            string          `json:"message,omitempty"`
	LastTransitionTime time.Time       `json:"lastTransitionTime,omitempty"`
}

// Equal reports whether two conditions describe the same state. It compares
// LastTransitionTime with time.Time.Equal rather than ==, because == also
// compares the monotonic clock reading and the *Location pointer: the same
// instant read back from JSONB (UTC) and taken from an apimachinery
// metav1.Time (Local, see its UnmarshalJSON) is not == equal.
func (c Condition) Equal(other Condition) bool {
	return c.Status == other.Status &&
		c.Message == other.Message &&
		c.LastTransitionTime.Equal(other.LastTransitionTime)
}

func NewCondition(status bool, message string) Condition {
	return Condition{
		Status:             ConditionStatusFromBool(status),
		Message:            message,
		LastTransitionTime: time.Now(),
	}
}

// SetCondition updates existing to match condition. LastTransitionTime is set
// to now when the status changes, and is preserved otherwise. This follows the
// same transition-time behavior as apimachinery's meta.SetStatusCondition.
func SetCondition(existing *Condition, condition Condition) {
	if existing.Status != condition.Status || existing.LastTransitionTime.IsZero() {
		if condition.LastTransitionTime.IsZero() {
			existing.LastTransitionTime = time.Now()
		} else {
			existing.LastTransitionTime = condition.LastTransitionTime
		}
	}

	existing.Status = condition.Status
	existing.Message = condition.Message
}

// OperationType identifies a long-running cluster mutation that is mutually
// exclusive with other CP-mutating operations. Persisted as the Type field of
// UpdateInProgress inside clusters.conditions JSONB; string values must stay
// stable because they are consumed by the UI and nvkectl.
type OperationType string

const (
	OperationTypeEtcdEncryption   OperationType = "etcd_encryption"
	OperationTypeK8sUpgrade       OperationType = "k8s_upgrade"
	OperationTypeCPNodesRecycling OperationType = "cp_nodes_recycling"
	OperationTypeOIDCEnable       OperationType = "oidc_enable"
)

// UpdateInProgress represents a mutually-exclusive cluster mutation in flight.
// Stored inside ClusterConditions JSONB; nil means no update in progress.
// A single UpdateInProgress entry replaces the proliferation of per-operation
// boolean flags (ControlPlaneUpgrading, CPNodesRecycling, etc.) so new
// operation types can be added without schema changes.
type UpdateInProgress struct {
	Type OperationType `json:"type"`
	// Message is a short, operator-facing string. It is echoed verbatim in the
	// 409 response body on UpdateCluster conflicts, so writers MUST NOT put
	// internal identifiers (workflow IDs, activity IDs, stack traces, or any
	// other sensitive detail) here. Keep it to a human-readable summary.
	Message   string    `json:"message,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

func notFoundErrIfNoRowsAffected(res sql.Result) error {
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

func isViolationOfConstraint(err error, constraintName string) bool {
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		if pgErr.IntegrityViolation() {
			// https://www.postgresql.org/docs/current/protocol-error-fields.html
			// Field 'n' is the name of the constraint that was violated
			if pgErr.Field('n') == constraintName {
				return true
			}
		}
	}

	return false
}
