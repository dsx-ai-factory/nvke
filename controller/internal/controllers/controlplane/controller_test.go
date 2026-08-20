// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/controllers/controlplane"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

// Controller tests verify high-level flows, focusing on happy paths.
// The primary goal is to test the re-entrant and idempotency properties of the controller.
//
// Testing very specific cases is difficult and may require extensive setup. Moreover,
// due to the asynchronous nature of controllers, hitting specific code paths precisely
// (as in traditional unit tests) can be challenging and may introduce flakiness.
//
// These tests are most effective when verifying the end/stable state of the world,
// rather than the exact path taken to reach that state. For tests that need to verify
// specific pieces of logic, it is recommended to write unit tests for those individual
// functions directly.

const (
	controllerEventuallyTimeout = 5 * time.Second
	testKubernetesVersion       = "1.31.0"
)

func TestUnit_ControllerReconcileSkipsMissingCluster(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	mockTx := &testutil.MockTx{}
	mockDB := &testutil.MockDB{MockTx: mockTx}
	mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound)
	controller := controlplane.NewController(
		config.Config{},
		slog.Default(),
		dbwrapper.New(mockDB, slog.Default()),
		nil,
	)

	result, err := controller.Reconcile(context.Background(), dbreconcile.ClusterKey(clusterID))

	require.NoError(t, err)
	assert.Empty(t, result)
	mockDB.AssertExpectations(t)
	mockTx.AssertExpectations(t)
}

func TestController_Reconcile_WithOneCluster_ReplaceOneMachine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tc := newControllerTest(ctx, t)

	// Mark one node for recycling
	nodeToRecycle := tc.controlPlane.nodes[0]
	nodeToRecycle.Conditions.MarkedForRecycling = db.Condition{Status: db.ConditionStatusTrue}
	_, err := tc.database.UpdateNode(ctx, &nodeToRecycle)
	require.NoError(t, err)

	// Start the controller loop
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tc.startControllers(ctx)

	// Verify we eventually have 3 healthy nodes, none marked for recycling,
	// and the original node has been deleted
	assert.Eventually(t, func() bool {
		// Check that the original node has been deleted
		_, err := tc.database.GetNode(ctx, nodeToRecycle.ID)
		if !errors.Is(err, db.ErrNotFound) {
			return false
		}

		// Check we have 3 healthy nodes
		notDeleting := false
		nodes, err := tc.database.ListNodes(ctx, db.NodeFilters{
			NodePoolID: &tc.controlPlane.nodePool.ID,
			Deleting:   &notDeleting,
		})
		if err != nil || len(nodes) != 3 {
			return false
		}
		for _, node := range nodes {
			if node.Conditions.KubernetesReady.Status != db.ConditionStatusTrue {
				return false
			}
			if node.Conditions.MarkedForRecycling.Status == db.ConditionStatusTrue {
				return false
			}
		}
		return true
	}, controllerEventuallyTimeout, 10*time.Millisecond)
}

func TestController_Reconcile_WithOneCluster_ReplaceAllMachines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tc := newControllerTest(ctx, t)

	// Mark all nodes for recycling
	originalNodeIDs := make([]uuid.UUID, len(tc.controlPlane.nodes))
	for i, node := range tc.controlPlane.nodes {
		originalNodeIDs[i] = node.ID
		node.Conditions.MarkedForRecycling = db.Condition{Status: db.ConditionStatusTrue}
		_, err := tc.database.UpdateNode(ctx, &node)
		require.NoError(t, err)
	}

	// Start the controller loop
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tc.startControllers(ctx)

	// Verify we eventually have 3 healthy nodes, none marked for recycling,
	// and all original nodes have been deleted
	assert.Eventually(t, func() bool {
		// Check that all original nodes have been deleted
		for _, nodeID := range originalNodeIDs {
			_, err := tc.database.GetNode(ctx, nodeID)
			if !errors.Is(err, db.ErrNotFound) {
				return false
			}
		}

		// Check we have 3 healthy nodes
		notDeleting := false
		nodes, err := tc.database.ListNodes(ctx, db.NodeFilters{
			NodePoolID: &tc.controlPlane.nodePool.ID,
			Deleting:   &notDeleting,
		})
		if err != nil || len(nodes) != 3 {
			return false
		}
		for _, node := range nodes {
			if node.Conditions.KubernetesReady.Status != db.ConditionStatusTrue {
				return false
			}
			if node.Conditions.MarkedForRecycling.Status == db.ConditionStatusTrue {
				return false
			}
		}
		return true
	}, controllerEventuallyTimeout, 10*time.Millisecond)
}

// controllerTest holds all resources needed for site controller tests.
type controllerTest struct {
	database     db.DB
	site         db.Site
	cluster      db.Cluster
	controlPlane controlPlaneFixture
	cfg          config.Config
	logger       *slog.Logger
}

// sqliteCompatDB is a test-only wrapper that avoids FOR UPDATE in SQLite-backed tests.
// It preserves behavior for these integration-style controller tests while production
// code continues to use GetNodeForUpdate on real Postgres.
type sqliteCompatDB struct {
	db.DB
}

func (d sqliteCompatDB) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (db.Node, error) {
	return d.DB.GetNode(ctx, id)
}

func (d sqliteCompatDB) GetClusterForUpdate(ctx context.Context, id uuid.UUID) (db.Cluster, error) {
	return d.DB.GetCluster(ctx, id)
}

func (d sqliteCompatDB) RunInTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error {
	return d.DB.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return fn(ctx, sqliteCompatTx{Tx: tx})
	})
}

type sqliteCompatTx struct {
	db.Tx
}

func (tx sqliteCompatTx) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (db.Node, error) {
	return tx.Tx.GetNode(ctx, id)
}

func (tx sqliteCompatTx) GetClusterForUpdate(ctx context.Context, id uuid.UUID) (db.Cluster, error) {
	return tx.Tx.GetCluster(ctx, id)
}

// newControllerTest creates a new test environment with a site, cluster, and control plane.
func newControllerTest(ctx context.Context, t *testing.T) *controllerTest {
	t.Helper()

	// Setup in-memory SQLite database with all required tables
	bunDB := testutil.SetupDB(ctx, t,
		(*db.Site)(nil),
		(*db.Cluster)(nil),
		(*db.ControlPlaneConfiguration)(nil),
		(*db.NodePool)(nil),
		(*db.NodeSet)(nil),
		(*db.Node)(nil),
		(*db.WorkflowOutbox)(nil),
	)

	database := sqliteCompatDB{DB: db.NewDB(bunDB)}

	// Create a site
	siteID := uuid.New()
	site := db.Site{
		ID:        siteID,
		VIPRanges: []string{"10.0.0.1-10.0.0.255"},
	}
	_, err := database.RegisterSite(ctx, &site)
	require.NoError(t, err)

	// Create a cluster in the site
	clusterID := uuid.New()
	cluster := db.Cluster{
		ID:                clusterID,
		Name:              "test-cluster",
		OrgID:             "test-org",
		SiteID:            siteID,
		VpcID:             uuid.New(),
		KubernetesVersion: testKubernetesVersion,
	}
	_, err = database.CreateCluster(ctx, &cluster)
	require.NoError(t, err)

	// Create a control plane node pool with 3 nodes
	cp := createControlPlaneNodePool(ctx, t, database, clusterID, 3)

	return &controllerTest{
		database:     database,
		site:         site,
		cluster:      cluster,
		controlPlane: cp,
		cfg:          config.Config{},
		logger:       slog.Default(),
	}
}

// startControllers starts all the fake controllers and the site controller.
func (tc *controllerTest) startControllers(ctx context.Context) {
	go newFakeNodeController(tc.database, tc.logger).Start(ctx)
	go newFakeNodePoolController(tc.database, tc.logger).Start(ctx)
	go newFakeProvisioner(tc.database, tc.logger).Start(ctx)
	go newCPController(tc.cfg, tc.logger, tc.database, tc.cluster.ID).Start(ctx)
}

// controlPlaneFixture holds the resources created for a control plane node pool.
type controlPlaneFixture struct {
	nodePool db.NodePool
	nodeSet  db.NodeSet
	nodes    []db.Node
}

// createControlPlaneNodePool creates a control plane node pool with the specified
// number of healthy nodes, including the node set.
func createControlPlaneNodePool(ctx context.Context, t *testing.T, database db.DB, clusterID uuid.UUID, nodeCount int) controlPlaneFixture {
	t.Helper()

	// Create node set ID upfront so we can reference it in the node pool
	nodeSetID := uuid.New()

	nodePool := createNodePool(ctx, t, database, clusterID, "control-plane", true,
		func(np *db.NodePool) {
			np.DesiredCount = nodeCount
			np.CurrentNodeSetID = &nodeSetID
		},
	)

	nodeSet := createNodeSet(ctx, t, database, nodePool.ID,
		func(ns *db.NodeSet) {
			ns.ID = nodeSetID
		},
	)

	nodes := make([]db.Node, 0, nodeCount)
	for i := range nodeCount {
		node := createNode(ctx, t, database, nodePool.ID, nodeSet.ID, fmt.Sprintf("cp-node-%d", i))
		nodes = append(nodes, node)
	}

	return controlPlaneFixture{
		nodePool: nodePool,
		nodeSet:  nodeSet,
		nodes:    nodes,
	}
}

// Option types for functional options pattern
type (
	nodePoolOpt func(*db.NodePool)
	nodeSetOpt  func(*db.NodeSet)
	nodeOpt     func(*db.Node)
)

// newNodePool creates a new NodePool struct with sensible defaults for testing.
func newNodePool(clusterID uuid.UUID, name string, controlPlane bool, opts ...nodePoolOpt) db.NodePool {
	np := db.NodePool{
		ID:                uuid.New(),
		ClusterID:         clusterID,
		Name:              name,
		NodeNamePrefix:    "node",
		SubnetIDs:         []uuid.UUID{uuid.New()},
		VpcPrefixIDs:      []uuid.UUID{uuid.New()},
		InstanceTypeID:    uuid.New(),
		SSHKeyGroupIDs:    []uuid.UUID{uuid.New()},
		OSProfile:         "test-os-profile",
		ConfigProfile:     "test-config-profile",
		ControlPlane:      controlPlane,
		NodeLabels:        []string{},
		KubernetesVersion: testKubernetesVersion,
	}
	for _, opt := range opts {
		opt(&np)
	}
	return np
}

// createNodePool stores a NodePool in the database.
func createNodePool(ctx context.Context, t *testing.T, database db.DB, clusterID uuid.UUID, name string, controlPlane bool, opts ...nodePoolOpt) db.NodePool {
	t.Helper()
	nodePool := newNodePool(clusterID, name, controlPlane, opts...)
	created, err := database.CreateNodePool(ctx, &nodePool)
	require.NoError(t, err)
	return created
}

// newNodeSet creates a new NodeSet struct with sensible defaults for testing.
func newNodeSet(nodePoolID uuid.UUID, opts ...nodeSetOpt) db.NodeSet {
	ns := db.NodeSet{
		ID:                   uuid.New(),
		NodePoolID:           nodePoolID,
		OSProfile:            "test-os-profile",
		ConfigProfile:        "test-config-profile",
		SystemReservedMemory: "16Gi",
		KubernetesVersion:    testKubernetesVersion,
	}
	for _, opt := range opts {
		opt(&ns)
	}
	return ns
}

// createNodeSet stores a NodeSet in the database.
func createNodeSet(ctx context.Context, t *testing.T, database db.DB, nodePoolID uuid.UUID, opts ...nodeSetOpt) db.NodeSet {
	t.Helper()
	nodeSet := newNodeSet(nodePoolID, opts...)
	created, err := database.CreateNodeSet(ctx, &nodeSet)
	require.NoError(t, err)
	return created
}

// newNode creates a new Node struct with sensible defaults for testing.
// By default, nodes are healthy (KubernetesReady=True, Provisioned=True, NodeAgentReady=True, Pending=False).
func newNode(nodePoolID, nodeSetID uuid.UUID, name string, opts ...nodeOpt) db.Node {
	n := db.Node{
		ID:              uuid.New(),
		NodePoolID:      nodePoolID,
		NodeSetID:       nodeSetID,
		Name:            name,
		ForgeInstanceID: "forge-instance-" + name,
		ForgeMachineID:  "forge-machine-" + name,
		IP:              "10.0.0.1",
		Conditions: db.NodeConditions{
			KubernetesReady: db.Condition{Status: db.ConditionStatusTrue},
			Provisioned:     db.Condition{Status: db.ConditionStatusTrue},
			NodeAgentReady:  db.Condition{Status: db.ConditionStatusTrue},
			Pending:         db.Condition{Status: db.ConditionStatusFalse},
		},
	}
	for _, opt := range opts {
		opt(&n)
	}
	return n
}

// createNode stores a Node in the database.
func createNode(ctx context.Context, t *testing.T, database db.DB, nodePoolID, nodeSetID uuid.UUID, name string, opts ...nodeOpt) db.Node {
	t.Helper()
	node := newNode(nodePoolID, nodeSetID, name, opts...)
	created, err := database.CreateNode(ctx, &node)
	require.NoError(t, err)
	return created
}

// reconcileFunc is a function that performs a single reconciliation.
type reconcileFunc func(ctx context.Context) error

// controller runs a reconcile function in a loop.
type controller struct {
	reconcile reconcileFunc
	interval  time.Duration
	logger    *slog.Logger
}

// newController transforms a reconciler in a control loop.
func newController(reconcile reconcileFunc, logger *slog.Logger) *controller {
	return &controller{
		reconcile: reconcile,
		interval:  100 * time.Millisecond,
		logger:    logger,
	}
}

// Start runs the reconciliation loop until the context is canceled.
func (c *controller) Start(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.reconcile(ctx); err != nil {
				c.logger.Error("reconcile failed", "error", err)
			}
		}
	}
}

// newCPController creates a controller wrapper for a controlplane.Controller.
func newCPController(cfg config.Config, logger *slog.Logger, database db.DB, clusterID uuid.UUID) *controller {
	cpController := controlplane.NewController(cfg, logger, dbwrapper.New(database, logger), nil)
	return newController(func(ctx context.Context) error {
		_, err := cpController.Reconcile(ctx, dbreconcile.ClusterKey(clusterID))
		return err
	}, logger)
}

// newFakeNodeController creates a controller that simulates node deletion.
// It deletes nodes that are marked for deletion from the database.
func newFakeNodeController(database db.DB, logger *slog.Logger) *controller {
	return newController(func(ctx context.Context) error {
		// Find nodes marked for deletion
		deleting := true
		nodes, err := database.ListNodes(ctx, db.NodeFilters{Deleting: &deleting})
		if err != nil {
			return err
		}

		// Delete them from the database
		for _, node := range nodes {
			if err := database.DeleteNode(ctx, node.ID); err != nil {
				return err
			}
			logger.Info("fake node controller deleted node", "nodeID", node.ID, "nodeName", node.Name)
		}

		return nil
	}, logger)
}

// newFakeNodePoolController creates a controller that simulates node provisioning.
// It creates new nodes (in pending state) when the actual count is less than the desired count.
func newFakeNodePoolController(database db.DB, logger *slog.Logger) *controller {
	nodeCounter := 0
	return newController(func(ctx context.Context) error {
		nodePools, err := database.ListNodePools(ctx, db.NodePoolFilters{})
		if err != nil {
			return err
		}

		for _, nodePool := range nodePools {
			if nodePool.CurrentNodeSetID == nil {
				continue
			}

			// Count non-deleting nodes in this node pool
			notDeleting := false
			nodes, err := database.ListNodes(ctx, db.NodeFilters{
				NodePoolID: &nodePool.ID,
				Deleting:   &notDeleting,
			})
			if err != nil {
				return err
			}

			actualCount := len(nodes)
			if actualCount >= nodePool.DesiredCount {
				continue
			}

			// Create missing nodes in pending state
			for i := actualCount; i < nodePool.DesiredCount; i++ {
				node := newNode(nodePool.ID, *nodePool.CurrentNodeSetID, fmt.Sprintf("node-%d", nodeCounter),
					func(n *db.Node) {
						n.Conditions.Pending = db.Condition{Status: db.ConditionStatusTrue}
						n.Conditions.KubernetesReady = db.Condition{Status: db.ConditionStatusFalse}
					},
				)
				nodeCounter++
				if _, err := database.CreateNode(ctx, &node); err != nil {
					return err
				}
				logger.Info("fake node pool controller created node",
					"nodePoolID", nodePool.ID,
					"nodeName", node.Name,
					"actualCount", actualCount,
					"desiredCount", nodePool.DesiredCount,
				)
			}
		}

		return nil
	}, logger)
}

// newFakeProvisioner creates a controller that simulates the node provisioning lifecycle.
// It transitions nodes through: Pending=True -> Pending=False -> KubernetesReady=True -> Provisioned=True
// Each transition happens after ~10ms + jitter.
func newFakeProvisioner(database db.DB, logger *slog.Logger) *controller {
	// Track when nodes became not-pending so we know when to mark them ready
	notPendingSince := make(map[uuid.UUID]time.Time)

	return newController(func(ctx context.Context) error {
		// Find all pending nodes
		pending := true
		notDeleting := false
		pendingNodes, err := database.ListNodes(ctx, db.NodeFilters{
			Pending:  &pending,
			Deleting: &notDeleting,
		})
		if err != nil {
			return err
		}

		// Mark pending nodes as not-pending after 10ms + jitter
		for _, node := range pendingNodes {
			elapsed := time.Since(node.CreatedAt)
			jitter := time.Duration(rand.IntN(5)) * time.Millisecond
			if elapsed >= 10*time.Millisecond+jitter {
				node.Conditions.Pending = db.Condition{Status: db.ConditionStatusFalse}
				if _, err := database.UpdateNode(ctx, &node); err != nil {
					return err
				}
				notPendingSince[node.ID] = time.Now()
				logger.Info("fake provisioner marked node as not pending", "nodeID", node.ID, "nodeName", node.Name)
			}
		}

		// Find all non-pending, non-ready nodes
		notPending := false
		notReady := false
		provisioningNodes, err := database.ListNodes(ctx, db.NodeFilters{
			Pending:         &notPending,
			KubernetesReady: &notReady,
			Deleting:        &notDeleting,
		})
		if err != nil {
			return err
		}

		// Mark non-pending nodes as ready after 10ms + jitter
		for _, node := range provisioningNodes {
			becameNotPending, tracked := notPendingSince[node.ID]
			if !tracked {
				// Node was already not-pending before we started tracking
				notPendingSince[node.ID] = time.Now()
				continue
			}

			elapsed := time.Since(becameNotPending)
			jitter := time.Duration(rand.IntN(5)) * time.Millisecond
			if elapsed >= 10*time.Millisecond+jitter {
				node.Conditions.KubernetesReady = db.Condition{Status: db.ConditionStatusTrue}
				node.Conditions.Provisioned = db.Condition{Status: db.ConditionStatusTrue}
				node.Conditions.NodeAgentReady = db.Condition{Status: db.ConditionStatusTrue}
				if _, err := database.UpdateNode(ctx, &node); err != nil {
					return err
				}
				delete(notPendingSince, node.ID)
				logger.Info("fake provisioner marked node as kubernetes ready", "nodeID", node.ID, "nodeName", node.Name)
			}
		}

		return nil
	}, logger)
}
