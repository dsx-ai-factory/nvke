// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package testutil

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"

	"github.com/NVIDIA/nke/controller/internal/db"
)

// MockDB is a testify mock for db.DB. Set MockTx (or GetTx) before use so RunInTx invokes the callback with it.
type MockDB struct {
	mock.Mock
	db.DB
	// MockTx is the transaction passed to the RunInTx callback. Prefer setting GetTx in tests to avoid nil receiver issues.
	MockTx *MockTx
	// GetTx, if set, is used by RunInTx to obtain the tx (e.g. func() db.Tx { return mockTx }). Use when MockTx is not reliable.
	GetTx func() db.Tx
}

func (m *MockDB) Ping(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

// RunInTx runs the callback with the tx from GetTx() if set, otherwise MockTx.
// If both are nil or GetTx returns nil, the callback is not invoked.
func (m *MockDB) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	args := m.Called(ctx, fn)
	var tx db.Tx
	if m.GetTx != nil {
		tx = m.GetTx()
	}
	if tx == nil {
		tx = m.MockTx
	}
	if tx != nil {
		if err := fn(ctx, tx); err != nil {
			return err
		}
	}
	return args.Error(0)
}

func (m *MockDB) DeleteNodePool(ctx context.Context, nodePoolID uuid.UUID) error {
	args := m.Called(ctx, nodePoolID)
	return args.Error(0)
}

func (m *MockDB) ListClusters(ctx context.Context, filters db.ClusterFilters) ([]db.Cluster, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.Cluster)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetCluster(ctx context.Context, id uuid.UUID) (db.Cluster, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) SyncClusterControlPlaneConfigFromCPNodePools(ctx context.Context, clusterID uuid.UUID) error {
	args := m.Called(ctx, clusterID)
	return args.Error(0)
}

func (m *MockDB) UpdateSiteNextCSRApprovalDue(ctx context.Context, siteID uuid.UUID, nextDue time.Time) error {
	args := m.Called(ctx, siteID, nextDue)
	return args.Error(0)
}

func (m *MockDB) UpdateSiteNextCertRenewalDue(ctx context.Context, siteID uuid.UUID, nextDue time.Time) error {
	args := m.Called(ctx, siteID, nextDue)
	return args.Error(0)
}

func (m *MockDB) CreateControlPlaneConfiguration(ctx context.Context, row *db.ControlPlaneConfiguration) (db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, row)
	v, ok := args.Get(0).(db.ControlPlaneConfiguration)
	if !ok {
		return db.ControlPlaneConfiguration{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetControlPlaneConfiguration(ctx context.Context, id uuid.UUID) (db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.ControlPlaneConfiguration)
	if !ok {
		return db.ControlPlaneConfiguration{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListControlPlaneConfigurationsByIDs(ctx context.Context, ids []uuid.UUID) ([]db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, ids)
	v, ok := args.Get(0).([]db.ControlPlaneConfiguration)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListNodePools(ctx context.Context, filters db.NodePoolFilters) ([]db.NodePool, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.NodePool)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetNodePool(ctx context.Context, id uuid.UUID) (db.NodePool, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetNodeSet(ctx context.Context, id uuid.UUID) (db.NodeSet, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.NodeSet)
	if !ok {
		return db.NodeSet{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListNodes(ctx context.Context, filters db.NodeFilters) ([]db.Node, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.Node)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetNode(ctx context.Context, id uuid.UUID) (db.Node, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (db.Node, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListNodeSets(ctx context.Context, filters db.NodeSetFilters) ([]db.NodeSet, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.NodeSet)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

// MockTx is a testify mock for db.Tx.
type MockTx struct {
	mock.Mock
	db.Tx
}

func (m *MockTx) GetSite(ctx context.Context, id uuid.UUID, lockForUpdate bool) (db.Site, error) {
	args := m.Called(ctx, id, lockForUpdate)
	v, ok := args.Get(0).(db.Site)
	if !ok {
		return db.Site{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) CreateVIPAllocation(ctx context.Context, vipAllocation *db.VIPAllocation) (db.VIPAllocation, error) {
	args := m.Called(ctx, vipAllocation)
	v, ok := args.Get(0).(db.VIPAllocation)
	if !ok {
		return db.VIPAllocation{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) ListVIPAllocations(ctx context.Context, siteID *uuid.UUID) ([]db.VIPAllocation, error) {
	args := m.Called(ctx, siteID)
	v, ok := args.Get(0).([]db.VIPAllocation)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) CreateCluster(ctx context.Context, cluster *db.Cluster) (db.Cluster, error) {
	args := m.Called(ctx, cluster)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetCluster(ctx context.Context, id uuid.UUID) (db.Cluster, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetClusterForUpdate(ctx context.Context, id uuid.UUID) (db.Cluster, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) UpdateCluster(ctx context.Context, cluster *db.Cluster) (db.Cluster, error) {
	args := m.Called(ctx, cluster)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) UpdateSiteNextCSRApprovalDue(ctx context.Context, siteID uuid.UUID, nextDue time.Time) error {
	args := m.Called(ctx, siteID, nextDue)
	return args.Error(0)
}

func (m *MockTx) UpdateSiteNextCertRenewalDue(ctx context.Context, siteID uuid.UUID, nextDue time.Time) error {
	args := m.Called(ctx, siteID, nextDue)
	return args.Error(0)
}

func (m *MockTx) SyncClusterControlPlaneConfigFromCPNodePools(ctx context.Context, clusterID uuid.UUID) error {
	args := m.Called(ctx, clusterID)
	return args.Error(0)
}

func (m *MockTx) CreateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	args := m.Called(ctx, nodePool)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetNodePool(ctx context.Context, id uuid.UUID) (db.NodePool, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) CreateControlPlaneConfiguration(ctx context.Context, row *db.ControlPlaneConfiguration) (db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, row)
	v, ok := args.Get(0).(db.ControlPlaneConfiguration)
	if !ok {
		return db.ControlPlaneConfiguration{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetControlPlaneConfiguration(ctx context.Context, id uuid.UUID) (db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.ControlPlaneConfiguration)
	if !ok {
		return db.ControlPlaneConfiguration{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) ListControlPlaneConfigurationsByIDs(ctx context.Context, ids []uuid.UUID) ([]db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, ids)
	v, ok := args.Get(0).([]db.ControlPlaneConfiguration)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) ListNodePools(ctx context.Context, filters db.NodePoolFilters) ([]db.NodePool, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.NodePool)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetNodePoolForUpdate(ctx context.Context, id uuid.UUID) (db.NodePool, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) UpdateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	args := m.Called(ctx, nodePool)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetNodeSet(ctx context.Context, id uuid.UUID) (db.NodeSet, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.NodeSet)
	if !ok {
		return db.NodeSet{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) CreateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	args := m.Called(ctx, nodeSet)
	v, ok := args.Get(0).(db.NodeSet)
	if !ok {
		return db.NodeSet{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) ListNodeSets(ctx context.Context, filters db.NodeSetFilters) ([]db.NodeSet, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.NodeSet)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) UpdateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	args := m.Called(ctx, nodeSet)
	v, ok := args.Get(0).(db.NodeSet)
	if !ok {
		return db.NodeSet{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) DeleteNodeSet(ctx context.Context, id uuid.UUID) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockTx) ListClusterNodeNames(ctx context.Context, clusterID uuid.UUID) ([]string, error) {
	args := m.Called(ctx, clusterID)
	v, ok := args.Get(0).([]string)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) ListNodes(ctx context.Context, filters db.NodeFilters) ([]db.Node, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.Node)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) CreateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	args := m.Called(ctx, node)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetNode(ctx context.Context, id uuid.UUID) (db.Node, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (db.Node, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) UpdateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	args := m.Called(ctx, node)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListNodeConditions(ctx context.Context) ([]db.NodeConditionStatus, error) {
	args := m.Called(ctx)
	v, ok := args.Get(0).([]db.NodeConditionStatus)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListNodePoolMetrics(ctx context.Context) ([]db.NodePoolMetrics, error) {
	args := m.Called(ctx)
	v, ok := args.Get(0).([]db.NodePoolMetrics)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListNodeInfo(ctx context.Context) ([]db.NodeInfo, error) {
	args := m.Called(ctx)
	v, ok := args.Get(0).([]db.NodeInfo)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListNodeLifecycleElapsed(ctx context.Context) ([]db.NodeLifecycleElapsed, error) {
	args := m.Called(ctx)
	v, ok := args.Get(0).([]db.NodeLifecycleElapsed)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) CreateWorkflowOutboxEntry(
	ctx context.Context,
	workflowType string,
	workflowID string,
	workflowInput interface{},
) (db.WorkflowOutbox, error) {
	args := m.Called(ctx, workflowType, workflowID, workflowInput)
	v, ok := args.Get(0).(db.WorkflowOutbox)
	if !ok {
		return db.WorkflowOutbox{}, args.Error(1)
	}
	return v, args.Error(1)
}
