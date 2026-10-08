//go:build integration_test

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

type workflowHistory struct {
	Events []struct {
		ID      json.Number `json:"eventId"`
		Started *struct {
			WorkflowType struct {
				Name string `json:"name"`
			} `json:"workflowType"`
			Input workflowPayloads `json:"input"`
		} `json:"workflowExecutionStartedEventAttributes"`
		Scheduled *struct {
			ActivityType struct {
				Name string `json:"name"`
			} `json:"activityType"`
			Input workflowPayloads `json:"input"`
		} `json:"activityTaskScheduledEventAttributes"`
		Completed *struct {
			ScheduledEventID json.Number `json:"scheduledEventId"`
		} `json:"activityTaskCompletedEventAttributes"`
	} `json:"events"`
}

type workflowPayloads struct {
	Payloads []struct {
		Data []byte `json:"data"`
	} `json:"payloads"`
}

func (e environment) verifyWorkflow(ctx context.Context, workflowID, clusterID string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.temporal, "workflow", "show", "--workflow-id", workflowID,
		"--address", e.temporalAddress, "--namespace", e.temporalNamespace,
		"--output", "json", "--no-json-shorthand-payloads", "--disable-config-file", "--disable-config-env")
	data, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("read Temporal workflow %s: %w", workflowID, err)
	}
	var history workflowHistory
	if err := json.Unmarshal(data, &history); err != nil {
		return fmt.Errorf("decode Temporal history: %w", err)
	}
	started := false
	var scheduledID json.Number
	for _, event := range history.Events {
		if start := event.Started; start != nil {
			if start.WorkflowType.Name != "ProvisionCluster" {
				return fmt.Errorf("unexpected workflow type %q", start.WorkflowType.Name)
			}
			if err := verifyClusterPayload(start.Input, "clusterID", clusterID); err != nil {
				return fmt.Errorf("workflow input: %w", err)
			}
			started = true
		}
		if scheduled := event.Scheduled; scheduled != nil && scheduled.ActivityType.Name == "ApplyCAPIClusterResources" {
			if err := verifyClusterPayload(scheduled.Input, "ClusterID", clusterID); err != nil {
				return fmt.Errorf("CAPI activity input: %w", err)
			}
			scheduledID = event.ID
		}
		if event.Completed != nil && started && scheduledID != "" && event.Completed.ScheduledEventID == scheduledID {
			return nil
		}
	}
	return fmt.Errorf("waiting for ApplyCAPIClusterResources to complete in %s", workflowID)
}

func verifyClusterPayload(input workflowPayloads, fieldName, clusterID string) error {
	if len(input.Payloads) != 1 {
		return fmt.Errorf("expected one ClusterID payload, found %d", len(input.Payloads))
	}
	var fields map[string]string
	if err := json.Unmarshal(input.Payloads[0].Data, &fields); err != nil {
		return fmt.Errorf("decode ClusterID payload: %w", err)
	}
	if len(fields) != 1 || fields[fieldName] != clusterID {
		return fmt.Errorf("payload must contain only the expected %s", fieldName)
	}
	return nil
}
