// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
)

func mustPayloads(t *testing.T, values ...any) *commonpb.Payloads {
	t.Helper()
	p, err := converter.GetDefaultDataConverter().ToPayloads(values...)
	if err != nil {
		t.Fatalf("failed to build payloads: %v", err)
	}
	return p
}

func TestActivityDisplaySuffix(t *testing.T) {
	t.Parallel()

	type runbookInput struct {
		NodeID      string
		HookPhase   string
		RunbookName string
	}

	tests := []struct {
		name     string
		activity string
		input    *commonpb.Payloads
		want     string
	}{
		{
			name:     "node runbook hook appends runbook name",
			activity: activityExecuteNodeRunbookHook,
			input:    mustPayloads(t, runbookInput{RunbookName: "approve-latest-node-csr", HookPhase: "post-provision"}),
			want:     " - approve-latest-node-csr",
		},
		{
			name:     "cluster runbook hook appends runbook name",
			activity: activityExecuteClusterRunbookHook,
			input:    mustPayloads(t, runbookInput{RunbookName: "drain-cluster"}),
			want:     " - drain-cluster",
		},
		{
			// Defends against a "pkg." prefixed activity name convention.
			name:     "prefixed runbook hook name still appends",
			activity: "Activities." + activityExecuteNodeRunbookHook,
			input:    mustPayloads(t, runbookInput{RunbookName: "install-chrony"}),
			want:     " - install-chrony",
		},
		{
			name:     "non-runbook activity has no suffix",
			activity: "Activities.CreateCAPIInfraMachine",
			input:    mustPayloads(t, runbookInput{RunbookName: "ignored"}),
			want:     "",
		},
		{
			name:     "empty runbook name yields no suffix",
			activity: activityExecuteNodeRunbookHook,
			input:    mustPayloads(t, runbookInput{RunbookName: ""}),
			want:     "",
		},
		{
			name:     "nil input yields no suffix",
			activity: activityExecuteNodeRunbookHook,
			input:    nil,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := activityDisplaySuffix(tt.activity, tt.input); got != tt.want {
				t.Errorf("activityDisplaySuffix(%q) = %q, want %q", tt.activity, got, tt.want)
			}
		})
	}
}

func TestActivityDisplayNameWithSuffix(t *testing.T) {
	t.Parallel()

	input := mustPayloads(t, struct{ RunbookName string }{RunbookName: "approve-latest-node-csr"})
	name := activityExecuteNodeRunbookHook
	got := activityDisplayName(name) + activityDisplaySuffix(name, input)
	want := "Execute Node Runbook Hook - approve-latest-node-csr"
	if got != want {
		t.Errorf("display name = %q, want %q", got, want)
	}
}
