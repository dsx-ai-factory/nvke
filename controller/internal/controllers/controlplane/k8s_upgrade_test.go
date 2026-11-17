// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	temporalmocks "go.temporal.io/sdk/mocks"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// The API raises ControlPlaneUpgrading when it stores the target, so while the
// condition is true the reconciler finishes the kubeadm ConfigMap update before it
// checks whether the rollout is complete.
func TestReconcileK8sUpgrade_UpdatesKubeadmConfigMapWhileUpgrading(t *testing.T) {
	t.Parallel()

	clusterID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	const version = "1.37.2"
	workflowID := constants.UpdateKubeadmCMWorkflowPrefix + clusterID.String() + "-" + version

	tests := []struct {
		name string
		// describe is what DescribeWorkflowExecution returns for the ConfigMap workflow.
		describe    func() (*workflowservice.DescribeWorkflowExecutionResponse, error)
		wantStart   bool // the ConfigMap workflow is started
		wantRequeue bool
		// wantErrContains is set when the reconcile reaches reconcileUpgradeInProgress:
		// with no control-plane nodes the add-on upgrade fails, which marks that it ran.
		wantErrContains string
	}{
		{
			name: "starts the ConfigMap workflow and waits",
			describe: func() (*workflowservice.DescribeWorkflowExecutionResponse, error) {
				return nil, serviceerror.NewNotFound("workflow not found")
			},
			wantStart:   true,
			wantRequeue: true,
		},
		{
			name: "waits while the ConfigMap workflow runs",
			describe: func() (*workflowservice.DescribeWorkflowExecutionResponse, error) {
				return &workflowservice.DescribeWorkflowExecutionResponse{
					WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enums.WORKFLOW_EXECUTION_STATUS_RUNNING},
				}, nil
			},
			wantRequeue: true,
		},
		{
			name: "checks the rollout once the ConfigMap workflow completed",
			describe: func() (*workflowservice.DescribeWorkflowExecutionResponse, error) {
				return &workflowservice.DescribeWorkflowExecutionResponse{
					WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{
						CloseTime: timestamppb.New(time.Now()),
						Status:    enums.WORKFLOW_EXECUTION_STATUS_COMPLETED,
					},
				}, nil
			},
			wantErrContains: "no control plane nodes found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			temporalClient := &temporalmocks.Client{}
			resp, err := tt.describe()
			temporalClient.On("DescribeWorkflowExecution", mock.Anything, workflowID, "").Return(resp, err).Once()
			var workflowRun *temporalmocks.WorkflowRun
			if tt.wantStart {
				workflowRun = &temporalmocks.WorkflowRun{}
				workflowRun.On("GetRunID").Return("run").Maybe()
				temporalClient.On("ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(workflowRun, nil).Once()
			}

			r := &reconciler{
				cfg:                      config.Config{TemporalTaskQueue: "test-task-queue"},
				logger:                   slog.New(slog.DiscardHandler),
				controllerTemporalClient: temporalClient,
			}
			cluster := db.Cluster{
				ID:                clusterID,
				Name:              "test-cluster",
				KubernetesVersion: version,
				Conditions: db.ClusterConditions{
					ControlPlaneUpgrading: db.NewCondition(true, "upgrading control plane to "+version),
				},
			}

			err = r.reconcileK8sUpgrade(t.Context(), cluster, &controlPlane{})
			if tt.wantErrContains != "" {
				require.ErrorContains(t, err, tt.wantErrContains)
			} else {
				require.NoError(t, err)
			}
			if tt.wantRequeue {
				assert.Equal(t, upgradeWaitRequeueInterval, r.requeue)
			} else {
				assert.Zero(t, r.requeue)
			}
			temporalClient.AssertExpectations(t)
		})
	}
}
