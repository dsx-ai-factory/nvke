// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package teleporttoken

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	capiutil "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/metrics"
	"github.com/NVIDIA/nke/controller/internal/nodes"
	"github.com/NVIDIA/nke/controller/internal/teleport"
)

const (
	// TeleportNamespace is the Teleport namespace for SSH nodes.
	// Teleport uses "default" for all SSH nodes; this is not a Kubernetes namespace.
	TeleportNamespace = "default"

	// TPMTokenTTL is the expiry duration for TPM join tokens.
	TPMTokenTTL = 2 * time.Hour

	// TokenRefreshThreshold is the minimum token age before re-upserting.
	TokenRefreshThreshold = TPMTokenTTL
)

type teleportReconcileCounters struct {
	eligible      int
	confirmed     int
	deconfirmed   int
	tokensCreated int
	tokensSkipped int
	tokensFailed  int
}

// Controller reconciles Teleport TPM join tokens for eligible nodes.
type Controller struct {
	cfg            config.Config
	db             *dbwrapper.DB
	k8sClient      client.Client
	logger         *slog.Logger
	teleportClient teleport.Client
	// tokenCreatedAt tracks when each node's token was last upserted.
	// Resets on restart; first post-restart cycle re-upserts (idempotent).
	tokenCreatedAt map[uuid.UUID]time.Time
}

// NewController creates a new Controller.
func NewController(
	cfg config.Config,
	dbInstance *dbwrapper.DB,
	k8sClient client.Client,
	logger *slog.Logger,
	teleportClient teleport.Client,
) *Controller {
	return &Controller{
		cfg:            cfg,
		db:             dbInstance,
		k8sClient:      k8sClient,
		logger:         logger.With("controller", "teleport-token"),
		teleportClient: teleportClient,
		tokenCreatedAt: make(map[uuid.UUID]time.Time),
	}
}

// SetupWithManager registers the DB-rooted Teleport token reconciler.
//
// NodeID is the steady-state root key: eligible node changes, token TTLs, and
// registration waits reconcile the specific node. Cluster and NodeSet events are
// dependency invalidations only: they fan out to affected NodeKeys before they
// reach Reconcile because those rows decide whether existing nodes are eligible.
// NodePool Teleport enablement materializes through NodeSet changes in the
// existing NodePool reconciler, so this controller intentionally watches NodeSets
// instead of adding a direct NodePool source.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"teleport-token",
		c,
		dbwrapper.NewNodeSource(c.db, dbfilters.Node{Deleting: ptr.To(false)}),
		dbwrapper.NewClusterSource(c.db, dbfilters.Cluster{}, c.nodeKeysForCluster),
		dbwrapper.NewNodeSetSource(c.db, dbfilters.NodeSet{}, c.nodeKeysForNodeSet),
		source.TypedKind(
			mgr.GetCache(),
			&nicov1.NicoMachine{},
			handler.TypedEnqueueRequestsFromMapFunc(c.nodeKeysForNicoMachine),
		),
	)
}

func (c *Controller) nodeKeysForCluster(ctx context.Context, cluster db.Cluster) ([]dbreconcile.Key, error) {
	nodeList, err := c.db.ListNodes(ctx, db.NodeFilters{ClusterID: &cluster.ID})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes for cluster %s: %w", cluster.ID, err)
	}
	return nodeKeys(nodeList), nil
}

func (c *Controller) nodeKeysForNodeSet(ctx context.Context, nodeSet db.NodeSet) ([]dbreconcile.Key, error) {
	nodeList, err := c.db.ListNodes(ctx, db.NodeFilters{NodeSetID: &nodeSet.ID})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes for node set %s: %w", nodeSet.ID, err)
	}
	return nodeKeys(nodeList), nil
}

func nodeKeys(nodes []db.Node) []dbreconcile.Key {
	keys := make([]dbreconcile.Key, 0, len(nodes))
	for _, node := range nodes {
		if node.ID != uuid.Nil {
			keys = append(keys, dbreconcile.NodeKey(node.ID))
		}
	}
	return keys
}

func (c *Controller) nodeKeysForNicoMachine(ctx context.Context, nicoMachine *nicov1.NicoMachine) []dbreconcile.Key {
	machine, err := capiutil.GetOwnerMachine(ctx, c.k8sClient, nicoMachine.ObjectMeta)
	if err != nil {
		c.logger.Error("Failed to get owner Machine for NicoMachine", "namespace", nicoMachine.Namespace, "name", nicoMachine.Name, "error", err)
		return nil
	}
	if machine == nil {
		c.logger.Debug("NicoMachine has no owner Machine, skipping Teleport token reconciliation", "namespace", nicoMachine.Namespace, "name", nicoMachine.Name)
		return nil
	}

	hostID := machine.GetAnnotations()[constants.MachineAnnotationKeyHostID]
	nodeID, err := uuid.Parse(hostID)
	if err != nil {
		c.logger.Error("Machine has invalid host ID annotation, skipping Teleport token reconciliation",
			"namespace", machine.Namespace,
			"name", machine.Name,
			"hostID", hostID,
			"error", err,
		)
		return nil
	}

	return []dbreconcile.Key{dbreconcile.NodeKey(nodeID)}
}

// Reconcile derives Teleport registration state from the live Teleport API for
// a node key. For each eligible node, it compares the DB condition against
// actual Teleport presence to determine the correct action:
//
//   - inTeleport && DB=false  -> confirm registration (set condition True)
//   - inTeleport && DB=true   -> no-op (steady state)
//   - !inTeleport && DB=true  -> deconfirm (set condition False) and create token
//   - !inTeleport && DB=false -> create token (still pending)
func (c *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	node, err := c.db.GetNode(ctx, key.ID)
	if errors.Is(err, db.ErrNotFound) {
		delete(c.tokenCreatedAt, key.ID)
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get node %s: %w", key.ID, err)
	}

	var countersBySite map[uuid.UUID]*teleportReconcileCounters
	defer func() {
		c.recordMetrics(countersBySite)
	}()

	if isNodeIneligible(node) {
		delete(c.tokenCreatedAt, node.ID)
		return reconcile.Result{}, nil
	}

	cluster, err := c.clusterForNode(ctx, node)
	if err != nil {
		return reconcile.Result{}, err
	}

	siteID := cluster.SiteID
	eligible, err := c.isTeleportEligible(ctx, node, cluster)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !eligible {
		delete(c.tokenCreatedAt, node.ID)
		return reconcile.Result{}, nil
	}

	counters := &teleportReconcileCounters{}
	countersBySite = map[uuid.UUID]*teleportReconcileCounters{siteID: counters}

	tpmEKPubHash, err := c.getPubHashFromNicoMachine(ctx, node)
	if err != nil {
		return reconcile.Result{}, err
	}
	if tpmEKPubHash == "" {
		delete(c.tokenCreatedAt, node.ID)
		c.logger.Debug("Waiting for NicoMachine TPM EK public hash before creating Teleport token", "nodeID", node.ID, "nodeName", node.Name)
		return reconcile.Result{}, nil
	}

	counters.eligible++

	inTeleport, err := c.isNodeRegistered(ctx, siteID, node.ID)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to list Teleport nodes (aborting to prevent incorrect state): %w", err)
	}

	dbRegistered := node.Conditions.TeleportRegistered.Status == db.ConditionStatusTrue
	switch {
	case inTeleport && !dbRegistered:
		if err := c.confirmNode(ctx, siteID, node); err != nil {
			c.logger.Error("Failed to confirm node registration", "nodeID", node.ID, "nodeName", node.Name, "error", err)
			return reconcile.Result{}, err
		}
		counters.confirmed++
		node.Conditions.TeleportRegistered = db.NewCondition(true, "Registered with Teleport")
	case !inTeleport && dbRegistered:
		if err := c.deconfirmNode(ctx, node); err != nil {
			c.logger.Error("Failed to deconfirm stale registration", "nodeID", node.ID, "nodeName", node.Name, "error", err)
			return reconcile.Result{}, err
		}
		counters.deconfirmed++
		node.Conditions.TeleportRegistered = db.NewCondition(false, "Not found in Teleport, re-enrolling")
	}

	if node.Conditions.TeleportRegistered.Status == db.ConditionStatusTrue {
		return reconcile.Result{}, nil
	}

	created, requeueAfter, err := c.ensureTokenForNode(ctx, node, tpmEKPubHash)
	switch {
	case err != nil:
		counters.tokensFailed++
		return reconcile.Result{RequeueAfter: requeueAfter}, err
	case created:
		counters.tokensCreated++
	default:
		counters.tokensSkipped++
	}
	return reconcile.Result{RequeueAfter: requeueAfter}, nil
}

func (c *Controller) recordMetrics(countersBySite map[uuid.UUID]*teleportReconcileCounters) {
	for siteID, counters := range countersBySite {
		c.logger.Info("Teleport token reconciliation complete",
			"siteID", siteID,
			"eligible", counters.eligible,
			"confirmed", counters.confirmed,
			"deconfirmed", counters.deconfirmed,
			"tokensCreated", counters.tokensCreated,
			"tokensSkipped", counters.tokensSkipped,
			"tokensFailed", counters.tokensFailed,
		)

		siteIDStr := siteID.String()
		metrics.RecordTeleportTokensCreated(siteIDStr, counters.tokensCreated)
		metrics.RecordTeleportTokensSkipped(siteIDStr, counters.tokensSkipped)
		metrics.RecordTeleportTokenErrors(siteIDStr, counters.tokensFailed)
		metrics.RecordTeleportConfirmation(siteIDStr, counters.confirmed)
		metrics.RecordTeleportDeconfirmation(siteIDStr, counters.deconfirmed)
	}
}

func (c *Controller) clusterForNode(ctx context.Context, node db.Node) (db.Cluster, error) {
	cluster, err := c.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return db.Cluster{}, fmt.Errorf("failed to get cluster for node %s: %w", node.ID, err)
	}
	return cluster, nil
}

func isNodeIneligible(node db.Node) bool {
	return node.Conditions.Deleting.Status == db.ConditionStatusTrue
}

func (c *Controller) getPubHashFromNicoMachine(ctx context.Context, node db.Node) (string, error) {
	nicoMachine := &nicov1.NicoMachine{}
	if err := c.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: constants.NamespaceNKESystem,
		Name:      node.Name,
	}, nicoMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to get NicoMachine for node %s (%s): %w", node.ID, node.Name, err)
	}
	return nicoMachine.Status.TpmEkPubHash, nil
}

func (c *Controller) isTeleportEligible(ctx context.Context, node db.Node, cluster db.Cluster) (bool, error) {
	if !cluster.TeleportEnabled {
		return false, nil
	}

	if node.ControlPlane && node.NodeSetID == uuid.Nil {
		return true, nil
	}

	if node.NodeSetID == uuid.Nil {
		return false, nil
	}

	nodeSet, err := c.db.GetNodeSet(ctx, node.NodeSetID)
	if err != nil {
		return false, fmt.Errorf("failed to get node set for node %s: %w", node.ID, err)
	}

	return nodeSet.TeleportEnabled, nil
}

func (c *Controller) isNodeRegistered(ctx context.Context, siteID, nodeID uuid.UUID) (bool, error) {
	registeredNodeIDs, err := c.fetchRegisteredNodeIDs(ctx, map[string]string{
		teleport.LabelSite:   siteID.String(),
		teleport.LabelNodeID: nodeID.String(),
	})
	if err != nil {
		return false, err
	}
	return registeredNodeIDs[nodeID.String()], nil
}

// fetchRegisteredNodeIDs queries Teleport for registered CWE-managed nodes matching
// the given labels and returns a set of node IDs (by UUID string) for O(1) lookup.
func (c *Controller) fetchRegisteredNodeIDs(ctx context.Context, labels map[string]string) (map[string]bool, error) {
	registeredNodes, err := c.teleportClient.ListNodes(ctx, TeleportNamespace, teleport.WithLabels(labels))
	if err != nil {
		return nil, fmt.Errorf("failed to list registered nodes from Teleport: %w", err)
	}

	registeredNodeIDs := make(map[string]bool, len(registeredNodes))
	for _, node := range registeredNodes {
		if nodeID := node.Labels[teleport.LabelNodeID]; nodeID != "" {
			registeredNodeIDs[nodeID] = true
		}
	}
	return registeredNodeIDs, nil
}

// confirmNode updates the DB to mark a node as registered with Teleport.
// Caller should log and continue on error.
func (c *Controller) confirmNode(ctx context.Context, siteID uuid.UUID, node db.Node) error {
	condition := db.NewCondition(true, "Registered with Teleport")
	if err := c.db.UpdateNodeTeleportRegistered(ctx, node.ID, condition); err != nil {
		return err
	}
	c.logger.Info("Node confirmed registered with Teleport",
		"nodeID", node.ID,
		"nodeName", node.Name,
	)
	if createdAt, ok := c.tokenCreatedAt[node.ID]; ok {
		metrics.RecordTeleportRegistrationLatency(siteID.String(), time.Since(createdAt).Seconds())
	}
	delete(c.tokenCreatedAt, node.ID)
	return nil
}

// deconfirmNode updates the DB to mark a previously-registered node as unregistered.
// This is the anti-entropy path: the node was registered in the DB but is absent from Teleport.
// Caller should log and continue on error.
func (c *Controller) deconfirmNode(ctx context.Context, node db.Node) error {
	condition := db.NewCondition(false, "Not found in Teleport, re-enrolling")
	if err := c.db.UpdateNodeTeleportRegistered(ctx, node.ID, condition); err != nil {
		return err
	}
	c.logger.Warn("Node deconfirmed: was registered in DB but absent from Teleport",
		"nodeID", node.ID,
		"nodeName", node.Name,
	)
	return nil
}

// ensureTokenForNode creates a Teleport TPM join token for a single node,
// skipping if a token was recently created (within TokenRefreshThreshold).
// Returns whether a token was created, when to requeue for registration/TTL
// checks, and an error when token creation fails.
func (c *Controller) ensureTokenForNode(ctx context.Context, node db.Node, ekPubHash string) (bool, time.Duration, error) {
	if createdAt, ok := c.tokenCreatedAt[node.ID]; ok {
		tokenAge := time.Since(createdAt)
		if tokenAge < TokenRefreshThreshold {
			requeueAfter := TokenRefreshThreshold - tokenAge
			c.logger.Debug("Skipping token upsert - recently created",
				"nodeID", node.ID,
				"nodeName", node.Name,
				"tokenAge", tokenAge,
				"refreshThreshold", TokenRefreshThreshold,
				"requeueAfter", requeueAfter,
			)
			return false, requeueAfter, nil
		}
	}

	if err := c.createTokenForNode(ctx, node, ekPubHash); err != nil {
		c.logger.Error("Failed to create Teleport token for node",
			"nodeID", node.ID,
			"nodeName", node.Name,
			"error", err,
		)
		return false, 0, fmt.Errorf("node %s (%s): %w", node.ID, node.Name, err)
	}

	c.tokenCreatedAt[node.ID] = time.Now()
	return true, TokenRefreshThreshold, nil
}

// createTokenForNode creates a Teleport TPM join token for a node.
func (c *Controller) createTokenForNode(ctx context.Context, node db.Node, ekPubHash string) error {
	_, nodePool, err := nodes.GetNodeContext(ctx, c.db, node)
	if err != nil {
		return fmt.Errorf("failed to resolve node context: %w", err)
	}

	cluster, err := c.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	if err := teleport.ValidateEKPubHash(ekPubHash); err != nil {
		return fmt.Errorf("invalid EK public key hash for node %s: %w", node.ID, err)
	}

	tokenName := teleport.TPMTokenName(node.ID)
	expiresAt := time.Now().Add(TPMTokenTTL)

	c.logger.Info("Creating Teleport TPM join token",
		"nodeID", node.ID,
		"nodeName", node.Name,
		"forgeInstanceID", node.ForgeInstanceID,
		"tokenName", tokenName,
		"expiresAt", expiresAt,
	)
	nicoProvider, err := cluster.NicoProvider()
	if err != nil {
		return err
	}

	token := teleport.ProvisionToken{
		Name:       tokenName,
		Roles:      []string{"Node"},
		Expiry:     expiresAt,
		JoinMethod: teleport.JoinMethodTPM,
		Labels: map[string]string{
			teleport.LabelSite:                  nicoProvider.SiteID.String(),
			teleport.LabelNKESiteControllerName: c.cfg.NKESiteControllerName,
			teleport.LabelCluster:               cluster.ID.String(),
			teleport.LabelClusterName:           cluster.Name,
			teleport.LabelNodeID:                node.ID.String(),
			teleport.LabelNodePool:              nodePool.ID.String(),
			teleport.LabelNodeName:              node.Name,
			teleport.LabelResourceGroup:         teleport.LabelResourceGroupNKEValue,
			teleport.LabelEnvironment:           c.cfg.Teleport.Environment,
		},
		Spec: &teleport.TPMConfig{
			Allow: []teleport.TPMRule{{
				Description:  fmt.Sprintf("NKE node %s", node.Name),
				EKPublicHash: ekPubHash,
			}},
		},
	}

	if err := c.teleportClient.UpsertToken(ctx, token); err != nil {
		return fmt.Errorf("failed to upsert Teleport TPM token: %w", err)
	}

	c.logger.Info("Teleport TPM join token created",
		"tokenName", tokenName,
		"nodeID", node.ID,
		"nodeName", node.Name,
	)

	return nil
}
