// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/controller/internal/config"
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

// TeleportTokenController reconciles Teleport TPM join tokens for eligible nodes.
type TeleportTokenController struct {
	cfg            config.Config
	db             db.DB
	logger         *slog.Logger
	teleportClient teleport.Client
	// tokenCreatedAt tracks when each node's token was last upserted.
	// Resets on restart; first post-restart cycle re-upserts (idempotent).
	tokenCreatedAt map[uuid.UUID]time.Time
}

// NewTeleportTokenController creates a new TeleportTokenController.
func NewTeleportTokenController(
	cfg config.Config,
	dbInstance db.DB,
	logger *slog.Logger,
	teleportClient teleport.Client,
) *TeleportTokenController {
	return &TeleportTokenController{
		cfg:            cfg,
		db:             dbInstance,
		logger:         logger.With("controller", "teleport-token"),
		teleportClient: teleportClient,
		tokenCreatedAt: make(map[uuid.UUID]time.Time),
	}
}

// Reconcile derives Teleport registration state from the live Teleport API on every pass,
// using the observed-state pattern. For each eligible node, it compares the DB condition
// against actual Teleport presence to determine the correct action:
//
//   - inTeleport && DB=false  -> confirm registration (set condition True)
//   - inTeleport && DB=true   -> no-op (steady state)
//   - !inTeleport && DB=true  -> deconfirm (set condition False) and create token
//   - !inTeleport && DB=false -> create token (still pending)
func (c *TeleportTokenController) Reconcile(ctx context.Context, site db.Site) error {
	allNodes, err := c.db.ListTeleportEligibleNodes(ctx, site.ID)
	if err != nil {
		return fmt.Errorf("failed to list eligible nodes: %w", err)
	}

	registeredNodeIDs, err := c.fetchRegisteredNodeIDs(ctx, site.ID)
	if err != nil {
		return fmt.Errorf("failed to list Teleport nodes (aborting to prevent incorrect state): %w", err)
	}

	c.logger.Debug("Reconciling Teleport state",
		"siteID", site.ID,
		"eligibleNodes", len(allNodes),
		"teleportRegistered", len(registeredNodeIDs),
	)

	var confirmed, deconfirmed, tokensCreated, tokensSkipped, tokensFailed int
	var reconcileErrors []error

	for _, node := range allNodes {
		inTeleport := registeredNodeIDs[node.ID.String()]
		dbRegistered := node.Conditions.TeleportRegistered.Status == db.ConditionStatusTrue

		// 1. Update DB condition if necessary (confirm or deconfirm)
		switch {
		case inTeleport && !dbRegistered:
			// Newly confirmed: node appeared in Teleport, update DB to reflect
			if err := c.confirmNode(ctx, site.ID, node); err != nil {
				c.logger.Error("Failed to confirm node registration", "nodeID", node.ID, "nodeName", node.Name, "error", err)
				reconcileErrors = append(reconcileErrors, err)
			} else {
				confirmed++
				// Update in-memory condition so "needs token" check below is skipped
				node.Conditions.TeleportRegistered = db.NewCondition(true, "Registered with Teleport")
			}
		case !inTeleport && dbRegistered:
			// Stale: DB says registered but node absent from Teleport, deconfirm
			if err := c.deconfirmNode(ctx, node); err != nil {
				c.logger.Error("Failed to deconfirm stale registration", "nodeID", node.ID, "nodeName", node.Name, "error", err)
				reconcileErrors = append(reconcileErrors, err)
			} else {
				deconfirmed++
				// Update in-memory condition so "needs token" check below runs this iteration
				node.Conditions.TeleportRegistered = db.NewCondition(false, "Not found in Teleport, re-enrolling")
			}
		}

		// 2. If not registered (in DB), ensure token exists
		if node.Conditions.TeleportRegistered.Status != db.ConditionStatusTrue {
			c.createTokenAndUpdateCounters(ctx, node, &tokensCreated, &tokensSkipped, &tokensFailed, &reconcileErrors)
		}
	}

	// Clean up tokenCreatedAt entries for nodes no longer eligible (deleted, etc.)
	eligibleIDs := make(map[uuid.UUID]struct{}, len(allNodes))
	for _, n := range allNodes {
		eligibleIDs[n.ID] = struct{}{}
	}
	for nodeID := range c.tokenCreatedAt {
		if _, ok := eligibleIDs[nodeID]; !ok {
			delete(c.tokenCreatedAt, nodeID)
		}
	}

	c.logger.Info("Teleport token reconciliation complete",
		"siteID", site.ID,
		"eligible", len(allNodes),
		"confirmed", confirmed,
		"deconfirmed", deconfirmed,
		"tokensCreated", tokensCreated,
		"tokensSkipped", tokensSkipped,
		"tokensFailed", tokensFailed,
	)

	siteIDStr := site.ID.String()
	metrics.RecordTeleportTokensCreated(siteIDStr, tokensCreated)
	metrics.RecordTeleportTokensSkipped(siteIDStr, tokensSkipped)
	metrics.RecordTeleportTokenErrors(siteIDStr, tokensFailed)
	metrics.RecordTeleportConfirmation(siteIDStr, confirmed)
	metrics.RecordTeleportDeconfirmation(siteIDStr, deconfirmed)

	return errors.Join(reconcileErrors...)
}

// fetchRegisteredNodeIDs queries Teleport for registered CWE-managed nodes at the given site
// and returns a set of node IDs (by UUID string) for O(1) lookup.
func (c *TeleportTokenController) fetchRegisteredNodeIDs(ctx context.Context, siteID uuid.UUID) (map[string]bool, error) {
	registeredNodes, err := c.teleportClient.ListNodes(ctx, TeleportNamespace,
		teleport.WithLabels(map[string]string{teleport.LabelSite: siteID.String()}),
	)
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
func (c *TeleportTokenController) confirmNode(ctx context.Context, siteID uuid.UUID, node db.Node) error {
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
func (c *TeleportTokenController) deconfirmNode(ctx context.Context, node db.Node) error {
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

// createTokenAndUpdateCounters ensures a token for the node and updates the caller's
// running counters (created/skipped/failed and reconcileErrors). Only used from Reconcile.
func (c *TeleportTokenController) createTokenAndUpdateCounters(
	ctx context.Context,
	node db.Node,
	tokensCreated, tokensSkipped, tokensFailed *int,
	reconcileErrors *[]error,
) {
	created, err := c.ensureTokenForNode(ctx, node)
	switch {
	case err != nil:
		*reconcileErrors = append(*reconcileErrors, err)
		*tokensFailed++
	case created:
		*tokensCreated++
	default:
		*tokensSkipped++
	}
}

// ensureTokenForNode creates a Teleport TPM join token for a single node,
// skipping if a token was recently created (within TokenRefreshThreshold).
// Returns (true, nil) if created, (false, nil) if skipped, (false, err) on failure.
func (c *TeleportTokenController) ensureTokenForNode(ctx context.Context, node db.Node) (bool, error) {
	if createdAt, ok := c.tokenCreatedAt[node.ID]; ok && time.Since(createdAt) < TokenRefreshThreshold {
		c.logger.Debug("Skipping token upsert - recently created",
			"nodeID", node.ID,
			"nodeName", node.Name,
			"tokenAge", time.Since(createdAt),
			"refreshThreshold", TokenRefreshThreshold,
		)
		return false, nil
	}

	if err := c.createTokenForNode(ctx, node); err != nil {
		c.logger.Error("Failed to create Teleport token for node",
			"nodeID", node.ID,
			"nodeName", node.Name,
			"error", err,
		)
		return false, fmt.Errorf("node %s (%s): %w", node.ID, node.Name, err)
	}

	c.tokenCreatedAt[node.ID] = time.Now()
	return true, nil
}

// createTokenForNode creates a Teleport TPM join token for a node.
func (c *TeleportTokenController) createTokenForNode(ctx context.Context, node db.Node) error {
	_, nodePool, err := nodes.GetNodeContext(ctx, c.db, node)
	if err != nil {
		return fmt.Errorf("failed to resolve node context: %w", err)
	}

	cluster, err := c.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	ekPubHash := node.TpmEKPubHash
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
