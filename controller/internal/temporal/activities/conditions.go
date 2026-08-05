// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	"github.com/NVIDIA/nke/controller/internal/db"
)

type ConditionUpdate struct {
	Status  db.ConditionStatus
	Message string
}

func updateCondition(existing *db.Condition, update *ConditionUpdate) {
	if update == nil {
		return
	}
	db.SetCondition(existing, db.Condition{Status: update.Status, Message: update.Message})
}

// Wrap an activity with updates to a condition, at the start of the activity and on error.
type ActivityStatusConditionUpdateWrapper[R any] struct {
	Message         string
	UpdateCondition func(ctx context.Context, message string) error
	RenderError     func(error) string
	ActivityBody    func() (R, error)
}

func (w *ActivityStatusConditionUpdateWrapper[R]) Run(ctx context.Context) (R, error) {
	logger := activity.GetLogger(ctx)

	if w.Message == "" || w.UpdateCondition == nil {
		// If the condition message or updater are not set, run the activity body without updating any conditions.
		return w.ActivityBody()
	}

	if w.RenderError == nil {
		w.RenderError = func(err error) string {
			return err.Error()
		}
	}

	message := w.Message

	activityInfo := activity.GetInfo(ctx)
	if activityInfo.Attempt > 1 {
		message += fmt.Sprintf(
			" (%s, %s elapsed) -- retrying",
			failedAttemptsMessage(activityInfo.Attempt-1), // Current attempt hasn't failed yet
			elapsedSinceScheduled(activityInfo),
		)
	}

	if err := w.UpdateCondition(ctx, message); err != nil {
		return *new(R), fmt.Errorf("failed to update condition with activity start message: %w", err)
	}

	result, err := w.ActivityBody()
	if err != nil {
		message := w.Message
		message += fmt.Sprintf(
			" (%s, %s elapsed) -- last error: %s",
			failedAttemptsMessage(activityInfo.Attempt), // Current attempt has failed
			elapsedSinceScheduled(activityInfo),
			w.RenderError(err),
		)

		if err := w.UpdateCondition(ctx, message); err != nil {
			logger.Error("Failed to update condition with activity error message", "error", err)
		}
		return *new(R), err
	}

	return result, nil
}

func failedAttemptsMessage(failedAttempts int32) string {
	if failedAttempts == 1 {
		return "1 attempt has failed so far"
	}
	return fmt.Sprintf("%d attempts have failed so far", failedAttempts)
}

func elapsedSinceScheduled(activityInfo activity.Info) string {
	elapsedTime := time.Since(activityInfo.ScheduledTime).Round(time.Minute)
	var elapsedTimeString string
	if elapsedTime > time.Hour {
		elapsedTimeString = fmt.Sprintf("%dh%dm", elapsedTime/time.Hour, (elapsedTime%time.Hour)/time.Minute)
	} else {
		elapsedTimeString = fmt.Sprintf("%dm", elapsedTime/time.Minute)
	}
	return elapsedTimeString
}

func (a *Activities) nodeProvisioningConditionMessageUpdater(nodeID uuid.UUID) func(ctx context.Context, message string) error {
	return func(ctx context.Context, message string) error {
		return a.UpdateNodeConditions(ctx, NodeConditionsUpdate{
			NodeID: nodeID,
			Conditions: NodeConditions{
				// Setting NodeCondition.Provisioning to true implies that Pending is now false.
				Pending: &ConditionUpdate{
					Status:  db.ConditionStatusFalse,
					Message: "",
				},
				Provisioning: &ConditionUpdate{
					Status:  db.ConditionStatusTrue,
					Message: message,
				},
			},
		})
	}
}

func (a *Activities) nodeDeletingConditionMessageUpdater(nodeID uuid.UUID) func(ctx context.Context, message string) error {
	return func(ctx context.Context, message string) error {
		return a.UpdateNodeConditions(ctx, NodeConditionsUpdate{
			NodeID: nodeID,
			Conditions: NodeConditions{
				Deleting: &ConditionUpdate{
					Status:  db.ConditionStatusTrue,
					Message: message,
				},
			},
		})
	}
}

func (a *Activities) nodeRebootingConditionMessageUpdater(nodeID uuid.UUID) func(ctx context.Context, message string) error {
	return func(ctx context.Context, message string) error {
		return a.UpdateNodeConditions(ctx, NodeConditionsUpdate{
			NodeID: nodeID,
			Conditions: NodeConditions{
				Rebooting: &ConditionUpdate{
					Status:  db.ConditionStatusTrue,
					Message: message,
				},
			},
		})
	}
}

func (a *Activities) nodePoolProvisioningConditionMessageUpdater(nodePoolID uuid.UUID) func(ctx context.Context, message string) error {
	return func(ctx context.Context, message string) error {
		return a.UpdateNodePoolConditions(ctx, NodePoolConditionsUpdate{
			NodePoolID: nodePoolID,
			Conditions: NodePoolConditions{
				Provisioning: &ConditionUpdate{
					Status:  db.ConditionStatusTrue,
					Message: message,
				},
			},
		})
	}
}

func (a *Activities) nodePoolDeletingConditionMessageUpdater(nodePoolID uuid.UUID) func(ctx context.Context, message string) error {
	return func(ctx context.Context, message string) error {
		return a.UpdateNodePoolConditions(ctx, NodePoolConditionsUpdate{
			NodePoolID: nodePoolID,
			Conditions: NodePoolConditions{
				Deleting: &ConditionUpdate{
					Status:  db.ConditionStatusTrue,
					Message: message,
				},
			},
		})
	}
}

func (a *Activities) clusterProvisioningConditionMessageUpdater(clusterID uuid.UUID) func(ctx context.Context, message string) error {
	return func(ctx context.Context, message string) error {
		return a.UpdateClusterConditions(ctx, ClusterConditionsUpdate{
			ClusterID: clusterID,
			Conditions: ClusterConditions{
				Provisioning: &ConditionUpdate{
					Status:  db.ConditionStatusTrue,
					Message: message,
				},
			},
		})
	}
}

func (a *Activities) clusterDeletingConditionMessageUpdater(clusterID uuid.UUID) func(ctx context.Context, message string) error {
	return func(ctx context.Context, message string) error {
		return a.UpdateClusterConditions(ctx, ClusterConditionsUpdate{
			ClusterID: clusterID,
			Conditions: ClusterConditions{
				Deleting: &ConditionUpdate{
					Status:  db.ConditionStatusTrue,
					Message: message,
				},
			},
		})
	}
}
