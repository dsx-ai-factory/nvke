// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

func TestRebootAndWaitCAPINode(t *testing.T) {
	timeoutErr := temporal.NewTimeoutError(enums.TIMEOUT_TYPE_SCHEDULE_TO_CLOSE, nil)
	for _, test := range []struct {
		name        string
		softErr     error
		waitErr     error
		lateBoot    bool
		withdrawn   bool
		deleting    bool
		terminating bool
		hardErr     error
		wantHard    bool
		wantErr     bool
	}{
		{name: "accepted soft reboot completes"},
		{name: "verification deadline escalates", waitErr: timeoutErr, wantHard: true},
		{name: "late boot avoids duplicate reboot", waitErr: timeoutErr, lateBoot: true},
		{name: "withdrawn request cancels escalation", waitErr: timeoutErr, withdrawn: true, wantErr: true},
		{name: "deleting node cancels escalation", waitErr: timeoutErr, deleting: true, wantErr: true},
		{name: "termination request cancels escalation", waitErr: timeoutErr, terminating: true, wantErr: true},
		{name: "canceled verification avoids hard reboot", waitErr: temporal.NewCanceledError(), wantErr: true},
		{name: "verification failure does not prove timeout", waitErr: temporal.NewNonRetryableApplicationError("cannot read boot state", "read", nil), wantErr: true},
		{name: "soft scheduling failure escalates", softErr: errors.New("agent rejected reboot"), wantHard: true},
		{name: "canceled scheduling avoids hard reboot", softErr: temporal.NewCanceledError(), wantErr: true},
		{name: "hard fallback failure is returned", waitErr: timeoutErr, hardErr: temporal.NewNonRetryableApplicationError("provider unavailable", "provider", nil), wantHard: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			env, a := setupRebootNodeWorkflowEnv(t)
			input := RebootNodeInput{
				NodeID: uuid.New(), InitialTimeout: 30 * time.Minute,
				MaxTimeout: time.Hour, PollingInterval: time.Minute, SoftRebootEnabled: true,
			}
			env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).Return(activities.SoftRebootNodeResult{}, test.softErr).Once()
			if test.softErr == nil {
				env.OnActivity(a.WaitForNodeReboot, mock.Anything, mock.MatchedBy(func(in activities.WaitForNodeRebootInput) bool {
					return in.PollingInterval == 10*time.Second && in.PreviousBootID == "old-boot"
				})).Return(func(ctx context.Context, _ activities.WaitForNodeRebootInput) (activities.WaitForNodeRebootResult, error) {
					require.Equal(t, 30*time.Minute, activity.GetInfo(ctx).ScheduleToCloseTimeout)
					return activities.WaitForNodeRebootResult{}, test.waitErr
				}).Once()
			}
			if (test.softErr != nil && !temporal.IsCanceledError(test.softErr)) || isRebootVerificationTimeout(test.waitErr) {
				node := rebootTestKubeNode("old-boot")
				if test.lateBoot {
					node.Status.NodeInfo.BootID = "new-boot"
				}
				if test.withdrawn {
					keys.Reboot.Delete(node.Labels)
				}
				if test.deleting {
					ts := metav1.Now()
					node.DeletionTimestamp = &ts
				}
				if test.terminating {
					node.Labels[keys.Terminate.String()] = "repair"
				}
				env.OnActivity(a.GetKubeNode, mock.Anything, mock.Anything).Return(activities.GetKubeNodeResult{Node: node}, nil).Once()
			}
			if test.wantHard {
				env.OnActivity(a.ApplyCAPIMachineRebootAnnotation, mock.Anything, mock.Anything).Return(
					activities.ApplyCAPIMachineRebootAnnotationResult{RebootStartedTime: time.Now()}, test.hardErr,
				).Once()
				if test.hardErr == nil {
					env.OnActivity(a.WaitForNodeReboot, mock.Anything, mock.MatchedBy(func(in activities.WaitForNodeRebootInput) bool {
						return in.PollingInterval == time.Minute && in.PreviousBootID == "old-boot"
					})).Return(activities.WaitForNodeRebootResult{BootID: "new-boot"}, nil).Once()
				}
			}
			env.ExecuteWorkflow(testRebootAndWaitCAPINodeWorkflow, input)
			require.True(t, env.IsWorkflowCompleted())
			if test.wantErr {
				require.Error(t, env.GetWorkflowError())
			} else {
				require.NoError(t, env.GetWorkflowError())
			}
			env.AssertExpectations(t)
			if !test.wantHard {
				env.AssertNotCalled(t, "ApplyCAPIMachineRebootAnnotation", mock.Anything, mock.Anything)
			}
		})
	}
}

func testRebootAndWaitCAPINodeWorkflow(ctx workflow.Context, input RebootNodeInput) error {
	return rebootAndWaitCAPINode(ctx, input, 1, "old-boot")
}

func TestSoftRebootVerificationWindow(t *testing.T) {
	for _, test := range []struct {
		name    string
		initial time.Duration
		want    time.Duration
	}{
		{name: "short configured window", initial: 45 * time.Second, want: 45 * time.Second},
		{name: "long configured window is capped", initial: time.Hour, want: 30 * time.Minute},
		{name: "missing window uses default", want: 30 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			env, a := setupRebootNodeWorkflowEnv(t)
			env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
			env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).Return(activities.SoftRebootNodeResult{}, nil).Once()
			env.OnActivity(a.WaitForNodeReboot, mock.Anything, mock.Anything).Return(
				func(ctx context.Context, _ activities.WaitForNodeRebootInput) (activities.WaitForNodeRebootResult, error) {
					require.Equal(t, test.want, activity.GetInfo(ctx).ScheduleToCloseTimeout)
					return activities.WaitForNodeRebootResult{BootID: "new-boot"}, nil
				},
			).Once()
			env.ExecuteWorkflow(testRebootAndWaitCAPINodeWorkflow, RebootNodeInput{NodeID: uuid.New(), InitialTimeout: test.initial, SoftRebootEnabled: true})
			require.NoError(t, env.GetWorkflowError())
			env.AssertExpectations(t)
		})
	}
}

func rebootTestKubeNode(bootID string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{keys.Reboot.String(): "repair"}},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: bootID}},
	}
}

func TestRebootNodeTimeoutEscalationRetainsHardStrategy(t *testing.T) {
	env, a := setupRebootNodeWorkflowEnv(t)
	env.RegisterActivity(a.GetNodePoolForNode)
	env.RegisterActivity(a.LabelNode)
	env.RegisterActivity(a.DeleteCAPIMachineRebootAnnotation)
	env.OnActivity(a.GetNodePoolForNode, mock.Anything, mock.Anything).Return(db.NodePool{}, nil).Once()
	env.OnActivity(a.LabelNode, mock.Anything, mock.Anything).Return(activities.LabelNodeResult{}, nil)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.GetKubeNode, mock.Anything, mock.Anything).Return(activities.GetKubeNodeResult{Node: rebootTestKubeNode("old-boot")}, nil)
	env.OnActivity(a.DeleteCAPIMachineRebootAnnotation, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).Return(activities.SoftRebootNodeResult{}, nil).Once()
	timeoutErr := temporal.NewTimeoutError(enums.TIMEOUT_TYPE_SCHEDULE_TO_CLOSE, nil)
	env.OnActivity(a.WaitForNodeReboot, mock.Anything, mock.MatchedBy(func(in activities.WaitForNodeRebootInput) bool {
		return in.PollingInterval == 10*time.Second
	})).Return(activities.WaitForNodeRebootResult{}, timeoutErr).Once()
	env.OnActivity(a.ApplyCAPIMachineRebootAnnotation, mock.Anything, mock.Anything).Return(
		activities.ApplyCAPIMachineRebootAnnotationResult{RebootStartedTime: time.Now()}, nil,
	).Twice()
	hardInput := mock.MatchedBy(func(in activities.WaitForNodeRebootInput) bool {
		return in.PollingInterval == time.Minute
	})
	env.OnActivity(a.WaitForNodeReboot, mock.Anything, hardInput).Return(activities.WaitForNodeRebootResult{}, timeoutErr).Once()
	env.OnActivity(a.WaitForNodeReboot, mock.Anything, hardInput).Return(activities.WaitForNodeRebootResult{BootID: "new-boot"}, nil).Once()
	env.ExecuteWorkflow(RebootNode, RebootNodeInput{
		NodeID: uuid.New(), InitialTimeout: 30 * time.Minute, TimeoutIncrement: 15 * time.Minute,
		MaxTimeout: time.Hour, PollingInterval: time.Minute, SoftRebootEnabled: true,
	})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestRebootCAPINode_SoftRebootSucceeds_NoHardFallback(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).Return(activities.SoftRebootNodeResult{}, nil).Once()

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID, true)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
	env.AssertNotCalled(t, "ApplyCAPIMachineRebootAnnotation", mock.Anything, mock.Anything)
}

func TestRebootCAPINode_SoftRebootFails_FallsBackToHard(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).Return(activities.SoftRebootNodeResult{}, errors.New("node-agent unreachable")).Once()
	env.OnActivity(a.ApplyCAPIMachineRebootAnnotation, mock.Anything, mock.Anything).Return(
		activities.ApplyCAPIMachineRebootAnnotationResult{RebootStartedTime: time.Now()}, nil,
	).Once()

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID, true)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func TestRebootCAPINode_SoftRebootCanceled_PropagatesWithoutFallback(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.SoftRebootNode, mock.Anything, mock.Anything).Return(
		activities.SoftRebootNodeResult{}, temporal.NewCanceledError(),
	).Once()

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID, true)

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	env.AssertExpectations(t)
	env.AssertNotCalled(t, "ApplyCAPIMachineRebootAnnotation", mock.Anything, mock.Anything)
}

// SoftRebootEnabled=false (CAPD, and the NICo containers lane) skips the soft RPC entirely.
func TestRebootCAPINode_SoftRebootDisabled_GoesStraightToHard(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.UpdateNodeConditions, mock.Anything, mock.Anything).Return(nil).Once()
	env.OnActivity(a.ApplyCAPIMachineRebootAnnotation, mock.Anything, mock.Anything).Return(
		activities.ApplyCAPIMachineRebootAnnotationResult{RebootStartedTime: time.Now()}, nil,
	).Once()

	env.ExecuteWorkflow(testRebootCAPINodeWorkflow, nodeID, false)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
	env.AssertNotCalled(t, "SoftRebootNode", mock.Anything, mock.Anything)
}

func TestWaitForNodeReboot_BootChangeDetected(t *testing.T) {
	nodeID := uuid.New()

	env, a := setupRebootNodeWorkflowEnv(t)
	env.OnActivity(a.WaitForNodeReboot, mock.Anything, mock.Anything).Return(activities.WaitForNodeRebootResult{BootID: "new-boot-id"}, nil).Once()

	input := RebootNodeInput{
		NodeID:          nodeID,
		InitialTimeout:  30 * time.Minute,
		MaxTimeout:      time.Hour,
		PollingInterval: time.Minute,
	}
	env.ExecuteWorkflow(testWaitForNodeRebootWorkflow, input)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func testRebootCAPINodeWorkflow(ctx workflow.Context, nodeID uuid.UUID, softRebootEnabled bool) error {
	_, err := rebootCAPINode(ctx, RebootNodeInput{NodeID: nodeID, SoftRebootEnabled: softRebootEnabled}, 1)
	return err
}

func testWaitForNodeRebootWorkflow(ctx workflow.Context, input RebootNodeInput) error {
	return waitForNodeReboot(ctx, input, 1, "previous-boot-id", workflow.Now(ctx).UTC())
}

func setupRebootNodeWorkflowEnv(t *testing.T) (*testsuite.TestWorkflowEnvironment, *activities.Activities) {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	env.SetWorkerOptions(worker.Options{
		DeadlockDetectionTimeout: 5 * time.Second,
	})

	a := &activities.Activities{}
	env.RegisterActivity(a.UpdateNodeConditions)
	env.RegisterActivity(a.SoftRebootNode)
	env.RegisterActivity(a.ApplyCAPIMachineRebootAnnotation)
	env.RegisterActivity(a.WaitForNodeReboot)
	env.RegisterActivity(a.GetKubeNode)

	return env, a
}

// A reboot request is applied by an operator or an external automation, so it
// keeps arriving under the legacy domain; a terminate request withdraws it in
// either domain too.
func TestRebootRequestActiveReadsBothDomains(t *testing.T) {
	tests := map[string]struct {
		labels map[string]string
		want   bool
	}{
		"no labels":                      {labels: nil},
		"current domain":                 {labels: map[string]string{keys.Reboot.String(): "repair"}, want: true},
		"legacy domain":                  {labels: map[string]string{keys.Reboot.Legacy(): "repair"}, want: true},
		"empty value is not a request":   {labels: map[string]string{keys.Reboot.String(): ""}},
		"terminate wins, current domain": {labels: map[string]string{keys.Reboot.String(): "repair", keys.Terminate.String(): "replace"}},
		"terminate wins, legacy domain":  {labels: map[string]string{keys.Reboot.String(): "repair", keys.Terminate.Legacy(): "replace"}},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: params.labels}}
			require.Equal(t, params.want, rebootRequestActive(node))
		})
	}
}

// Completion has to clear the request in both domains: clearing only the one
// this build writes would leave a legacy label that immediately restarts the
// workflow.
func TestRebootLabelRemovalCoversBothDomains(t *testing.T) {
	require.Equal(t,
		[]string{keys.Reboot.String() + "-", keys.Reboot.Legacy() + "-"},
		keys.Reboot.RemoveSelectors())
}
