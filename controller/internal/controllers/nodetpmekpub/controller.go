// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodetpmekpub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiconditions "sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	forgeapi "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	"github.com/NVIDIA/nke/controller/internal/teleport"
	"github.com/NVIDIA/nke/controller/internal/tpm"
)

const (
	nodeTPMEKPubRetryAfter  = 30 * time.Second
	nicoMachineTemplateKind = "NicoMachineTemplate"
	tpmSourceForgeAPI       = "forge-api"
	tpmSourceNicoCR         = "nico-cr"
)

var (
	errTPMDataNotReady      = errors.New("TPM EK public key data is not ready")
	errTPMDataNotApplicable = errors.New("TPM EK public key reconciliation is not applicable")
)

// Controller is responsible for reconciling the TPM EK public key hash for nodes.
type Controller struct {
	cfg           config.Config
	db            *dbwrapper.DB
	logger        *slog.Logger
	forgeProvider nodeTPMForgeProvider
	k8sClient     client.Client
}

type nodeTPMForgeProvider interface {
	GetInstanceByID(ctx context.Context, orgID, siteID, instanceID string) (*forgeapi.Instance, error)
}

type tpmEKPubResult struct {
	hash   string
	source string
}

func NewController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance *dbwrapper.DB,
	forgeProvider *forge.ForgeProvider,
	k8sClient client.Client,
) *Controller {
	return &Controller{
		cfg:           cfg,
		db:            dbInstance,
		forgeProvider: forgeProvider,
		k8sClient:     k8sClient,
		logger:        logger,
	}
}

// SetupWithManager wires the controller into controller-runtime with DB-backed sources.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"node-tpm-ekpub-controller",
		c,
		dbwrapper.NewNodeSource(c.db, dbfilters.Node{
			TpmEKPubHash: ptr.To(""),
			Deleting:     ptr.To(false),
		}),
	)
}

// Reconcile reconciles the TPM EK public key hash for a single Node key.
func (c *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	node, err := c.db.GetNode(ctx, key.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			c.logger.Debug("Node not found, skipping TPM EK public key reconciliation", "nodeID", key.ID)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get node %s: %w", key.ID, err)
	}

	if node.TpmEKPubHash != "" {
		c.logger.Debug("Node already has TPM EK public key hash, skipping reconciliation", "nodeID", node.ID, "nodeName", node.Name)
		return reconcile.Result{}, nil
	}
	if node.Conditions.Deleting.Status == db.ConditionStatusTrue {
		c.logger.Debug("Node is deleting, skipping TPM EK public key hash reconciliation", "nodeID", node.ID, "nodeName", node.Name)
		return reconcile.Result{}, nil
	}

	cluster, err := c.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get cluster for node %s (%s): %w", node.ID, node.Name, err)
	}

	tpmResult, err := c.reconcileNodeEKPub(ctx, node, cluster)
	if errors.Is(err, errTPMDataNotReady) {
		c.logger.Debug("TPM EK public key data is not available yet, requeueing",
			"nodeID", node.ID,
			"nodeName", node.Name,
			"requeueAfter", nodeTPMEKPubRetryAfter.String(),
			"reason", err.Error(),
		)
		return reconcile.Result{RequeueAfter: nodeTPMEKPubRetryAfter}, nil
	}
	if errors.Is(err, errTPMDataNotApplicable) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	if tpmResult == nil {
		return reconcile.Result{}, fmt.Errorf("TPM EK public key reconciliation returned no result for node %s (%s)", node.ID, node.Name)
	}

	err = c.persistNodeTPMEKPubHash(ctx, node, tpmResult.hash, tpmResult.source)
	return reconcile.Result{}, err
}

// reconcileNodeEKPub routes a node to the provider-specific TPM EK public key
// reconciliation path.
func (c *Controller) reconcileNodeEKPub(ctx context.Context, node db.Node, cluster db.Cluster) (*tpmEKPubResult, error) {
	if cluster.ProviderType == db.ProviderTypeNico {
		return c.reconcileNicoProviderNodeEKPub(ctx, node, cluster)
	}
	if cluster.ProviderType == db.ProviderTypeCAPI {
		return c.reconcileCAPIProviderNodeEKPub(ctx, node, cluster)
	}

	return nil, errTPMDataNotApplicable
}

func (c *Controller) reconcileCAPIProviderNodeEKPub(ctx context.Context, node db.Node, cluster db.Cluster) (*tpmEKPubResult, error) {
	nodeSet, _, err := nodes.GetNodeContext(ctx, c.db, node)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve node context for TPM EK public key reconciliation: %w", err)
	}

	capiNodeSetProvider, err := nodeSet.CAPIProvider()
	if err != nil {
		return nil, fmt.Errorf("CAPI node %s (%s) has invalid CAPI provider payload: %w", node.ID, node.Name, err)
	}
	if capiNodeSetProvider.SpireAttestationMethod != db.SpireAttestationMethodTPM {
		c.logger.Debug("Skipping TPM EK public key hash reconciliation for non-TPM CAPI node",
			"nodeID", node.ID,
			"nodeName", node.Name,
			"spireAttestationMethod", capiNodeSetProvider.SpireAttestationMethod,
		)
		return nil, errTPMDataNotApplicable
	}

	isNicoTemplate, err := isNicoMachineTemplate(node, capiNodeSetProvider)
	if err != nil {
		return nil, err
	}
	if !isNicoTemplate {
		c.logger.Debug("Skipping TPM EK public key hash reconciliation for non-Nico CAPI infra provider",
			"nodeID", node.ID,
			"nodeName", node.Name,
		)
		return nil, errTPMDataNotApplicable
	}

	return c.reconcileNicoMachineEKPub(ctx, node, cluster)
}

func (c *Controller) reconcileNicoProviderNodeEKPub(ctx context.Context, node db.Node, cluster db.Cluster) (*tpmEKPubResult, error) {
	nicoNodeProvider, err := node.NicoProvider()
	if err != nil {
		return nil, err
	}
	if nicoNodeProvider.ForgeInstanceID != "" {
		return c.reconcileForgeNodeEKPub(ctx, node, cluster)
	}

	return c.reconcileNicoMachineEKPub(ctx, node, cluster)
}

func isNicoMachineTemplate(node db.Node, capiNodeSetProvider db.CAPINodePoolProvider) (bool, error) {
	if capi.IsEmptyManifest(capiNodeSetProvider.InfraMachineTemplate) {
		return false, fmt.Errorf("CAPI node %s (%s) has TPM attestation but empty infra machine template", node.ID, node.Name)
	}

	manifest, err := capi.ParseManifest(capiNodeSetProvider.InfraMachineTemplate)
	if err != nil {
		return false, fmt.Errorf("failed to parse CAPI infra machine template for node %s (%s): %w", node.ID, node.Name, err)
	}
	groupVersion, err := schema.ParseGroupVersion(manifest.GetAPIVersion())
	if err != nil {
		return false, fmt.Errorf("failed to parse CAPI infra machine template apiVersion %q for node %s (%s): %w", manifest.GetAPIVersion(), node.ID, node.Name, err)
	}

	return groupVersion.Group == nicov1.GroupVersion.Group && manifest.GetKind() == nicoMachineTemplateKind, nil
}

func (c *Controller) reconcileForgeNodeEKPub(ctx context.Context, node db.Node, cluster db.Cluster) (*tpmEKPubResult, error) {
	nicoClusterProvider, err := cluster.NicoProvider()
	if err != nil {
		return nil, err
	}
	if nicoClusterProvider.OrgID == "" {
		return nil, fmt.Errorf("nico cluster %s has no organization ID", cluster.ID)
	}
	nicoNodeProvider, err := node.NicoProvider()
	if err != nil {
		return nil, err
	}
	if nicoNodeProvider.ForgeInstanceID == "" {
		return nil, fmt.Errorf("%w: Forge instance ID is not set for node %s (%s)", errTPMDataNotReady, node.ID, node.Name)
	}
	if c.forgeProvider == nil {
		return nil, fmt.Errorf("forge provider is not configured for TPM EK public key reconciliation")
	}

	forgeInstance, err := c.forgeProvider.GetInstanceByID(ctx, nicoClusterProvider.OrgID, nicoClusterProvider.SiteID.String(), nicoNodeProvider.ForgeInstanceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get forge instance for node: %s (%s): %w", node.ID, node.Name, err)
	}

	if !isForgeInstanceAvailable(forgeInstance) {
		c.logger.Debug("Forge instance is not ready, TPM EK public key hash is not available yet",
			"nodeName", node.Name,
			"nodeID", node.ID,
			"forgeInstanceID", nicoNodeProvider.ForgeInstanceID,
			"forgeInstanceStatus", ptr.Deref(forgeInstance.Status, ""),
		)
		return nil, fmt.Errorf("%w: Forge instance is not ready for node %s (%s)", errTPMDataNotReady, node.ID, node.Name)
	}

	tpmEkCertS := forgeInstance.GetTpmEkCertificate()
	if tpmEkCertS == "" {
		return nil, fmt.Errorf("%w: Forge did not return a TPM EK certificate for ready instance: %s (%s)", errTPMDataNotReady, node.ID, node.Name)
	}

	tpmEKPubHash, err := tpm.ComputePublicKeyHash(tpmEkCertS)
	if err != nil {
		return nil, fmt.Errorf("failed to compute TPM public key hash for node: %s (%s): %w", node.ID, node.Name, err)
	}

	return &tpmEKPubResult{hash: tpmEKPubHash, source: tpmSourceForgeAPI}, nil
}

func (c *Controller) reconcileNicoMachineEKPub(ctx context.Context, node db.Node, _ db.Cluster) (*tpmEKPubResult, error) {
	nicoMachine := &nicov1.NicoMachine{}
	// buildNicoMachine names the NicoMachine CR after the database node name, so
	// node.Name is the corresponding NicoMachine lookup key.
	if err := c.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: constants.NamespaceNKESystem,
		Name:      node.Name,
	}, nicoMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: expected NicoMachine for node %s (%s)", errTPMDataNotReady, node.ID, node.Name)
		}
		return nil, fmt.Errorf("failed to get NicoMachine for node %s (%s): %w", node.ID, node.Name, err)
	}

	if !nicoMachine.DeletionTimestamp.IsZero() {
		c.logger.Debug("NicoMachine is being deleted, skipping TPM EK public key hash reconciliation",
			"nodeName", node.Name,
			"nodeID", node.ID,
		)
		return nil, fmt.Errorf("%w: NicoMachine is deleting for node %s (%s)", errTPMDataNotReady, node.ID, node.Name)
	}

	if !isNicoMachineAvailable(nicoMachine) {
		c.logger.Debug("NicoMachine is not ready, TPM EK public key hash is not available yet",
			"nodeName", node.Name,
			"nodeID", node.ID,
			"providerID", nicoMachine.Spec.ProviderID,
		)
		return nil, fmt.Errorf("%w: NicoMachine is not ready for node %s (%s)", errTPMDataNotReady, node.ID, node.Name)
	}

	tpmEKPubHash := nicoMachine.Status.TpmEkPubHash
	if tpmEKPubHash == "" {
		return nil, fmt.Errorf("%w: ready NicoMachine has no TPM EK public key hash for node: %s (%s)", errTPMDataNotReady, node.ID, node.Name)
	}
	if err := teleport.ValidateEKPubHash(tpmEKPubHash); err != nil {
		return nil, fmt.Errorf("invalid TPM EK public key hash on NicoMachine for node %s (%s): %w", node.ID, node.Name, err)
	}

	return &tpmEKPubResult{hash: tpmEKPubHash, source: tpmSourceNicoCR}, nil
}

func (c *Controller) persistNodeTPMEKPubHash(ctx context.Context, node db.Node, tpmEKPubHash, tpmSource string) error {
	err := c.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		lockedNode, err := tx.GetNodeForUpdate(ctx, node.ID)
		if err != nil {
			return err
		}
		if lockedNode.TpmEKPubHash != "" {
			c.logger.Debug("Node already has TPM EK public key hash after lock, skipping update",
				"nodeID", lockedNode.ID,
				"nodeName", lockedNode.Name,
			)
			return nil
		}
		if lockedNode.Conditions.Deleting.Status == db.ConditionStatusTrue {
			c.logger.Debug("Node is deleting after lock, skipping TPM EK public key hash update",
				"nodeID", lockedNode.ID,
				"nodeName", lockedNode.Name,
			)
			return nil
		}

		lockedNode.TpmEKPubHash = tpmEKPubHash
		_, err = tx.UpdateNode(ctx, &lockedNode)
		return err
	})
	if err != nil {
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
