// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodescheduling

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestUnit_ServiceRetryAfter(t *testing.T) {
	require.Equal(t, 3*time.Minute, NewService(config.Config{
		ControllerManagerInterval: 3 * time.Minute,
	}, slog.Default(), nil, nil, nil).RetryAfter())

	require.Equal(t, 2*time.Minute, NewService(config.Config{}, slog.Default(), nil, nil, nil).RetryAfter())
}

func TestUnit_ServiceScheduleCAPIInfraNodesStartsProvisionWorkflows(t *testing.T) {
	ctx := context.Background()
	firstNodeID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	secondNodeID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	temporalClient := &mockWorkflowStarter{}
	temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
		return opts.TaskQueue == "test-task-queue" &&
			opts.ID == constants.ProvisionNodeWorkflowPrefix+firstNodeID.String() &&
			opts.WorkflowIDConflictPolicy == enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING &&
			opts.WorkflowIDReusePolicy == enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY
	}), mock.Anything, mock.MatchedBy(func(input workflows.ProvisionNodeInput) bool {
		return input.NodeID == firstNodeID && input.RebootNodeWaitForUptimeTimeout == 7*time.Minute
	})).Return(mockWorkflowRun{id: "first"}, nil).Once()
	temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
		return opts.TaskQueue == "test-task-queue" &&
			opts.ID == constants.ProvisionNodeWorkflowPrefix+secondNodeID.String()
	}), mock.Anything, mock.MatchedBy(func(input workflows.ProvisionNodeInput) bool {
		return input.NodeID == secondNodeID && input.RebootNodeWaitForUptimeTimeout == 7*time.Minute
	})).Return(mockWorkflowRun{id: "second"}, nil).Once()

	service := NewService(config.Config{
		TemporalTaskQueue:         "test-task-queue",
		NodeRebootMaxTimeout:      7 * time.Minute,
		ControllerManagerInterval: time.Minute,
	}, slog.Default(), nil, temporalClient, nil)

	result, err := service.ScheduleCAPIInfraNodes(ctx, slog.Default(), []db.Node{
		{ID: firstNodeID},
		{ID: secondNodeID},
	})
	require.NoError(t, err)
	require.Zero(t, result)
	temporalClient.AssertExpectations(t)
}

func TestUnit_ServiceScheduleForgeNodesLimitsByCapacityAndUpdatesUnschedulableNodes(t *testing.T) {
	ctx := context.Background()
	cluster := nicoCluster(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	subnetID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	instanceTypeID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	firstNodeID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	secondNodeID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	createdAt := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	databaseTx := &testutil.MockTx{}
	database := &testutil.MockDB{GetTx: func() db.Tx { return databaseTx }}
	// The control-plane reservation reads site pools and pending control-plane
	// nodes on every pass; empty results mean "no reservation", which is what
	// these capacity tests assert against. Maybe() so tests that never reach
	// the reservation still pass AssertExpectations.
	database.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Maybe()
	database.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{}, nil).Maybe()
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	databaseTx.On("GetNodeForUpdate", mock.Anything, secondNodeID).Return(db.Node{
		ID: secondNodeID,
		Conditions: db.NodeConditions{
			Pending: db.Condition{Status: db.ConditionStatusTrue},
		},
	}, nil).Once()
	databaseTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		return node.ID == secondNodeID &&
			node.Conditions.Pending.Message == "Forge resources unavailable: pending since 2026-08-21T12:00:00Z"
	})).Return(db.Node{}, nil).Once()

	temporalClient := &mockWorkflowStarter{}
	temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
		return opts.ID == constants.ProvisionNodeWorkflowPrefix+firstNodeID.String()
	}), mock.Anything, mock.MatchedBy(func(input workflows.ProvisionNodeInput) bool {
		return input.NodeID == firstNodeID
	})).Return(mockWorkflowRun{id: "first"}, nil).Once()

	service := NewService(config.Config{
		TemporalTaskQueue:         "test-task-queue",
		ControllerManagerInterval: 4 * time.Minute,
	}, slog.Default(), database, temporalClient, &fakeForgeReader{
		instanceType: instanceTypeWithUnusedUsable(1),
		subnet:       subnet(subnetID.String(), "10.0.0.0", 30),
	})

	result, err := service.ScheduleForgeNodes(ctx, slog.Default(), cluster, []db.Node{
		{ID: firstNodeID, CreatedAt: createdAt},
		{ID: secondNodeID, CreatedAt: createdAt},
	}, instanceTypeID, ForgePlacementSpec{SubnetIDs: []uuid.UUID{subnetID}}, ForgeScheduleOptions{})
	require.NoError(t, err)
	require.Equal(t, 4*time.Minute, result.RequeueAfter)
	database.AssertExpectations(t)
	databaseTx.AssertExpectations(t)
	temporalClient.AssertExpectations(t)
}

func TestUnit_ServiceScheduleForgeNodesRequeuesOnlyTemporaryForgeResourceErrors(t *testing.T) {
	ctx := context.Background()
	cluster := nicoCluster(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	nodeID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	instanceTypeID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	service := NewService(config.Config{ControllerManagerInterval: 4 * time.Minute}, slog.Default(), nil, nil, &fakeForgeReader{
		instanceTypeErr: errors.New("cache expired"),
	})
	result, err := service.ScheduleForgeNodes(ctx, slog.Default(), cluster, []db.Node{{ID: nodeID}}, instanceTypeID, ForgePlacementSpec{}, ForgeScheduleOptions{})
	require.NoError(t, err)
	require.Equal(t, 4*time.Minute, result.RequeueAfter)

	service = NewService(config.Config{ControllerManagerInterval: 4 * time.Minute}, slog.Default(), nil, nil, &fakeForgeReader{
		instanceType: instanceTypeWithUnusedUsable(1),
		subnet:       subnet(uuid.New().String(), "not-a-cidr-prefix", 30),
	})
	result, err = service.ScheduleForgeNodes(ctx, slog.Default(), cluster, []db.Node{{ID: nodeID}}, instanceTypeID, ForgePlacementSpec{SubnetIDs: []uuid.UUID{uuid.New()}}, ForgeScheduleOptions{})
	require.Error(t, err)
	require.Zero(t, result)
	require.ErrorContains(t, err, "failed to parse forge subnet")
}

type mockWorkflowStarter struct {
	mock.Mock
}

func (m *mockWorkflowStarter) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error) {
	callArgs := make([]interface{}, 0, 3+len(args))
	callArgs = append(callArgs, ctx, options, workflow)
	callArgs = append(callArgs, args...)
	ret := m.Called(callArgs...)
	run, _ := ret.Get(0).(client.WorkflowRun)
	return run, ret.Error(1)
}

type mockWorkflowRun struct {
	id    string
	runID string
}

func (m mockWorkflowRun) GetID() string {
	return m.id
}

func (m mockWorkflowRun) GetRunID() string {
	return m.runID
}

func (m mockWorkflowRun) Get(_ context.Context, _ interface{}) error {
	return nil
}

type fakeForgeReader struct {
	instanceType    *forgesdk.InstanceType
	instanceTypeErr error
	instances       []*forgesdk.Instance
	instancesErr    error
	subnet          *forgesdk.Subnet
	subnetErr       error
	vpcPrefix       *forgesdk.VpcPrefix
	vpcPrefixErr    error
}

func (f *fakeForgeReader) GetInstanceTypeByID(context.Context, string, string, string) (*forgesdk.InstanceType, error) {
	return f.instanceType, f.instanceTypeErr
}

func (f *fakeForgeReader) GetInstances(context.Context, string, string) ([]*forgesdk.Instance, error) {
	return f.instances, f.instancesErr
}

func (f *fakeForgeReader) GetSubnetByID(context.Context, string, string, string) (*forgesdk.Subnet, error) {
	return f.subnet, f.subnetErr
}

func (f *fakeForgeReader) GetVpcPrefixByID(context.Context, string, string, string) (*forgesdk.VpcPrefix, error) {
	return f.vpcPrefix, f.vpcPrefixErr
}

func nicoCluster(id uuid.UUID) db.Cluster {
	siteID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	vpcID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	return db.Cluster{
		ID:           id,
		ProviderType: db.ProviderTypeNico,
		Nico: &db.NicoClusterProvider{
			OrgID:  "org-1",
			SiteID: siteID,
			VpcID:  vpcID,
		},
	}
}

func instanceTypeWithUnusedUsable(unusedUsable int32) *forgesdk.InstanceType {
	return &forgesdk.InstanceType{
		AllocationStats: &forgesdk.InstanceTypeAllocationStats{
			UnusedUsable: &unusedUsable,
		},
	}
}

func subnet(id, ipv4Prefix string, prefixLength int32) *forgesdk.Subnet {
	nullablePrefix := forgesdk.NullableString{}
	nullablePrefix.Set(&ipv4Prefix)
	return &forgesdk.Subnet{
		Id:           &id,
		Ipv4Prefix:   nullablePrefix,
		PrefixLength: &prefixLength,
	}
}
