// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeclusterstaticentry

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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/constants"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/NVIDIA/nke/controller/internal/db"
)

const (
	clusterStaticEntryKind = "ClusterStaticEntry"
	unixRootSelector       = "unix:uid:0"
)

// Controller is responsible for reconciling Node TPM ClusterStaticEntries.
type Controller struct {
	cfg         config.Config
	db          *dbwrapper.DB
	k8sClient   client.Client
	logger      *slog.Logger
	trustDomain string
}

func NewController(
	cfg config.Config,
	dbInstance *dbwrapper.DB,
	k8sClient client.Client,
	logger *slog.Logger,
	trustDomain string,
) *Controller {
	return &Controller{
		cfg:         cfg,
		db:          dbInstance,
		k8sClient:   k8sClient,
		logger:      logger,
		trustDomain: trustDomain,
	}
}

// SetupWithManager wires the controller into controller-runtime with DB-backed and
// ClusterStaticEntry-backed sources.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"node-cluster-static-entry-controller",
		c,
		dbwrapper.NewNodeSource(c.db, dbfilters.Node{}),
		source.TypedKind(
			mgr.GetCache(),
			&spirev1alpha1.ClusterStaticEntry{},
			handler.TypedEnqueueRequestsFromMapFunc(c.keysForClusterStaticEntry),
		),
	)
}

func (c *Controller) keysForClusterStaticEntry(_ context.Context, entry *spirev1alpha1.ClusterStaticEntry) []dbreconcile.Key {
	nodeID, ok := nodeIDFromClusterStaticEntryName(entry.GetName())
	if !ok {
		return nil
	}
	if !c.ownsTPMClusterStaticEntry(entry, nodeID) {
		return nil
	}
	return []dbreconcile.Key{dbreconcile.NodeKey(nodeID)}
}

func nodeIDFromClusterStaticEntryName(name string) (uuid.UUID, bool) {
	idText, ok := strings.CutPrefix(name, "host-")
	if !ok {
		return uuid.Nil, false
	}
	nodeID, err := uuid.Parse(idText)
	if err != nil {
		return uuid.Nil, false
	}
	return nodeID, true
}

// Reconcile ensures the ClusterStaticEntry for a single node key matches the node state.
func (c *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	node, err := c.db.GetNode(ctx, key.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// if the node was already deleted, then delete the clusterStaticEntry for it.
			return reconcile.Result{}, c.deleteOwnedClusterStaticEntry(ctx, key.ID)
		}
		return reconcile.Result{}, fmt.Errorf("failed to get node %s: %w", key.ID, err)
	}

	if node.TpmEKPubHash == "" {
		c.logger.Debug("Node has no TPM EK public key hash, deleting cluster static entry if present",
			"nodeID", node.ID, "nodeName", node.Name)
		return reconcile.Result{}, c.deleteOwnedClusterStaticEntry(ctx, node.ID)
	}

	cluster, err := c.db.GetCluster(ctx, node.ClusterID)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get cluster for node %s (%s): %w", node.ID, node.Name, err)
	}

	skip, err := c.shouldSkipTPMClusterStaticEntry(ctx, node, cluster)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to resolve SPIRE attestation ownership for node %s (%s): %w", node.ID, node.Name, err)
	}
	if skip {
		c.logger.Debug("Skipping node TPM cluster static entry for non-TPM CAPI machine",
			"nodeID", node.ID, "nodeName", node.Name)
		return reconcile.Result{}, c.deleteOwnedClusterStaticEntry(ctx, node.ID)
	}

	if err := c.applyClusterStaticEntry(ctx, clusterStaticEntryName(node.ID), node); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (c *Controller) shouldSkipTPMClusterStaticEntry(ctx context.Context, node db.Node, cluster db.Cluster) (bool, error) {
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

func clusterStaticEntryName(nodeID uuid.UUID) string {
	return fmt.Sprintf("host-%s", nodeID.String())
}

func (c *Controller) deleteOwnedClusterStaticEntry(ctx context.Context, nodeID uuid.UUID) error {
	name := clusterStaticEntryName(nodeID)
	cse := &spirev1alpha1.ClusterStaticEntry{}
	if err := c.k8sClient.Get(ctx, client.ObjectKey{Name: name}, cse); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get cluster static entry %s: %w", name, err)
	}
	if !c.ownsTPMClusterStaticEntry(cse, nodeID) {
		c.logger.Debug("Skipping deletion of cluster static entry not owned by node TPM reconciler",
			"name", name, "nodeID", nodeID)
		return nil
	}
	if err := c.k8sClient.Delete(ctx, cse); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete cluster static entry %s: %w", name, err)
	}
	c.logger.Info("Deleted cluster static entry", "name", name, "nodeID", nodeID)
	return nil
}

func (c *Controller) ownsTPMClusterStaticEntry(entry *spirev1alpha1.ClusterStaticEntry, nodeID uuid.UUID) bool {
	if entry.GetLabels()[constants.LabelKeyNkeManagedBy] != constants.LabelValueNkeManagedBy {
		return false
	}
	if entry.Spec.ClassName != c.cfg.SpireClassName {
		return false
	}
	if !strings.HasPrefix(entry.Spec.ParentID, fmt.Sprintf("spiffe://%s%s", c.trustDomain, constants.TPMSpireAgentPath)) {
		return false
	}
	return entry.Spec.SPIFFEID == fmt.Sprintf("spiffe://%s/host/%s", c.trustDomain, nodeID.String())
}

// This uses the server-side apply API, so it will create if not exists, update if different.
// We don't do any client-side diffing or checking before applying.
// See: https://kubernetes.io/blog/2022/10/20/advanced-server-side-apply/#reconstructive-controllers
func (c *Controller) applyClusterStaticEntry(ctx context.Context, name string, node db.Node) error {
	cse := &spirev1alpha1.ClusterStaticEntry{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spirev1alpha1.SchemeGroupVersion.String(),
			Kind:       clusterStaticEntryKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				constants.LabelKeyNkeManagedBy: constants.LabelValueNkeManagedBy,
			},
		},
		Spec: spirev1alpha1.ClusterStaticEntrySpec{
			ClassName: c.cfg.SpireClassName,
			ParentID:  node.GetTPMSpiffeID(c.trustDomain), // e.g. spiffe://example.org/spire/agent/tpm/<pubhash>
			Selectors: []string{unixRootSelector},
			SPIFFEID:  node.GetSpiffeID(c.trustDomain), // e.g. spiffe://example.org/host/<node-id>
		},
	}

	c.logger.Debug("Applying cluster static entry", "name", name, "desiredSpec", cse.Spec)
	cseMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cse)
	if err != nil {
		return fmt.Errorf("failed to convert cluster static entry to unstructured: %s: %w", name, err)
	}
	cseUnstructured := &unstructured.Unstructured{Object: cseMap}

	err = c.k8sClient.Apply(
		ctx,
		client.ApplyConfigurationFromUnstructured(cseUnstructured),
		client.FieldOwner(constants.FieldManagerCWE),
		client.ForceOwnership,
	)
	if err != nil {
		return fmt.Errorf("failed to apply cluster static entry: %s: %w", name, err)
	}

	return nil
}
