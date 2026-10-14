// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/workflow"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestLabelRemovals(t *testing.T) {
	t.Parallel()

	t.Run("default domain removes one form", func(t *testing.T) {
		t.Parallel()
		k := keys.New("", "")
		require.Equal(t,
			[]string{"nke.nvidia.com/reboot-", "nke.nvidia.com/reboot-attempt-"},
			labelRemovals(k.Reboot, k.RebootAttempt))
	})

	t.Run("custom domain removes the legacy form too", func(t *testing.T) {
		t.Parallel()
		k := keys.New("nvke.example.com", "")
		require.Equal(t,
			[]string{"nvke.example.com/reboot-", "nke.nvidia.com/reboot-"},
			labelRemovals(k.Reboot))
	})
}

func completeRebootTestWorkflow(ctx workflow.Context, input RebootNodeInput, withSuccess bool) error {
	if !withSuccess {
		return completeReboot(ctx, input, constants.RebootStatusFailed, nil)
	}
	return completeReboot(ctx, input, constants.RebootStatusCompleted, &rebootSuccessDetails{
		node: corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{"nke.nvidia.com/reboot": "legacy-reason"},
		}},
		workflowStartTime: workflow.Now(ctx),
		rebootAttempt:     1,
	})
}

// The reboot workflow writes its status labels under the domain captured in its
// input, and clears the request labels in every form so a request raised under
// the legacy domain does not outlive a change of domain.
func TestCompleteRebootHonoursLabelDomain(t *testing.T) {
	for _, tc := range []struct {
		name        string
		domain      string
		withSuccess bool
		want        []string
	}{
		{
			name:   "default domain, failed",
			domain: "",
			want: []string{
				"nke.nvidia.com/reboot-status=Failed",
				"nke.nvidia.com/reboot-status-timestamp=",
				"nke.nvidia.com/reboot-started-",
				"nke.nvidia.com/reboot-",
				"nke.nvidia.com/reboot-attempt-",
			},
		},
		{
			name:        "custom domain, completed",
			domain:      "nvke.example.com",
			withSuccess: true,
			want: []string{
				"nvke.example.com/reboot-status=Completed",
				"nvke.example.com/reboot-status-timestamp=",
				"nvke.example.com/last-completed-reboot=",
				"nvke.example.com/reboot-started-", "nke.nvidia.com/reboot-started-",
				"nvke.example.com/reboot-", "nke.nvidia.com/reboot-",
				"nvke.example.com/reboot-attempt-", "nke.nvidia.com/reboot-attempt-",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, a := setupRebootNodeWorkflowEnv(t)
			env.RegisterActivity(a.DeleteCAPIMachineRebootAnnotation)
			env.RegisterActivity(a.LabelNode)

			var got []string
			env.OnActivity(a.DeleteCAPIMachineRebootAnnotation, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.LabelNode, mock.Anything, mock.Anything).Return(
				func(_ context.Context, input activities.LabelNodeInput) (activities.LabelNodeResult, error) {
					got = input.Labels
					return activities.LabelNodeResult{}, nil
				},
			)
			env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil)

			env.ExecuteWorkflow(completeRebootTestWorkflow, RebootNodeInput{NodeID: uuid.New(), LabelDomain: tc.domain}, tc.withSuccess)

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			require.Len(t, got, len(tc.want))
			for i, want := range tc.want {
				// Timestamps vary, so compare the key part of "key=value" entries
				// up to and including the "=".
				if idx := len(want) - 1; want[idx] == '=' {
					require.Contains(t, got[i], want, "label %d", i)
					continue
				}
				require.Equal(t, want, got[i], "label %d", i)
			}
		})
	}
}

// A zero-domain input is what a workflow recorded before the domain became
// configurable carries. It must keep meaning the legacy domain.
func TestRebootNodeInputWithoutDomainIsLegacy(t *testing.T) {
	t.Parallel()

	require.Equal(t, "nke.nvidia.com/reboot", RebootNodeInput{}.labelKeys().Reboot.String())
	require.Equal(t, "nke.nvidia.com/maintenance", keys.New(NodeReconfigureKubeletServerInput{}.LabelDomain, "").MaintenanceTaint.String())
}
