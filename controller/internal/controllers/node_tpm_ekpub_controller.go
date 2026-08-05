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

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiconditions "sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	"github.com/NVIDIA/nke/controller/internal/teleport"
	"github.com/NVIDIA/nke/controller/internal/tpm"
	nicov1 "gitlab-master.nvidia.com/nke/cluster-api-provider-nico/api/v1alpha1"
	forgeapi "gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go"
)

const (
	tpmSourceForgeAPI = "forge-api"
	tpmSourceNicoCR   = "nico-cr"
)

// NodeTPMEKPubController is responsible for reconciling the TPM EK public key hash for nodes.
type NodeTPMEKPubController struct {
	cfg           config.Config
	db            db.DB
	logger        *slog.Logger
	forgeProvider *forge.ForgeProvider
	k8sClient     client.Client
}

func NewNodeTPMEKPubController(
	cfg config.Config,
	dbInstance db.DB,
	logger *slog.Logger,
	forgeProvider *forge.ForgeProvider,
	k8sClient client.Client,
) *NodeTPMEKPubController {
	return &NodeTPMEKPubController{
		cfg:           cfg,
		db:            dbInstance,
		forgeProvider: forgeProvider,
		k8sClient:     k8sClient,
		logger:        logger,
	}
}

// Reconcile TPM EK public key hashes, for each node in the site:
//   - Find nodes with missing TPM EK public key hashes.
//   - Route each node independently (hybrid Forge/CAPNICo clusters are supported):
//   - If a NicoMachine exists for the node, read the hash from NicoMachine status;
//   - Otherwise, try to fetch the EK certificate from the Forge API.
//   - Persist the hash once the backing instance is Ready.
func (c *NodeTPMEKPubController) Reconcile(ctx context.Context, site db.Site) error {
	nodes, err := c.db.ListNodes(ctx, db.NodeFilters{
		SiteID:       &site.ID,
		TpmEKPubHash: pointerTo(""),
	})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	var errs []error
	for _, node := range nodes {
		cluster, err := c.db.GetCluster(ctx, node.ClusterID)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to get cluster for node %s (%s): %w", node.ID, node.Name, err))
			continue
		}

		if err := c.reconcileNodeEKPub(ctx, node, cluster, c.cfg.EnableForgeCapi); err != nil {
			// collect errors but continue to the next node
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcileNodeEKPub routes a node to the NicoMachine path when a CR exists,
// otherwise falls back to the Forge API.
func (c *NodeTPMEKPubController) reconcileNodeEKPub(ctx context.Context, node db.Node, cluster db.Cluster, capiEnabled bool) error {
	if capiEnabled {
		nicoMachine, nicoFound, err := c.getNicoMachine(ctx, node, cluster)
		if err != nil {
			return err
		}
		if nicoFound {
			return c.reconcileNicoNodeEKPub(ctx, node, nicoMachine)
		}
	}

	// Newly provisioned CAPNICo nodes must keep node.forge_instance_id empty for this flow to work as intended. This makes
	// the Forge fallback a no-op while their NicoMachine is being created. Adopted Forge nodes may retain node.forge_instance_id;
	// once their NicoMachine exists, its status takes precedence.
	// FIXME: https://jirasw.nvidia.com/browse/NKX-13411 - Remove the Forge fallback once this is implemented;
	// and report a per-node error when the expected NicoMachine is missing after migration.
	return c.reconcileForgeNodeEKPub(ctx, node, cluster)
}

func (c *NodeTPMEKPubController) getNicoMachine(ctx context.Context, node db.Node, cluster db.Cluster) (*nicov1.NicoMachine, bool, error) {
	nicoMachine := &nicov1.NicoMachine{}
	// buildNicoMachine names the NicoMachine CR after the database node name, so
	// node.Name is the corresponding NicoMachine lookup key.
	if err := c.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: capi.ClusterNamespace(cluster.Name),
		Name:      node.Name,
	}, nicoMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get NicoMachine for node %s (%s): %w", node.ID, node.Name, err)
	}
	return nicoMachine, true, nil
}

func (c *NodeTPMEKPubController) reconcileForgeNodeEKPub(ctx context.Context, node db.Node, cluster db.Cluster) error {
	if node.ForgeInstanceID == "" {
		return nil
	}
	if cluster.SiteID == uuid.Nil || cluster.OrgID == "" {
		return nil
	}

	forgeInstance, err := c.forgeProvider.GetInstanceByID(ctx, cluster.OrgID, cluster.SiteID.String(), node.ForgeInstanceID)
	if err != nil {
		return fmt.Errorf("failed to get forge instance for node: %s (%s): %w", node.ID, node.Name, err)
	}

	if !isForgeInstanceAvailable(forgeInstance) {
		c.logger.Debug("Forge instance is not ready, TPM EK public key hash is not available yet",
			"nodeName", node.Name,
			"nodeID", node.ID,
			"forgeInstanceID", node.ForgeInstanceID,
			"forgeInstanceStatus", ptr.Deref(forgeInstance.Status, ""),
		)
		return nil
	}

	tpmEkCertS := forgeInstance.GetTpmEkCertificate()
	if tpmEkCertS == "" {
		return fmt.Errorf("forge did not return a TPM EK certificate for ready instance: %s (%s)", node.ID, node.Name)
	}

	tpmEKPubHash, err := tpm.ComputePublicKeyHash(tpmEkCertS)
	if err != nil {
		return fmt.Errorf("failed to compute TPM public key hash for node: %s (%s): %w", node.ID, node.Name, err)
	}

	return c.persistNodeTPMEKPubHash(ctx, node, tpmEKPubHash, tpmSourceForgeAPI)
}

func (c *NodeTPMEKPubController) reconcileNicoNodeEKPub(ctx context.Context, node db.Node, nicoMachine *nicov1.NicoMachine) error {
	if !nicoMachine.DeletionTimestamp.IsZero() {
		c.logger.Debug("NicoMachine is being deleted, skipping TPM EK public key hash reconciliation",
			"nodeName", node.Name,
			"nodeID", node.ID,
		)
		return nil
	}

	if !isNicoMachineAvailable(nicoMachine) {
		c.logger.Debug("NicoMachine is not ready, TPM EK public key hash is not available yet",
			"nodeName", node.Name,
			"nodeID", node.ID,
			"providerID", nicoMachine.Spec.ProviderID,
		)
		return nil
	}

	tpmEKPubHash := nicoMachine.Status.TpmEkPubHash
	if tpmEKPubHash == "" {
		return fmt.Errorf("ready NicoMachine has no TPM EK public key hash for node: %s (%s)", node.ID, node.Name)
	}
	if err := teleport.ValidateEKPubHash(tpmEKPubHash); err != nil {
		return fmt.Errorf("invalid TPM EK public key hash on NicoMachine for node %s (%s): %w", node.ID, node.Name, err)
	}

	return c.persistNodeTPMEKPubHash(ctx, node, tpmEKPubHash, tpmSourceNicoCR)
}

func (c *NodeTPMEKPubController) persistNodeTPMEKPubHash(ctx context.Context, node db.Node, tpmEKPubHash, tpmSource string) error {
	// Re-read under a row lock so we only mutate TpmEKPubHash on the latest
	// persisted state. Without the lock, the whole-row write would clobber
	// node conditions updated concurrently by other writers.
	if err := c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		lockedNode, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return err
		}
		lockedNode.TpmEKPubHash = tpmEKPubHash
		_, err = tx.UpdateNode(ctx, &lockedNode)
		return err
	}); err != nil {
		return fmt.Errorf("failed to update TPM public key hash for node: %s (%s): %w", node.ID, node.Name, err)
	}

	c.logger.Info("Set TPM EK public key hash for node",
		"nodeName", node.Name,
		"nodeID", node.ID,
		"tpmSource", tpmSource,
	)
	return nil
}

func isForgeInstanceAvailable(forgeInstance *forgeapi.Instance) bool {
	if forgeInstance == nil || forgeInstance.Status == nil {
		return false
	}
	return *forgeInstance.Status == forgeapi.INSTANCESTATUS_READY
}

// isNicoMachineAvailable follows the CAPI InfraMachine readiness contract:
// prefer status.conditions[type=Ready] (v1beta2), fall back to status.ready (v1beta1).
// https://cluster-api.sigs.k8s.io/developer/providers/contracts/infra-machine#compatibility-with-the-deprecated-v1beta1-contract-1
func isNicoMachineAvailable(nicoMachine *nicov1.NicoMachine) bool {
	if nicoMachine == nil {
		return false
	}
	if readyCondition := capiconditions.Get(nicoMachine, clusterv1.ReadyCondition); readyCondition != nil {
		return readyCondition.Status == metav1.ConditionTrue
	}
	return nicoMachine.Status.Ready
}
