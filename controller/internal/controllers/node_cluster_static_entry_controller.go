// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/db"
)

const (
	clusterStaticEntryKind = "ClusterStaticEntry"
	unixRootSelector       = "unix:uid:0"
)

// NodeClusterStaticEntryController is responsible for reconciling Node TPM ClusterStaticEntries.
type NodeClusterStaticEntryController struct {
	cfg         config.Config
	db          db.DB
	k8sClient   client.Client
	logger      *slog.Logger
	trustDomain string
}

func NewClusterStaticEntryController(
	cfg config.Config,
	dbInstance db.DB,
	k8sClient client.Client,
	logger *slog.Logger,
	trustDomain string,
) *NodeClusterStaticEntryController {
	return &NodeClusterStaticEntryController{
		cfg:         cfg,
		db:          dbInstance,
		k8sClient:   k8sClient,
		logger:      logger,
		trustDomain: trustDomain,
	}
}

// Reconcile ClusterStaticEntries, for each node in the site:
// - If the node has a TPM EK public key hash
//   - Apply (create or update) the corresponding ClusterStaticEntry.
//
// - Delete ClusterStaticEntries for any nodes that no longer exist or have no TPM EK public key hash set.
//   - Note: If a node's TPM EK public key hash is unset, the corresponding ClusterStaticEntry is deleted.
func (c *NodeClusterStaticEntryController) Reconcile(ctx context.Context, site db.Site) error {
	if err := c.reconcileClusterStaticEntries(ctx, site.ID); err != nil {
		return fmt.Errorf("failed to reconcile cluster static entries: %w", err)
	}

	return nil
}

func (c *NodeClusterStaticEntryController) reconcileClusterStaticEntries(ctx context.Context, siteID uuid.UUID) error {
	nodes, err := c.db.ListNodes(ctx, db.NodeFilters{SiteID: &siteID})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	clusterStaticEntries := &spirev1alpha1.ClusterStaticEntryList{}
	err = c.k8sClient.List(ctx, clusterStaticEntries, client.MatchingLabels{
		constants.LabelKeyNkeManagedBy: constants.LabelValueNkeManagedBy,
		constants.LabelKeySiteID:       siteID.String(),
	})
	if err != nil {
		return fmt.Errorf("failed to list cluster static entries: %w", err)
	}

	var errs []error
	desiredEntryNames := make(map[string]struct{}, len(nodes))

	// Ensure that we have a ClusterStaticEntry for each node.
	for _, node := range nodes {
		name := fmt.Sprintf("host-%s", node.ID.String())
		if node.TpmEKPubHash == "" {
			// Note this will also result in the ClusterStaticEntry being deleted if it existed previously.
			c.logger.Debug("Node has no TPM EK public key hash", "nodeID", node.ID, "nodeName", node.Name)
			continue
		}

		skip, err := c.shouldSkipTPMClusterStaticEntry(ctx, node)
		if err != nil {
			desiredEntryNames[name] = struct{}{}
			errs = append(errs, fmt.Errorf("failed to resolve SPIRE attestation ownership for node %s (%s): %w", node.ID, node.Name, err))
			continue
		}
		if skip {
			c.logger.Debug("Skipping node TPM cluster static entry for non-TPM CAPI machine",
				"nodeID", node.ID, "nodeName", node.Name)
			continue
		}

		desiredEntryNames[name] = struct{}{}
		err = c.applyClusterStaticEntry(ctx, name, siteID, node)
		if err != nil {
			// Collect errors but continue reconciling the other nodes.
			errs = append(errs, fmt.Errorf("failed to apply cluster static entry: %s: %w", name, err))
			continue
		}
	}

	// Delete any ClusterStaticEntries for nodes that no longer exist, or that have no longer have a TPM EKPub hash set.
	for _, clusterStaticEntry := range clusterStaticEntries.Items {
		if _, exists := desiredEntryNames[clusterStaticEntry.GetName()]; !exists {
			c.logger.Info("Deleting cluster static entry", "name", clusterStaticEntry.GetName())
			if err := c.k8sClient.Delete(ctx, &clusterStaticEntry); err != nil {
				// Collect errors but continue deleting the other cluster static entries.
				errs = append(errs, err)
				continue
			}
		}
	}

	return errors.Join(errs...)
}

func (c *NodeClusterStaticEntryController) shouldSkipTPMClusterStaticEntry(ctx context.Context, node db.Node) (bool, error) {
	cluster, err := c.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return false, err
	}
	if cluster.ProviderType != db.ProviderTypeCAPI {
		return false, nil
	}

	machine := &clusterv1.Machine{}
	if err := c.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: constants.NamespaceNKESystem,
		Name:      node.Name,
	}, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to find CAPI machine for node %s", node.Name)
		}
		return false, err
	}

	spireAgentIDPath := machine.GetAnnotations()[constants.MachineAnnotationKeySpireAgentSpiffeIDPath]
	return spireAgentIDPath != "" && !strings.HasPrefix(spireAgentIDPath, constants.TPMSpireAgentPath), nil
}

// This uses the server-side apply API, so it will create if not exists, update if different.
// We don't do any client-side diffing or checking before applying.
// See: https://kubernetes.io/blog/2022/10/20/advanced-server-side-apply/#reconstructive-controllers
func (c *NodeClusterStaticEntryController) applyClusterStaticEntry(ctx context.Context, name string, siteID uuid.UUID, node db.Node) error {
	cse := &spirev1alpha1.ClusterStaticEntry{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spirev1alpha1.SchemeGroupVersion.String(),
			Kind:       clusterStaticEntryKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				constants.LabelKeyNkeManagedBy: constants.LabelValueNkeManagedBy,
				constants.LabelKeySiteID:       siteID.String(),
			},
		},
		Spec: spirev1alpha1.ClusterStaticEntrySpec{
			ClassName: c.cfg.SpireClassName,
			ParentID:  node.GetTPMSpiffeID(c.trustDomain), // e.g. spiffe://nke-site-forge-az24-dev7.nke.nvidia.com/spire/agent/tpm/<pubhash>
			Selectors: []string{unixRootSelector},
			SPIFFEID:  node.GetSpiffeID(c.trustDomain), // e.g. spiffe://nke-site-forge-az24-dev7.nke.nvidia.com/host/<node-id>
		},
	}

	c.logger.Debug("Applying cluster static entry", "name", name, "desiredSpec", cse.Spec)
	err := c.k8sClient.Patch(
		ctx,
		cse,
		client.Apply,
		client.FieldOwner(constants.FieldManagerCWE),
		client.ForceOwnership,
	)
	if err != nil {
		return fmt.Errorf("failed to apply cluster static entry: %s: %w", name, err)
	}

	return nil
}
