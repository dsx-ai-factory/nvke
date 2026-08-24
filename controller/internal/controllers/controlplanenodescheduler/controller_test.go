// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplanenodescheduler

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/controllers/nodescheduling"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/temporal/workflows"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestUnit_ClusterKeysForManagedControlPlaneNode(t *testing.T) {
	clusterID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	nodePoolID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	require.Equal(t,
		[]dbreconcile.Key{dbreconcile.ClusterKey(clusterID)},
		clusterKeysForManagedControlPlaneNode(db.Node{ClusterID: clusterID, ControlPlane: true}),
	)
	require.Nil(t, clusterKeysForManagedControlPlaneNode(db.Node{ClusterID: clusterID}))
	require.Nil(t, clusterKeysForManagedControlPlaneNode(db.Node{ClusterID: clusterID, ControlPlane: true, NodePoolID: nodePoolID}))
	require.Nil(t, clusterKeysForManagedControlPlaneNode(db.Node{ControlPlane: true}))
}

func TestUnit_ControlPlaneNodeSchedulerReconcileCAPIStartsManagedPendingNodeWorkflows(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	managedNodeID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	legacyNodePoolID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	legacyNodeID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	pending := true
	deleting := false
	controlPlane := true

	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
		ID:           clusterID,
		ProviderType: db.ProviderTypeCAPI,
	}, nil).Once()
	database.On("ListNodes", mock.Anything, db.NodeFilters{
		ClusterID:    &clusterID,
		ControlPlane: &controlPlane,
		Pending:      &pending,
		Deleting:     &deleting,
	}).Return([]db.Node{
		{ID: managedNodeID, ClusterID: clusterID, ControlPlane: true},
		{ID: legacyNodeID, ClusterID: clusterID, ControlPlane: true, NodePoolID: legacyNodePoolID},
	}, nil).Once()

	temporalClient := &mockWorkflowStarter{}
	expectProvisionWorkflow(t, temporalClient, managedNodeID)

	scheduler := NewControlPlaneNodeScheduler(config.Config{
		TemporalTaskQueue:    "test-task-queue",
		NodeRebootMaxTimeout: 5 * time.Minute,
	}, slog.Default(), dbwrapper.New(database, slog.Default()), temporalClient, nil)

	result, err := scheduler.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))
	require.NoError(t, err)
	require.Zero(t, result)
	database.AssertExpectations(t)
	temporalClient.AssertExpectations(t)
}

func TestUnit_ControlPlaneNodeSchedulerSkipsDeletingCluster(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{
		ID: clusterID,
		Conditions: db.ClusterConditions{
			Deleting: db.Condition{Status: db.ConditionStatusTrue},
		},
	}, nil).Once()

	scheduler := NewControlPlaneNodeScheduler(config.Config{}, slog.Default(), dbwrapper.New(database, slog.Default()), nil, nil)
	result, err := scheduler.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))
	require.NoError(t, err)
	require.Zero(t, result)
	database.AssertExpectations(t)
	database.AssertNotCalled(t, "ListNodes", mock.Anything, mock.Anything)
}

func TestUnit_ControlPlaneNodeSchedulerReturnsErrorForUnresolvedNodeContext(t *testing.T) {
	ctx := context.Background()
	cluster := nicoCluster(uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), uuid.New(), uuid.New())
	node := db.Node{
		ID:           uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
		ClusterID:    uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"),
		ControlPlane: true,
	}

	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, node.ClusterID).Return(db.Cluster{}, errors.New("context lookup failed")).Once()

	scheduler := NewControlPlaneNodeScheduler(config.Config{}, slog.Default(), dbwrapper.New(database, slog.Default()), nil, nil)
	result, err := scheduler.scheduleForgeControlPlaneNodes(ctx, slog.Default(), cluster, []db.Node{node})
	require.Error(t, err)
	require.Zero(t, result)
	require.ErrorContains(t, err, "failed to resolve context")
	database.AssertExpectations(t)
}

func TestUnit_ControlPlaneNodeSchedulerRequeuesWhenNoPreferredInstanceTypeHasCapacity(t *testing.T) {
	ctx := context.Background()
	instanceTypeID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	subnetID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	cluster := nicoCluster(uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"), instanceTypeID, subnetID)
	node := db.Node{
		ID:           uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd"),
		ClusterID:    cluster.ID,
		ControlPlane: true,
	}

	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()

	scheduler := NewControlPlaneNodeScheduler(config.Config{
		ControllerManagerInterval: 4 * time.Minute,
	}, slog.Default(), dbwrapper.New(database, slog.Default()), nil, &fakeForgeReader{
		instanceType: instanceTypeWithUnusedUsable(0),
		subnet:       subnet(subnetID.String(), "10.0.0.0", 30),
	})

	result, err := scheduler.scheduleForgeControlPlaneNodes(ctx, slog.Default(), cluster, []db.Node{node})
	require.NoError(t, err)
	require.Equal(t, 4*time.Minute, result.RequeueAfter)
	database.AssertExpectations(t)
}

func TestUnit_ControlPlaneNodeSchedulerReturnsErrorForDeterministicForgeResourceFailures(t *testing.T) {
	ctx := context.Background()
	instanceTypeID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	subnetID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	cluster := nicoCluster(uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"), instanceTypeID, subnetID)
	node := db.Node{
		ID:           uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd"),
		ClusterID:    cluster.ID,
		ControlPlane: true,
	}

	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()

	scheduler := NewControlPlaneNodeScheduler(config.Config{
		ControllerManagerInterval: 4 * time.Minute,
	}, slog.Default(), dbwrapper.New(database, slog.Default()), nil, &fakeForgeReader{
		instanceType: instanceTypeWithUnusedUsable(1),
		subnet:       subnet(subnetID.String(), "not-a-cidr-prefix", 30),
	})

	result, err := scheduler.scheduleForgeControlPlaneNodes(ctx, slog.Default(), cluster, []db.Node{node})
	require.Error(t, err)
	require.Zero(t, result)
	require.ErrorContains(t, err, "failed to parse forge subnet")
	database.AssertExpectations(t)
}

func TestUnit_ControlPlaneNodeSchedulerRequeuesForTemporaryForgeResourceFailures(t *testing.T) {
	ctx := context.Background()
	instanceTypeID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	subnetID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	cluster := nicoCluster(uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"), instanceTypeID, subnetID)
	node := db.Node{
		ID:           uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd"),
		ClusterID:    cluster.ID,
		ControlPlane: true,
	}

	database := &testutil.MockDB{}
	database.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil).Once()

	scheduler := NewControlPlaneNodeScheduler(config.Config{
		ControllerManagerInterval: 4 * time.Minute,
	}, slog.Default(), dbwrapper.New(database, slog.Default()), nil, &fakeForgeReader{
		instanceTypeErr: errors.New("cache expired"),
	})

	result, err := scheduler.scheduleForgeControlPlaneNodes(ctx, slog.Default(), cluster, []db.Node{node})
	require.NoError(t, err)
	require.Equal(t, 4*time.Minute, result.RequeueAfter)
	database.AssertExpectations(t)
}

func TestUnit_SelectControlPlaneInstanceTypeEmptyPreference(t *testing.T) {
	cluster := db.Cluster{
		ID:           uuid.New(),
		ProviderType: db.ProviderTypeNico,
		Nico: &db.NicoClusterProvider{
			OrgID:  "org-1",
			SiteID: uuid.New(),
			VpcID:  uuid.New(),
		},
		ControlPlaneConfig: db.ControlPlaneConfig{
			ControlPlaneConfigSpec: nicoControlPlaneConfigSpec(nil),
		},
	}
	scheduler := NewControlPlaneNodeScheduler(config.Config{}, slog.Default(), dbwrapper.New(&testutil.MockDB{}, slog.Default()), nil, nil)
	acct := newControlPlaneCapacityAccount(scheduler, slog.Default())

	_, unavailable, err := scheduler.selectControlPlaneInstanceType(context.Background(), cluster, nil, nodescheduling.ForgePlacementSpec{}, acct)
	require.Error(t, err)
	require.False(t, unavailable)
	require.ErrorContains(t, err, "no instance type preference configured")
}

func TestUnit_NicoControlPlaneInstanceTypePreferenceRequiresProviderPayload(t *testing.T) {
	instanceTypeID := uuid.New()

	got, err := nicoControlPlaneInstanceTypePreference(nicoControlPlaneConfigSpec([]uuid.UUID{instanceTypeID}))
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{instanceTypeID}, got)

	_, err = nicoControlPlaneInstanceTypePreference(db.ControlPlaneConfigSpec{
		InstanceTypePreference: []uuid.UUID{instanceTypeID},
	})
	require.ErrorContains(t, err, "nico control-plane provider payload is required")
}

func TestUnit_PersistControlPlaneInstanceTypeKeepsExistingStickyType(t *testing.T) {
	existing := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	requested := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	nodeID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	databaseTx := &testutil.MockTx{}
	database := &testutil.MockDB{GetTx: func() db.Tx { return databaseTx }}
	database.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Once()
	databaseTx.On("GetNodeForUpdate", mock.Anything, nodeID).Return(db.Node{ID: nodeID, InstanceTypeID: existing}, nil).Once()

	scheduler := NewControlPlaneNodeScheduler(config.Config{}, slog.Default(), dbwrapper.New(database, slog.Default()), nil, nil)
	got, err := scheduler.persistControlPlaneInstanceType(context.Background(), nodeID, requested)
	require.NoError(t, err)
	require.Equal(t, existing, got)
	databaseTx.AssertNotCalled(t, "UpdateNode", mock.Anything, mock.Anything)
	database.AssertExpectations(t)
	databaseTx.AssertExpectations(t)
}

func TestUnit_ControlPlaneCapacityAccountReserveDebitsBothLedgers(t *testing.T) {
	instanceTypeID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	subnetID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	placement := nodescheduling.ForgePlacementSpec{SubnetIDs: []uuid.UUID{subnetID}}
	placementKey := forgeNetworkPlacementKey(placement)

	scheduler := NewControlPlaneNodeScheduler(config.Config{}, slog.Default(), dbwrapper.New(&testutil.MockDB{}, slog.Default()), nil, nil)
	acct := newControlPlaneCapacityAccount(scheduler, slog.Default())
	acct.forgeRemaining[instanceTypeID] = 3
	acct.networkRemaining[placementKey] = 5

	acct.reserve(context.Background(), nicoCluster(uuid.New(), instanceTypeID, subnetID), instanceTypeID, placement)

	require.Equal(t, 2, acct.forgeRemaining[instanceTypeID])
	require.Equal(t, 4, acct.networkRemaining[placementKey])
}

func TestUnit_ForgeNetworkPlacementKeyOrderIndependent(t *testing.T) {
	subnetA, subnetB := uuid.New(), uuid.New()
	prefixA, prefixB := uuid.New(), uuid.New()

	forward := nodescheduling.ForgePlacementSpec{SubnetIDs: []uuid.UUID{subnetA, subnetB}, VpcPrefixIDs: []uuid.UUID{prefixA, prefixB}}
	reversed := nodescheduling.ForgePlacementSpec{SubnetIDs: []uuid.UUID{subnetB, subnetA}, VpcPrefixIDs: []uuid.UUID{prefixB, prefixA}}
	assert.Equal(t, forgeNetworkPlacementKey(forward), forgeNetworkPlacementKey(reversed))
	assert.NotEqual(t, forgeNetworkPlacementKey(forward), forgeNetworkPlacementKey(nodescheduling.ForgePlacementSpec{SubnetIDs: []uuid.UUID{subnetA}}))
}

func expectProvisionWorkflow(t *testing.T, temporalClient *mockWorkflowStarter, nodeID uuid.UUID) {
	t.Helper()
	temporalClient.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(opts client.StartWorkflowOptions) bool {
		return opts.TaskQueue == "test-task-queue" &&
			opts.ID == constants.ProvisionNodeWorkflowPrefix+nodeID.String()
	}), mock.Anything, mock.MatchedBy(func(input workflows.ProvisionNodeInput) bool {
		return input.NodeID == nodeID && input.RebootNodeWaitForUptimeTimeout == 5*time.Minute
	})).Return(mockWorkflowRun{id: nodeID.String()}, nil).Once()
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

func nicoCluster(id, instanceTypeID, subnetID uuid.UUID) db.Cluster {
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
		ControlPlaneConfig: db.ControlPlaneConfig{
			ControlPlaneConfigSpec: nicoControlPlaneConfigSpecWithPlacement([]uuid.UUID{instanceTypeID}, []uuid.UUID{subnetID}),
		},
	}
}

func nicoControlPlaneConfigSpec(instanceTypePreference []uuid.UUID) db.ControlPlaneConfigSpec {
	return nicoControlPlaneConfigSpecWithPlacement(instanceTypePreference, nil)
}

func nicoControlPlaneConfigSpecWithPlacement(instanceTypePreference, subnetIDs []uuid.UUID) db.ControlPlaneConfigSpec {
	return db.ControlPlaneConfigSpec{
		Nico: &db.NicoControlPlaneProvider{
			InstanceTypePreference: append([]uuid.UUID(nil), instanceTypePreference...),
			SubnetIDs:              append([]uuid.UUID(nil), subnetIDs...),
		},
		InstanceTypePreference: append([]uuid.UUID(nil), instanceTypePreference...),
		SubnetIDs:              append([]uuid.UUID(nil), subnetIDs...),
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
