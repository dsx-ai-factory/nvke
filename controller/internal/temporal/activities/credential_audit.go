// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/credentialaudit"
)

// recordCredentialAudit is activity-side IO only: adding observations to existing
// activities changes no Temporal commands, including histories replayed after an upgrade.
func (a *Activities) recordCredentialAudit(ctx context.Context, event credentialaudit.Event, operationID string) {
	if a.credentialAudit == nil {
		return
	}
	event.Timestamp = time.Now().UTC()
	event.Plane = credentialaudit.PlaneT2
	event.Actor.Kind = credentialaudit.ActorOperatorUnattributed
	event.Correlation.OperationHash = credentialaudit.Hash("operation", operationID)
	if activity.IsActivity(ctx) {
		info := activity.GetInfo(ctx)
		event.Correlation.WorkflowHash = credentialaudit.Hash("workflow", info.WorkflowExecution.ID)
		event.Correlation.RunHash = credentialaudit.Hash("run", info.WorkflowExecution.RunID)
		event.Correlation.ActivityHash = credentialaudit.Hash("activity", info.ActivityID)
		event.Correlation.Attempt = info.Attempt
		if event.Credential == credentialaudit.TypeKubeadm && scheduledCredentialAuditWorkflow(info.WorkflowExecution.ID, event.Target.ClusterID) {
			event.Actor.Kind = credentialaudit.ActorAutomatic
		}
	}
	a.credentialAudit.Record(ctx, event)
}

// scheduledCredentialAuditWorkflow identifies the scheduler's naming convention,
// not an authenticated human actor. A direct Temporal caller can choose this ID.
func scheduledCredentialAuditWorkflow(workflowID, clusterID string) bool {
	cycle, ok := strings.CutPrefix(workflowID, constants.RenewK8sCertsWorkflowPrefix+clusterID+"-")
	if !ok {
		return false
	}
	if base, retry, found := strings.Cut(cycle, "-retry-"); found {
		if _, err := strconv.ParseInt(retry, 10, 64); err != nil {
			return false
		}
		cycle = base
	}
	if cycle == "initial" {
		return true
	}
	_, err := strconv.ParseInt(cycle, 10, 64)
	return err == nil
}

func credentialOperationAudit(clusterID uuid.UUID, nodeID *uuid.UUID, action, credential, outcome, reason string) credentialaudit.Event {
	target := credentialaudit.Target{ClusterID: clusterID.String()}
	if nodeID != nil {
		target.NodeID = nodeID.String()
	}
	return credentialaudit.Event{Target: target, Action: action, Credential: credential, Outcome: outcome, Reason: reason}
}

func auditOperationOutcome(err error) (string, string) {
	if err != nil {
		return credentialaudit.OutcomeFailure, credentialaudit.ReasonFailed
	}
	return credentialaudit.OutcomeSuccess, credentialaudit.ReasonCompleted
}
