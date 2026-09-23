// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleporttoken

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/teleport"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

// --- Test environment (replaces mockDB with in-memory SQLite) ---

// testEnv holds an in-memory database with a pre-created entity chain:
// Cluster (teleport_enabled) -> NodePool -> NodeSet (teleport_enabled)
type testEnv struct {
	database  *dbwrapper.DB
	bunDB     *bun.DB
	k8sClient client.Client
	siteID    uuid.UUID
	cluster   db.Cluster
	nodePool  db.NodePool
	nodeSet   db.NodeSet
}

// sqliteCompatDB is test-only: it avoids SELECT ... FOR UPDATE paths that SQLite
// cannot execute while preserving functional behavior for controller tests.
type sqliteCompatDB struct {
	db.DB
}

func (d sqliteCompatDB) UpdateNodeTeleportRegistered(ctx context.Context, nodeID uuid.UUID, condition db.Condition) error {
	node, err := d.DB.GetNode(ctx, nodeID)
	if err != nil {
		return err
	}
	node.Conditions.TeleportRegistered = condition
	_, err = d.DB.UpdateNode(ctx, &node)
	return err
}

// setupTestEnv creates an isolated in-memory SQLite database and seeds it with
// a complete entity chain suitable for Teleport controller testing.
func setupTestEnv(ctx context.Context, t *testing.T) *testEnv {
	t.Helper()

	bunDB := testutil.SetupDB(ctx, t,
		(*db.Cluster)(nil),
		(*db.NodePool)(nil),
		(*db.NodeSet)(nil),
		(*db.Node)(nil),
	)
	database := sqliteCompatDB{DB: db.NewDB(bunDB)}

	siteID := uuid.New()
	cluster := db.Cluster{
		ID:                uuid.New(),
		Name:              "test-cluster",
		KubernetesVersion: "1.31.0",
		TeleportEnabled:   true,
	}
	cluster.SetNicoProvider(db.NicoClusterProvider{
		OrgID:  "test-org",
		SiteID: siteID,
		VpcID:  uuid.New(),
	})
	_, err := database.CreateCluster(ctx, &cluster)
	require.NoError(t, err)

	nodePool := db.NodePool{
		ID:                   uuid.New(),
		ClusterID:            cluster.ID,
		Name:                 "test-pool",
		NodeNamePrefix:       "test",
		NodeLabels:           []string{},
		SystemReservedMemory: "16Gi",
	}
	nodePool.SetNico(db.NicoNodePoolProvider{
		SubnetIDs:      []uuid.UUID{uuid.New()},
		VpcPrefixIDs:   []uuid.UUID{uuid.New()},
		InstanceTypeID: uuid.New(),
		SSHKeyGroupIDs: []uuid.UUID{},
		OSProfile:      "test-os",
		ConfigProfile:  "test-config",
	})
	_, err = database.CreateNodePool(ctx, &nodePool)
	require.NoError(t, err)

	nodeSet := db.NodeSet{
		ID:                   uuid.New(),
		NodePoolID:           nodePool.ID,
		SystemReservedMemory: "16Gi",
		TeleportEnabled:      true,
	}
	nodeSet.SetNico(db.NicoNodePoolProvider{
		OSProfile:     "test-os",
		ConfigProfile: "test-config",
	})
	_, err = database.CreateNodeSet(ctx, &nodeSet)
	require.NoError(t, err)

	return &testEnv{
		database: dbwrapper.New(database, testLogger()),
		bunDB:    bunDB,
		k8sClient: fake.NewClientBuilder().
			WithScheme(teleportControllerTestScheme(t)).
			Build(),
		siteID:   siteID,
		cluster:  cluster,
		nodePool: nodePool,
		nodeSet:  nodeSet,
	}
}

// createNode creates a Teleport-eligible node in the test database.
func (te *testEnv) createNode(ctx context.Context, t *testing.T, name, forgeID, ekHash string) db.Node {
	t.Helper()
	node := db.Node{
		ID:         uuid.New(),
		ClusterID:  te.cluster.ID,
		NodePoolID: te.nodePool.ID,
		NodeSetID:  te.nodeSet.ID,
		Name:       name,
		IP:         "10.0.0.1",
		Nico: &db.NicoNodeProvider{
			ForgeInstanceID: forgeID,
			ForgeMachineID:  "machine-" + name,
		},
	}
	created, err := te.database.CreateNode(ctx, &node)
	require.NoError(t, err)
	if ekHash != "" {
		te.createNicoMachineForNode(ctx, t, created, ekHash)
	}
	return created
}

func (te *testEnv) newController(mockTeleport *mockTeleportClient) *Controller {
	return NewController(testConfig(), te.database, te.k8sClient, testLogger(), mockTeleport)
}

func (te *testEnv) createNicoMachineForNode(ctx context.Context, t *testing.T, node db.Node, ekHash string) {
	t.Helper()

	machine := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: constants.NamespaceNKESystem,
			Annotations: map[string]string{
				constants.MachineAnnotationKeyHostID: node.ID.String(),
			},
		},
	}
	require.NoError(t, te.k8sClient.Create(ctx, machine))

	controller := true
	nicoMachine := &nicov1.NicoMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nicov1.GroupVersion.String(),
			Kind:       "NicoMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: constants.NamespaceNKESystem,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Machine",
				Name:       node.Name,
				Controller: &controller,
			}},
		},
		Status: nicov1.NicoMachineStatus{
			TpmEkPubHash: ekHash,
		},
	}
	require.NoError(t, te.k8sClient.Create(ctx, nicoMachine))
}

func teleportControllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, nicov1.AddToScheme(scheme))

	return scheme
}

// createRegisteredNode creates a Teleport-eligible node that is already marked as registered in the DB.
func (te *testEnv) createRegisteredNode(ctx context.Context, t *testing.T, name, forgeID, ekHash string) db.Node {
	t.Helper()
	node := te.createNode(ctx, t, name, forgeID, ekHash)
	condition := db.NewCondition(true, "Registered with Teleport")
	err := te.database.UpdateNodeTeleportRegistered(ctx, node.ID, condition)
	require.NoError(t, err)
	updated, err := te.database.GetNode(ctx, node.ID)
	require.NoError(t, err)
	return updated
}

// --- Mock Teleport client (external service, stays mocked) ---

type mockTeleportClient struct {
	mock.Mock
}

func (m *mockTeleportClient) Close() error {
	args := m.Called()
	return args.Error(0)
}

func (m *mockTeleportClient) UpsertToken(ctx context.Context, token teleport.ProvisionToken) error {
	args := m.Called(ctx, token)
	return args.Error(0)
}

func (m *mockTeleportClient) GetToken(ctx context.Context, name string) (*teleport.ProvisionToken, error) {
	args := m.Called(ctx, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*teleport.ProvisionToken), args.Error(1)
}

func (m *mockTeleportClient) DeleteToken(ctx context.Context, name string) error {
	args := m.Called(ctx, name)
	return args.Error(0)
}

func (m *mockTeleportClient) ListTokens(ctx context.Context) ([]teleport.ProvisionToken, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]teleport.ProvisionToken), args.Error(1)
}

func (m *mockTeleportClient) GetNode(ctx context.Context, namespace, name string) (*teleport.Node, error) {
	args := m.Called(ctx, namespace, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*teleport.Node), args.Error(1)
}

func (m *mockTeleportClient) DeleteNode(ctx context.Context, namespace, name string) error {
	args := m.Called(ctx, namespace, name)
	return args.Error(0)
}

func (m *mockTeleportClient) ListNodes(ctx context.Context, namespace string, opts ...teleport.ListNodesOption) ([]teleport.Node, error) {
	args := m.Called(ctx, namespace, opts)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]teleport.Node), args.Error(1)
}

// Ensure mockTeleportClient implements teleport.Client
var _ teleport.Client = (*mockTeleportClient)(nil)

// --- Helper functions ---

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testConfig() config.Config {
	return config.Config{
		NKESiteControllerName: "test-site",
		Teleport: config.TeleportConfig{
			Enabled:     true,
			Environment: "non-prod",
		},
	}
}

// validEKPubHash returns a valid 64-character hex string for testing.
// The suffix must be hex characters (0-9, a-f) only.
func validEKPubHash(hexSuffix string) string {
	// Base is 60 characters, suffix adds 4 to make 64 total
	base := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6"
	if len(hexSuffix) > 4 {
		hexSuffix = hexSuffix[:4]
	}
	// Pad suffix with zeros if needed
	for len(hexSuffix) < 4 {
		hexSuffix = "0" + hexSuffix
	}
	return base + hexSuffix
}

// --- Tests ---

func TestListTeleportEligibleNodes(t *testing.T) {
	ctx := context.Background()

	t.Run("returns eligible nodes", func(t *testing.T) {
		te := setupTestEnv(ctx, t)

		node1 := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))
		node2 := te.createNode(ctx, t, "node-2", "forge-2", validEKPubHash("0002"))

		result, err := te.database.ListTeleportEligibleNodes(ctx, te.siteID)
		require.NoError(t, err)
		assert.Len(t, result, 2)

		resultIDs := make([]uuid.UUID, len(result))
		for i, n := range result {
			resultIDs[i] = n.ID
		}
		assert.ElementsMatch(t, []uuid.UUID{node1.ID, node2.ID}, resultIDs)
	})

	t.Run("returns empty when no eligible nodes", func(t *testing.T) {
		te := setupTestEnv(ctx, t)

		result, err := te.database.ListTeleportEligibleNodes(ctx, te.siteID)
		require.NoError(t, err)
		assert.Empty(t, result)
	})

	t.Run("includes nodes already registered with Teleport", func(t *testing.T) {
		te := setupTestEnv(ctx, t)

		node := db.Node{
			ID:         uuid.New(),
			ClusterID:  te.cluster.ID,
			NodePoolID: te.nodePool.ID,
			NodeSetID:  te.nodeSet.ID,
			Name:       "registered-node",
			IP:         "10.0.0.1",
			Conditions: db.NodeConditions{
				TeleportRegistered: db.Condition{Status: db.ConditionStatusTrue},
			},
			Nico: &db.NicoNodeProvider{
				ForgeInstanceID: "forge-1",
				ForgeMachineID:  "machine-1",
			},
		}
		_, err := te.database.CreateNode(ctx, &node)
		require.NoError(t, err)

		result, err := te.database.ListTeleportEligibleNodes(ctx, te.siteID)
		require.NoError(t, err)
		assert.Len(t, result, 1)
		assert.Equal(t, node.ID, result[0].ID)
	})

	t.Run("includes CAPNICo nodes without forge instance ID when TPM hash is set", func(t *testing.T) {
		te := setupTestEnv(ctx, t)

		node := db.Node{
			ID:         uuid.New(),
			ClusterID:  te.cluster.ID,
			NodePoolID: te.nodePool.ID,
			NodeSetID:  te.nodeSet.ID,
			Name:       "capi-node",
			IP:         "10.0.0.1",
			Nico: &db.NicoNodeProvider{
				ForgeInstanceID: "",
				ForgeMachineID:  "machine-1",
			},
		}
		_, err := te.database.CreateNode(ctx, &node)
		require.NoError(t, err)

		result, err := te.database.ListTeleportEligibleNodes(ctx, te.siteID)
		require.NoError(t, err)
		require.Len(t, result, 1)
		assert.Equal(t, node.ID, result[0].ID)
	})

	t.Run("includes no-nodepool control plane nodes", func(t *testing.T) {
		te := setupTestEnv(ctx, t)

		node := db.Node{
			ID:           uuid.New(),
			ClusterID:    te.cluster.ID,
			ControlPlane: true,
			Name:         "cp-no-pool",
			IP:           "10.0.0.9",
			Nico: &db.NicoNodeProvider{
				ForgeInstanceID: "forge-cp",
				ForgeMachineID:  "machine-cp",
			},
		}
		_, err := te.database.CreateNode(ctx, &node)
		require.NoError(t, err)

		result, err := te.database.ListTeleportEligibleNodes(ctx, te.siteID)
		require.NoError(t, err)
		require.Len(t, result, 1)
		assert.Equal(t, node.ID, result[0].ID)
	})

	t.Run("includes nodes before NicoMachine hash is available", func(t *testing.T) {
		te := setupTestEnv(ctx, t)

		node := db.Node{
			ID:         uuid.New(),
			ClusterID:  te.cluster.ID,
			NodePoolID: te.nodePool.ID,
			NodeSetID:  te.nodeSet.ID,
			Name:       "no-hash-node",
			IP:         "10.0.0.1",
			Nico: &db.NicoNodeProvider{
				ForgeInstanceID: "forge-1",
				ForgeMachineID:  "machine-1",
			},
		}
		_, err := te.database.CreateNode(ctx, &node)
		require.NoError(t, err)

		result, err := te.database.ListTeleportEligibleNodes(ctx, te.siteID)
		require.NoError(t, err)
		require.Len(t, result, 1)
		assert.Equal(t, node.ID, result[0].ID)
	})
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()

	t.Run("creates tokens for unregistered nodes", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))

		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{}, nil)
		mockTeleport.On("UpsertToken", ctx, mock.MatchedBy(func(token teleport.ProvisionToken) bool {
			return token.Name == teleport.TPMTokenName(node.ID)
		})).Return(nil)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)
		assert.Equal(t, TokenRefreshThreshold, result.RequeueAfter)

		mockTeleport.AssertCalled(t, "UpsertToken", ctx, mock.Anything)
	})

	t.Run("marks already registered nodes and updates DB", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node1 := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))
		node2 := te.createNode(ctx, t, "node-2", "forge-2", validEKPubHash("0002"))

		// Teleport reports node-1 as registered
		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{
			{
				Hostname: "node-1",
				Labels: map[string]string{
					teleport.LabelManagedBy: teleport.LabelManagedByValue,
					teleport.LabelNodeID:    node1.ID.String(),
				},
			},
		}, nil)
		mockTeleport.On("UpsertToken", ctx, mock.Anything).Return(nil)

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node1.ID))
		require.NoError(t, err)

		_, err = controller.Reconcile(ctx, dbreconcile.NodeKey(node2.ID))
		require.NoError(t, err)

		// Verify node-1 was marked as registered in the DB
		updatedNode1, err := te.database.GetNode(ctx, node1.ID)
		require.NoError(t, err)
		assert.Equal(t, db.ConditionStatusTrue, updatedNode1.Conditions.TeleportRegistered.Status)

		// Verify node-2 was NOT marked as registered (token still created)
		updatedNode2, err := te.database.GetNode(ctx, node2.ID)
		require.NoError(t, err)
		assert.NotEqual(t, db.ConditionStatusTrue, updatedNode2.Conditions.TeleportRegistered.Status)
	})

	t.Run("ignores nodes with different node-id even if same hostname", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))

		// Teleport returns a node with same hostname but different node-id (cross-site collision)
		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{
			{
				Hostname: "node-1",
				Labels: map[string]string{
					teleport.LabelManagedBy: teleport.LabelManagedByValue,
					teleport.LabelNodeID:    uuid.New().String(), // Different node-id
				},
			},
		}, nil)
		mockTeleport.On("UpsertToken", ctx, mock.MatchedBy(func(token teleport.ProvisionToken) bool {
			return token.Name == teleport.TPMTokenName(node.ID)
		})).Return(nil)

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)

		// Token should still be created for our node
		mockTeleport.AssertCalled(t, "UpsertToken", ctx, mock.Anything)
	})

	// Note: "ignores non-CWE-managed nodes" test was removed because ListNodes now applies
	// server-side label filtering (managed-by + site), so non-CWE-managed nodes are never returned.

	t.Run("handles bootstrap scenario - registered node without site label", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))

		// Teleport reports node as registered (CWE-managed, matching node-id, no site label)
		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{
			{
				Hostname: "node-1",
				Labels: map[string]string{
					teleport.LabelManagedBy: teleport.LabelManagedByValue,
					teleport.LabelNodeID:    node.ID.String(),
					// No site label - bootstrap scenario
				},
			},
		}, nil)

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)

		// Verify node was marked as registered in the DB
		updatedNode, err := te.database.GetNode(ctx, node.ID)
		require.NoError(t, err)
		assert.Equal(t, db.ConditionStatusTrue, updatedNode.Conditions.TeleportRegistered.Status)

		// UpsertToken should NOT have been called
		mockTeleport.AssertNotCalled(t, "UpsertToken")
	})

	t.Run("fails when ListNodes returns error", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))

		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return(nil, errors.New("teleport api error"))

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to list Teleport nodes")
	})

	t.Run("no tokens created when no eligible nodes", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "no-tpm-node", "forge-1", "")

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)

		mockTeleport.AssertNotCalled(t, "ListNodes")
		mockTeleport.AssertNotCalled(t, "UpsertToken")
	})

	t.Run("waits when NicoMachine hash is empty", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "empty-hash-node", "forge-1", "")
		te.createNicoMachineForNode(ctx, t, node, "")
		controller.tokenCreatedAt[node.ID] = time.Now()

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)
		assert.Zero(t, result.RequeueAfter)
		assert.NotContains(t, controller.tokenCreatedAt, node.ID)
		mockTeleport.AssertNotCalled(t, "ListNodes")
		mockTeleport.AssertNotCalled(t, "UpsertToken")
	})

	t.Run("partial failure - returns aggregated error", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node1 := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))
		node2 := te.createNode(ctx, t, "node-2", "forge-2", validEKPubHash("0002"))

		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{}, nil)
		mockTeleport.On("UpsertToken", ctx, mock.Anything).Return(errors.New("upsert failed"))

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node1.ID))
		require.Error(t, err)
		_, err = controller.Reconcile(ctx, dbreconcile.NodeKey(node2.ID))
		require.Error(t, err)
	})

	t.Run("skips ineligible nodes and clears token throttle", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "deleting-node", "forge-1", validEKPubHash("0001"))
		node.Conditions.Deleting = db.NewCondition(true, "deleting")
		_, err := te.database.UpdateNode(ctx, &node)
		require.NoError(t, err)
		controller.tokenCreatedAt[node.ID] = time.Now()

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)
		assert.Zero(t, result.RequeueAfter)
		assert.NotContains(t, controller.tokenCreatedAt, node.ID)
		mockTeleport.AssertNotCalled(t, "ListNodes")
		mockTeleport.AssertNotCalled(t, "UpsertToken")
	})

	t.Run("clears token throttle for missing node", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)
		nodeID := uuid.New()
		controller.tokenCreatedAt[nodeID] = time.Now()

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(nodeID))
		require.NoError(t, err)
		assert.Zero(t, result.RequeueAfter)
		assert.NotContains(t, controller.tokenCreatedAt, nodeID)
		mockTeleport.AssertNotCalled(t, "ListNodes")
		mockTeleport.AssertNotCalled(t, "UpsertToken")
	})
}

func TestCreateTokenForNode(t *testing.T) {
	ctx := context.Background()

	t.Run("creates token with correct structure", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "test-node", "forge-123", validEKPubHash("abcd"))

		mockTeleport.On("UpsertToken", ctx, mock.MatchedBy(func(token teleport.ProvisionToken) bool {
			// Verify token structure
			assert.Equal(t, teleport.TPMTokenName(node.ID), token.Name)
			assert.Equal(t, []string{"Node"}, token.Roles)
			assert.Equal(t, teleport.JoinMethodTPM, token.JoinMethod)

			// Verify identity labels
			assert.Equal(t, te.siteID.String(), token.Labels[teleport.LabelSite])
			assert.Equal(t, testConfig().NKESiteControllerName, token.Labels[teleport.LabelNKESiteControllerName])
			assert.Equal(t, te.cluster.ID.String(), token.Labels[teleport.LabelCluster])
			assert.Equal(t, te.cluster.Name, token.Labels[teleport.LabelClusterName])
			assert.Equal(t, node.ID.String(), token.Labels[teleport.LabelNodeID])
			assert.Equal(t, te.nodePool.ID.String(), token.Labels[teleport.LabelNodePool])
			assert.Equal(t, "test-node", token.Labels[teleport.LabelNodeName])

			// Verify normalized labels (consistent with userData labels)
			assert.Equal(t, teleport.LabelResourceGroupNKEValue, token.Labels[teleport.LabelResourceGroup])
			assert.Equal(t, "non-prod", token.Labels[teleport.LabelEnvironment])

			// Verify TPM config uses the hash resolved from NicoMachine.
			require.NotNil(t, token.Spec)
			tpmConfig, ok := token.Spec.(*teleport.TPMConfig)
			require.True(t, ok, "Spec should be *teleport.TPMConfig")
			require.Len(t, tpmConfig.Allow, 1)
			assert.Equal(t, validEKPubHash("abcd"), tpmConfig.Allow[0].EKPublicHash)

			return true
		})).Return(nil)

		err := controller.createTokenForNode(ctx, node, validEKPubHash("abcd"))
		require.NoError(t, err)

		mockTeleport.AssertExpectations(t)
	})

	t.Run("creates token with nil node-pool label for no-nodepool control plane", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := db.Node{
			ID:           uuid.New(),
			ClusterID:    te.cluster.ID,
			ControlPlane: true,
			Name:         "cp-no-pool",
			IP:           "10.0.0.9",
			Nico: &db.NicoNodeProvider{
				ForgeInstanceID: "forge-cp",
				ForgeMachineID:  "machine-cp",
			},
		}
		_, err := te.database.CreateNode(ctx, &node)
		require.NoError(t, err)

		mockTeleport.On("UpsertToken", ctx, mock.MatchedBy(func(token teleport.ProvisionToken) bool {
			assert.Equal(t, uuid.Nil.String(), token.Labels[teleport.LabelNodePool])
			return true
		})).Return(nil)

		err = controller.createTokenForNode(ctx, node, validEKPubHash("0001"))
		require.NoError(t, err)

		mockTeleport.AssertExpectations(t)
	})

	t.Run("fails when node context resolution errors", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := db.Node{
			ID:         uuid.New(),
			ClusterID:  te.cluster.ID,
			NodePoolID: te.nodePool.ID,
			NodeSetID:  uuid.New(), // Non-existent node set
			Name:       "test-node",
			IP:         "10.0.0.1",
			Nico: &db.NicoNodeProvider{
				ForgeInstanceID: "forge-123",
				ForgeMachineID:  "machine-1",
			},
		}

		err := controller.createTokenForNode(ctx, node, validEKPubHash("abcd"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to resolve node context")
	})

	t.Run("fails when GetCluster errors", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		// Create a NodePool pointing to a non-existent Cluster
		orphanPool := db.NodePool{
			ID:                   uuid.New(),
			ClusterID:            uuid.New(), // Non-existent Cluster
			Name:                 "orphan-pool",
			NodeNamePrefix:       "test",
			NodeLabels:           []string{},
			SystemReservedMemory: "16Gi",
		}
		orphanPool.SetNico(db.NicoNodePoolProvider{
			InstanceTypeID: uuid.New(),
			SSHKeyGroupIDs: []uuid.UUID{},
			SubnetIDs:      []uuid.UUID{uuid.New()},
			VpcPrefixIDs:   []uuid.UUID{uuid.New()},
			OSProfile:      "test-os",
			ConfigProfile:  "test-config",
		})
		_, err := te.bunDB.NewInsert().Model(&orphanPool).Exec(ctx)
		require.NoError(t, err)

		node := db.Node{
			ID:         uuid.New(),
			ClusterID:  orphanPool.ClusterID,
			NodePoolID: orphanPool.ID,
			NodeSetID:  te.nodeSet.ID,
			Name:       "test-node",
			IP:         "10.0.0.1",
			Nico: &db.NicoNodeProvider{
				ForgeInstanceID: "forge-123",
				ForgeMachineID:  "machine-1",
			},
		}

		err = controller.createTokenForNode(ctx, node, validEKPubHash("abcd"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get cluster")
	})

	t.Run("fails when UpsertToken errors", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "test-node", "forge-123", validEKPubHash("abcd"))

		mockTeleport.On("UpsertToken", ctx, mock.Anything).Return(errors.New("teleport error"))

		err := controller.createTokenForNode(ctx, node, validEKPubHash("abcd"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to upsert Teleport TPM token")
	})
}

func TestTokenUsesResolvedEKPubHash(t *testing.T) {
	ctx := context.Background()
	te := setupTestEnv(ctx, t)
	mockTeleport := &mockTeleportClient{}
	controller := te.newController(mockTeleport)

	resolvedHash := validEKPubHash("beef")
	node := te.createNode(ctx, t, "test-node", "forge-123", resolvedHash)

	mockTeleport.On("UpsertToken", ctx, mock.MatchedBy(func(token teleport.ProvisionToken) bool {
		require.NotNil(t, token.Spec)
		tpmConfig, ok := token.Spec.(*teleport.TPMConfig)
		require.True(t, ok, "Spec should be *teleport.TPMConfig")
		require.Len(t, tpmConfig.Allow, 1)
		return tpmConfig.Allow[0].EKPublicHash == resolvedHash
	})).Return(nil)

	err := controller.createTokenForNode(ctx, node, resolvedHash)
	require.NoError(t, err)

	mockTeleport.AssertExpectations(t)
}

func TestCreateTokenForNode_InvalidEKPubHash(t *testing.T) {
	ctx := context.Background()
	te := setupTestEnv(ctx, t)
	mockTeleport := &mockTeleportClient{}
	controller := te.newController(mockTeleport)

	node := db.Node{
		ID:         uuid.New(),
		ClusterID:  te.cluster.ID,
		NodePoolID: te.nodePool.ID,
		NodeSetID:  te.nodeSet.ID,
		Name:       "test-node",
		IP:         "10.0.0.1",
		Nico: &db.NicoNodeProvider{
			ForgeInstanceID: "forge-123",
			ForgeMachineID:  "machine-1",
		},
	}

	// UpsertToken should NOT be called because validation fails first
	err := controller.createTokenForNode(ctx, node, "invalid-hash")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid EK public key hash")
	assert.Contains(t, err.Error(), node.ID.String())

	mockTeleport.AssertNotCalled(t, "UpsertToken")
}

func TestTokenCreationTracking(t *testing.T) {
	ctx := context.Background()

	t.Run("skips token upsert for recently created tokens", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))

		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{}, nil)
		mockTeleport.On("UpsertToken", ctx, mock.Anything).Return(nil)

		// First reconcile - should create token
		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)
		assert.Equal(t, TokenRefreshThreshold, result.RequeueAfter)

		// Second reconcile - should skip token (recently created)
		result, err = controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)
		assert.Positive(t, result.RequeueAfter)

		// UpsertToken should have been called exactly once
		mockTeleport.AssertNumberOfCalls(t, "UpsertToken", 1)
	})

	t.Run("refreshes token after threshold passes", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))

		// Inject a stale entry (older than TokenRefreshThreshold)
		controller.tokenCreatedAt[node.ID] = time.Now().Add(-TokenRefreshThreshold - time.Minute)

		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{}, nil)
		mockTeleport.On("UpsertToken", ctx, mock.Anything).Return(nil)

		result, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)
		assert.Equal(t, TokenRefreshThreshold, result.RequeueAfter)

		// UpsertToken should be called because the cached token is stale
		mockTeleport.AssertNumberOfCalls(t, "UpsertToken", 1)
	})

	t.Run("cleans up cache on registration confirmation", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))

		// Pre-populate cache as if we previously created a token
		controller.tokenCreatedAt[node.ID] = time.Now()

		// Teleport reports node as registered
		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{
			{
				Hostname: "node-1",
				Labels: map[string]string{
					teleport.LabelManagedBy: teleport.LabelManagedByValue,
					teleport.LabelNodeID:    node.ID.String(),
				},
			},
		}, nil)

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)

		// Cache entry should be removed after registration confirmation
		_, exists := controller.tokenCreatedAt[node.ID]
		assert.False(t, exists, "tokenCreatedAt entry should be removed after registration confirmation")

		// UpsertToken should NOT have been called (node was confirmed registered)
		mockTeleport.AssertNotCalled(t, "UpsertToken")
	})

	t.Run("fresh controller creates tokens normally", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		node1 := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))
		node2 := te.createNode(ctx, t, "node-2", "forge-2", validEKPubHash("0002"))

		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{}, nil)
		mockTeleport.On("UpsertToken", ctx, mock.Anything).Return(nil)

		// Empty cache (fresh controller) should create tokens for all nodes
		assert.Empty(t, controller.tokenCreatedAt)

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node1.ID))
		require.NoError(t, err)

		_, err = controller.Reconcile(ctx, dbreconcile.NodeKey(node2.ID))
		require.NoError(t, err)

		mockTeleport.AssertNumberOfCalls(t, "UpsertToken", 2)
		assert.Len(t, controller.tokenCreatedAt, 2)
	})
}

func TestObservedStateReconciliation(t *testing.T) {
	ctx := context.Background()

	t.Run("deconfirms node registered in DB but absent from Teleport", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		// Node is marked registered in DB
		node := te.createRegisteredNode(ctx, t, "stale-node", "forge-1", validEKPubHash("0001"))

		// Teleport returns empty - node is gone
		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{}, nil)
		mockTeleport.On("UpsertToken", ctx, mock.MatchedBy(func(token teleport.ProvisionToken) bool {
			return token.Name == teleport.TPMTokenName(node.ID)
		})).Return(nil)

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)

		// Verify node condition was flipped back to False
		updatedNode, err := te.database.GetNode(ctx, node.ID)
		require.NoError(t, err)
		assert.Equal(t, db.ConditionStatusFalse, updatedNode.Conditions.TeleportRegistered.Status)
		assert.Contains(t, updatedNode.Conditions.TeleportRegistered.Message, "Not found in Teleport")

		// Verify a new token was created
		mockTeleport.AssertCalled(t, "UpsertToken", ctx, mock.Anything)
	})

	t.Run("steady state - registered node still in Teleport is a no-op", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		// Node is marked registered in DB
		node := te.createRegisteredNode(ctx, t, "steady-node", "forge-1", validEKPubHash("0001"))

		// Teleport confirms node is still there
		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{
			{
				Hostname: "steady-node",
				Labels: map[string]string{
					teleport.LabelManagedBy: teleport.LabelManagedByValue,
					teleport.LabelNodeID:    node.ID.String(),
				},
			},
		}, nil)

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)

		// Verify node condition is still True
		updatedNode, err := te.database.GetNode(ctx, node.ID)
		require.NoError(t, err)
		assert.Equal(t, db.ConditionStatusTrue, updatedNode.Conditions.TeleportRegistered.Status)

		// No token should have been created and no DB writes needed
		mockTeleport.AssertNotCalled(t, "UpsertToken")
	})

	t.Run("newly confirmed - node appears in Teleport with DB condition False", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		// Node starts unregistered (default condition)
		node := te.createNode(ctx, t, "new-node", "forge-1", validEKPubHash("0001"))

		// Teleport reports node as registered
		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{
			{
				Hostname: "new-node",
				Labels: map[string]string{
					teleport.LabelManagedBy: teleport.LabelManagedByValue,
					teleport.LabelNodeID:    node.ID.String(),
				},
			},
		}, nil)

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)

		// Verify node condition was set to True
		updatedNode, err := te.database.GetNode(ctx, node.ID)
		require.NoError(t, err)
		assert.Equal(t, db.ConditionStatusTrue, updatedNode.Conditions.TeleportRegistered.Status)

		// No token should have been created
		mockTeleport.AssertNotCalled(t, "UpsertToken")
	})

	t.Run("deconfirmed node skips token if recently created", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		// Node is marked registered in DB
		node := te.createRegisteredNode(ctx, t, "stale-cached", "forge-1", validEKPubHash("0001"))

		// Simulate that we recently created a token for this node
		controller.tokenCreatedAt[node.ID] = time.Now()

		// Teleport returns empty - node is gone
		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{}, nil)

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node.ID))
		require.NoError(t, err)

		// Condition should still be flipped to False (deconfirmed)
		updatedNode, err := te.database.GetNode(ctx, node.ID)
		require.NoError(t, err)
		assert.Equal(t, db.ConditionStatusFalse, updatedNode.Conditions.TeleportRegistered.Status)

		// But NO token should be created (recently created, within threshold)
		mockTeleport.AssertNotCalled(t, "UpsertToken")
	})

	t.Run("mixed scenario - confirmed, deconfirmed, pending, and steady-state", func(t *testing.T) {
		te := setupTestEnv(ctx, t)
		mockTeleport := &mockTeleportClient{}
		controller := te.newController(mockTeleport)

		// Node 1: unregistered, appears in Teleport -> should be confirmed
		node1 := te.createNode(ctx, t, "confirm-me", "forge-1", validEKPubHash("0001"))

		// Node 2: registered in DB, still in Teleport -> steady-state no-op
		node2 := te.createRegisteredNode(ctx, t, "steady", "forge-2", validEKPubHash("0002"))

		// Node 3: registered in DB, gone from Teleport -> deconfirm + token
		node3 := te.createRegisteredNode(ctx, t, "stale", "forge-3", validEKPubHash("0003"))

		// Node 4: unregistered, not in Teleport -> pending, create token
		node4 := te.createNode(ctx, t, "pending", "forge-4", validEKPubHash("0004"))

		// Teleport reports node1 and node2 as registered; node3 and node4 absent
		mockTeleport.On("ListNodes", ctx, TeleportNamespace, mock.Anything).Return([]teleport.Node{
			{
				Hostname: "confirm-me",
				Labels: map[string]string{
					teleport.LabelManagedBy: teleport.LabelManagedByValue,
					teleport.LabelNodeID:    node1.ID.String(),
				},
			},
			{
				Hostname: "steady",
				Labels: map[string]string{
					teleport.LabelManagedBy: teleport.LabelManagedByValue,
					teleport.LabelNodeID:    node2.ID.String(),
				},
			},
		}, nil)
		mockTeleport.On("UpsertToken", ctx, mock.Anything).Return(nil)

		_, err := controller.Reconcile(ctx, dbreconcile.NodeKey(node1.ID))
		require.NoError(t, err)

		_, err = controller.Reconcile(ctx, dbreconcile.NodeKey(node2.ID))
		require.NoError(t, err)

		_, err = controller.Reconcile(ctx, dbreconcile.NodeKey(node3.ID))
		require.NoError(t, err)

		_, err = controller.Reconcile(ctx, dbreconcile.NodeKey(node4.ID))
		require.NoError(t, err)

		// Node 1: confirmed (False -> True)
		updated1, _ := te.database.GetNode(ctx, node1.ID)
		assert.Equal(t, db.ConditionStatusTrue, updated1.Conditions.TeleportRegistered.Status)

		// Node 2: steady state (still True)
		updated2, _ := te.database.GetNode(ctx, node2.ID)
		assert.Equal(t, db.ConditionStatusTrue, updated2.Conditions.TeleportRegistered.Status)

		// Node 3: deconfirmed (True -> False)
		updated3, _ := te.database.GetNode(ctx, node3.ID)
		assert.Equal(t, db.ConditionStatusFalse, updated3.Conditions.TeleportRegistered.Status)

		// Node 4: still unregistered
		updated4, _ := te.database.GetNode(ctx, node4.ID)
		assert.NotEqual(t, db.ConditionStatusTrue, updated4.Conditions.TeleportRegistered.Status)

		// Tokens created for node3 (deconfirmed) and node4 (pending) = 2 calls
		mockTeleport.AssertNumberOfCalls(t, "UpsertToken", 2)

		// Verify the correct nodes got tokens (node3 and node4)
		for _, call := range mockTeleport.Calls {
			if call.Method == "UpsertToken" {
				token := call.Arguments[1].(teleport.ProvisionToken)
				tokenNodeID := token.Labels[teleport.LabelNodeID]
				assert.True(t,
					tokenNodeID == node3.ID.String() || tokenNodeID == node4.ID.String(),
					"Token should be for node3 or node4, got %s", tokenNodeID,
				)
			}
		}
	})
}

func TestTeleportDependencyKeyMappers(t *testing.T) {
	ctx := context.Background()
	te := setupTestEnv(ctx, t)
	node1 := te.createNode(ctx, t, "node-1", "forge-1", validEKPubHash("0001"))
	node2 := te.createNode(ctx, t, "node-2", "forge-2", validEKPubHash("0002"))
	otherNodeSet := db.NodeSet{
		ID:                   uuid.New(),
		NodePoolID:           te.nodePool.ID,
		SystemReservedMemory: "16Gi",
		TeleportEnabled:      true,
		Nico: &db.NicoNodePoolProvider{
			OSProfile:     "test-os",
			ConfigProfile: "test-config",
		},
	}
	_, err := te.database.CreateNodeSet(ctx, &otherNodeSet)
	require.NoError(t, err)
	otherNode := db.Node{
		ID:         uuid.New(),
		ClusterID:  te.cluster.ID,
		NodePoolID: te.nodePool.ID,
		NodeSetID:  otherNodeSet.ID,
		Name:       "other-node",
		IP:         "10.0.0.3",
		Nico: &db.NicoNodeProvider{
			ForgeInstanceID: "forge-3",
			ForgeMachineID:  "machine-other-node",
		},
	}
	otherNode, err = te.database.CreateNode(ctx, &otherNode)
	require.NoError(t, err)
	te.createNicoMachineForNode(ctx, t, otherNode, validEKPubHash("0003"))

	controller := te.newController(&mockTeleportClient{})

	clusterKeys, err := controller.nodeKeysForCluster(ctx, te.cluster)
	require.NoError(t, err)
	assert.ElementsMatch(t, []dbreconcile.Key{
		dbreconcile.NodeKey(node1.ID),
		dbreconcile.NodeKey(node2.ID),
		dbreconcile.NodeKey(otherNode.ID),
	}, clusterKeys)

	nodeSetKeys, err := controller.nodeKeysForNodeSet(ctx, te.nodeSet)
	require.NoError(t, err)
	assert.ElementsMatch(t, []dbreconcile.Key{
		dbreconcile.NodeKey(node1.ID),
		dbreconcile.NodeKey(node2.ID),
	}, nodeSetKeys)

	nicoMachine := &nicov1.NicoMachine{}
	require.NoError(t, te.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: constants.NamespaceNKESystem,
		Name:      node1.Name,
	}, nicoMachine))
	assert.Equal(t, []dbreconcile.Key{dbreconcile.NodeKey(node1.ID)}, controller.nodeKeysForNicoMachine(ctx, nicoMachine))

	t.Run("skips NicoMachine without owner Machine", func(t *testing.T) {
		orphan := &nicov1.NicoMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "orphan-nicomachine",
				Namespace: constants.NamespaceNKESystem,
			},
		}

		assert.Nil(t, controller.nodeKeysForNicoMachine(ctx, orphan))
	})

	t.Run("skips NicoMachine with invalid host ID annotation", func(t *testing.T) {
		controllerRef := true
		machine := &clusterv1.Machine{
			TypeMeta: metav1.TypeMeta{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Machine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "invalid-host-machine",
				Namespace: constants.NamespaceNKESystem,
				Annotations: map[string]string{
					constants.MachineAnnotationKeyHostID: "not-a-uuid",
				},
			},
		}
		require.NoError(t, te.k8sClient.Create(ctx, machine))

		nicoMachine := &nicov1.NicoMachine{
			TypeMeta: metav1.TypeMeta{
				APIVersion: nicov1.GroupVersion.String(),
				Kind:       "NicoMachine",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "invalid-host-machine",
				Namespace: constants.NamespaceNKESystem,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Machine",
					Name:       "invalid-host-machine",
					Controller: &controllerRef,
				}},
			},
		}

		assert.Nil(t, controller.nodeKeysForNicoMachine(ctx, nicoMachine))
	})
}
