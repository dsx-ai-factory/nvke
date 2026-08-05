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
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"

	"github.com/NVIDIA/nke/controller/api"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

// Activity type names as recorded in Temporal history. The Go SDK registers a
// struct's methods under their bare method name (no receiver/package prefix),
// so these are matched against the short name (see shortActivityName).
const (
	activityExecuteNodeRunbookHook    = "ExecuteNodeRunbookHook"
	activityExecuteClusterRunbookHook = "ExecuteClusterRunbookHook"
)

//nolint:gochecknoglobals // read-only lookup tables, never mutated after init
var (
	// activitiesToSkip are internal bookkeeping activities not worth surfacing to users.
	activitiesToSkip = map[string]bool{
		"UpdateNodeConditions":       true,
		"UpdateNodePoolConditions":   true,
		"UpdateClusterConditions":    true,
		"UpdateNodeForgeDetails":     true,
		"GetNode":                    true,
		"GetNodePoolForNode":         true,
		"GetClusterForNode":          true,
		"GetNodeConfigProfile":       true,
		"GetNodeProvisioningContext": true,
		"GetTenantID":                true,
		"ListControlPlaneNodes":      true,
		"GetInstanceType":            true,
	}

	// runbookHookActivities maps activity names whose input carries a RunbookName
	// worth surfacing in the display label. Both the node- and cluster-scoped hook
	// activities share the same input shape.
	runbookHookActivities = map[string]bool{
		activityExecuteNodeRunbookHook:    true,
		activityExecuteClusterRunbookHook: true,
	}
)

func (s *nodeService) GetNodeHistory(ctx context.Context, req api.GetNodeHistoryRequestObject) (api.GetNodeHistoryResponseObject, error) {
	_, err := s.db.GetNode(ctx, req.NodeID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return api.GetNodeHistory404Response{}, nil
		}
		return nil, fmt.Errorf("failed to get node: %w", err)
	}

	if s.temporalClient == nil {
		return api.GetNodeHistory200JSONResponse{Workflows: []api.NodeHistoryWorkflow{}}, nil
	}

	nodeIDStr := req.NodeID.String()
	workflowIDs := []string{
		constants.ProvisionNodeWorkflowPrefix + nodeIDStr,
		constants.TeardownNodeWorkflowPrefix + nodeIDStr,
		constants.RebootNodeWorkflowPrefix + nodeIDStr,
		constants.RebootNodeUptimeWorkflowPrefix + nodeIDStr,
		constants.PatchNodeWorkflowPrefix + nodeIDStr,
		constants.ApplyK8sNodeConditionWorkflowPrefix + nodeIDStr,
	}

	var allWorkflows []api.NodeHistoryWorkflow

	for _, wfID := range workflowIDs {
		resp, err := s.temporalClient.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Query:    fmt.Sprintf("WorkflowId = %q", wfID),
			PageSize: 50,
		})
		if err != nil {
			s.logger.Warn("failed to list workflow executions", "workflowID", wfID, "error", err)
			continue
		}

		for _, exec := range resp.GetExecutions() {
			wfType := exec.GetType().GetName()
			startTime := exec.GetStartTime().AsTime()
			status := temporalWorkflowStatusToString(exec.GetStatus())

			var closeTime *time.Time
			var durationMs int64
			if ct := exec.GetCloseTime(); ct != nil && !ct.AsTime().IsZero() {
				t := ct.AsTime()
				closeTime = &t
				durationMs = t.Sub(startTime).Milliseconds()
			} else {
				durationMs = time.Since(startTime).Milliseconds()
			}

			activities, err := s.fetchNodeActivityHistory(ctx, exec.GetExecution().GetWorkflowId(), exec.GetExecution().GetRunId())
			if err != nil {
				s.logger.Warn("failed to fetch workflow history",
					"workflowID", exec.GetExecution().GetWorkflowId(),
					"runID", exec.GetExecution().GetRunId(),
					"error", err)
				activities = []api.NodeHistoryActivity{}
			}

			allWorkflows = append(allWorkflows, api.NodeHistoryWorkflow{
				WorkflowID:   exec.GetExecution().GetWorkflowId(),
				RunID:        exec.GetExecution().GetRunId(),
				WorkflowType: wfType,
				Status:       status,
				StartTime:    startTime,
				CloseTime:    closeTime,
				DurationMs:   durationMs,
				Activities:   activities,
			})
		}
	}

	sort.Slice(allWorkflows, func(i, j int) bool {
		return allWorkflows[i].StartTime.After(allWorkflows[j].StartTime)
	})

	return api.GetNodeHistory200JSONResponse{Workflows: allWorkflows}, nil
}

type pendingActivity struct {
	name              string
	displayName       string
	firstScheduledAt  time.Time
	latestScheduledID int64
}

func (s *nodeService) fetchNodeActivityHistory(ctx context.Context, workflowID, runID string) ([]api.NodeHistoryActivity, error) {
	iter := s.temporalClient.GetWorkflowHistory(ctx, workflowID, runID, false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)

	// pending tracks in-flight activities keyed by their latest scheduledEventId.
	// On retry, the same logical activity gets a new scheduledEventId, so we also
	// track by name to preserve the original start time across attempts.
	pendingByScheduleID := map[int64]*pendingActivity{}
	pendingByName := map[string]*pendingActivity{}

	activities := make([]api.NodeHistoryActivity, 0, 8)

	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			return activities, fmt.Errorf("error reading workflow history: %w", err)
		}

		switch event.GetEventType() { //nolint:exhaustive // only activity task events carry timing data relevant to users
		case enums.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			attrs := event.GetActivityTaskScheduledEventAttributes()
			if attrs == nil || attrs.GetActivityType() == nil {
				continue
			}
			name := attrs.GetActivityType().GetName()
			if activitiesToSkip[shortActivityName(name)] {
				continue
			}

			if existing, ok := pendingByName[name]; ok {
				// Retry: update the latest schedule ID but keep the original start time.
				delete(pendingByScheduleID, existing.latestScheduledID)
				existing.latestScheduledID = event.GetEventId()
				pendingByScheduleID[event.GetEventId()] = existing
			} else {
				pa := &pendingActivity{
					name:              name,
					displayName:       activityDisplayName(name) + activityDisplaySuffix(name, attrs.GetInput()),
					firstScheduledAt:  event.GetEventTime().AsTime(),
					latestScheduledID: event.GetEventId(),
				}
				pendingByName[name] = pa
				pendingByScheduleID[event.GetEventId()] = pa
			}

		case enums.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			attrs := event.GetActivityTaskCompletedEventAttributes()
			if attrs == nil {
				continue
			}
			pa, ok := pendingByScheduleID[attrs.GetScheduledEventId()]
			if !ok {
				continue
			}
			closeTime := event.GetEventTime().AsTime()
			durationMs := closeTime.Sub(pa.firstScheduledAt).Milliseconds()
			activities = append(activities, api.NodeHistoryActivity{
				Name:          pa.displayName,
				ScheduledTime: pa.firstScheduledAt,
				CloseTime:     &closeTime,
				DurationMs:    durationMs,
				Status:        "Completed",
			})
			delete(pendingByScheduleID, attrs.GetScheduledEventId())
			delete(pendingByName, pa.name)

		case enums.EVENT_TYPE_ACTIVITY_TASK_FAILED:
			attrs := event.GetActivityTaskFailedEventAttributes()
			if attrs == nil {
				continue
			}
			// Only record terminal failures; transient failures (IN_PROGRESS) will be retried.
			if attrs.GetRetryState() == enums.RETRY_STATE_IN_PROGRESS {
				continue
			}
			pa, ok := pendingByScheduleID[attrs.GetScheduledEventId()]
			if !ok {
				continue
			}
			closeTime := event.GetEventTime().AsTime()
			durationMs := closeTime.Sub(pa.firstScheduledAt).Milliseconds()
			failMsg := attrs.GetFailure().GetMessage()
			activities = append(activities, api.NodeHistoryActivity{
				Name:           pa.displayName,
				ScheduledTime:  pa.firstScheduledAt,
				CloseTime:      &closeTime,
				DurationMs:     durationMs,
				Status:         "Failed",
				FailureMessage: &failMsg,
			})
			delete(pendingByScheduleID, attrs.GetScheduledEventId())
			delete(pendingByName, pa.name)

		case enums.EVENT_TYPE_ACTIVITY_TASK_TIMED_OUT:
			attrs := event.GetActivityTaskTimedOutEventAttributes()
			if attrs == nil {
				continue
			}
			if attrs.GetRetryState() == enums.RETRY_STATE_IN_PROGRESS {
				continue
			}
			pa, ok := pendingByScheduleID[attrs.GetScheduledEventId()]
			if !ok {
				continue
			}
			closeTime := event.GetEventTime().AsTime()
			durationMs := closeTime.Sub(pa.firstScheduledAt).Milliseconds()
			msg := "timed out"
			if f := attrs.GetFailure(); f != nil && f.GetMessage() != "" {
				msg = f.GetMessage()
			}
			activities = append(activities, api.NodeHistoryActivity{
				Name:           pa.displayName,
				ScheduledTime:  pa.firstScheduledAt,
				CloseTime:      &closeTime,
				DurationMs:     durationMs,
				Status:         "Failed",
				FailureMessage: &msg,
			})
			delete(pendingByScheduleID, attrs.GetScheduledEventId())
			delete(pendingByName, pa.name)

		case enums.EVENT_TYPE_ACTIVITY_TASK_CANCELED:
			attrs := event.GetActivityTaskCanceledEventAttributes()
			if attrs == nil {
				continue
			}
			pa, ok := pendingByScheduleID[attrs.GetScheduledEventId()]
			if !ok {
				continue
			}
			closeTime := event.GetEventTime().AsTime()
			durationMs := closeTime.Sub(pa.firstScheduledAt).Milliseconds()
			activities = append(activities, api.NodeHistoryActivity{
				Name:          pa.displayName,
				ScheduledTime: pa.firstScheduledAt,
				CloseTime:     &closeTime,
				DurationMs:    durationMs,
				Status:        "Canceled",
			})
			delete(pendingByScheduleID, attrs.GetScheduledEventId())
			delete(pendingByName, pa.name)
		}
	}

	// Any activity still pending is currently running.
	for _, pa := range pendingByName {
		durationMs := time.Since(pa.firstScheduledAt).Milliseconds()
		activities = append(activities, api.NodeHistoryActivity{
			Name:          pa.displayName,
			ScheduledTime: pa.firstScheduledAt,
			DurationMs:    durationMs,
			Status:        "Running",
		})
	}

	return activities, nil
}

func temporalWorkflowStatusToString(status enums.WorkflowExecutionStatus) string {
	switch status {
	case enums.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED:
		return "Unknown"
	case enums.WORKFLOW_EXECUTION_STATUS_RUNNING:
		return "Running"
	case enums.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		return "Completed"
	case enums.WORKFLOW_EXECUTION_STATUS_FAILED:
		return "Failed"
	case enums.WORKFLOW_EXECUTION_STATUS_CANCELED:
		return "Canceled"
	case enums.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		return "Terminated"
	case enums.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW:
		return "ContinuedAsNew"
	case enums.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		return "TimedOut"
	default:
		return "Unknown"
	}
}

// activityDisplaySuffix returns an optional " - detail" suffix appended to an
// activity's display name to disambiguate otherwise-identical entries. For
// runbook hook activities it decodes the RunbookName from the scheduled input
// payload (e.g. "Execute Node Runbook Hook - approve-latest-node-csr"). It is
// best-effort: on any decode failure it returns an empty string.
func activityDisplaySuffix(name string, input *commonpb.Payloads) string {
	if !runbookHookActivities[shortActivityName(name)] || input == nil {
		return ""
	}
	var args struct {
		RunbookName string
	}
	if err := converter.GetDefaultDataConverter().FromPayloads(input, &args); err != nil {
		return ""
	}
	if args.RunbookName == "" {
		return ""
	}
	return " - " + args.RunbookName
}

// shortActivityName strips a leading "pkg." qualifier from a Temporal activity
// type name (e.g. "Activities.ExecuteNodeRunbookHook" -> "ExecuteNodeRunbookHook").
// The Go SDK records bare method names, but normalizing defends against either
// convention when matching against the lookup tables.
func shortActivityName(name string) string {
	if _, after, found := strings.Cut(name, "."); found {
		return after
	}
	return name
}

// activityDisplayName strips any "pkg." prefix and splits camelCase into
// words, keeping acronyms intact (e.g. "CreateCAPIInfraMachine" becomes
// "Create CAPI Infra Machine".
func activityDisplayName(name string) string {
	name = shortActivityName(name)
	runes := []rune(name)
	var b strings.Builder
	for i, r := range runes {
		// Insert a space before an uppercase letter when it begins a new word:
		// either it follows a non-uppercase character (camelCase boundary), or
		// it is the last uppercase of a run that is followed by a lowercase
		// letter (acronym-to-word boundary, e.g. the "P" in "CAPIMachine").
		if i > 0 && unicode.IsUpper(r) {
			prevLower := !unicode.IsUpper(runes[i-1])
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLower || nextLower {
				b.WriteRune(' ')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}
