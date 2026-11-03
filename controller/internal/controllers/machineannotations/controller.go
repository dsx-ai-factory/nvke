// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package machineannotations copies controller-owned annotations from a node's
// database record onto its Cluster API Machine.
package machineannotations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

type nodeReader interface {
	GetNode(ctx context.Context, id uuid.UUID) (db.Node, error)
}

// Controller sets the agent-connection, spire-attestation-method and
// spiffe-enabled annotations on Machines created before the controller wrote
// them at creation, so controllers that read only Kubernetes objects can take
// a node's agent transport and SPIRE attestation from its Machine.
type Controller struct {
	cfg       config.Config
	db        *dbwrapper.DB
	nodes     nodeReader
	k8sClient client.Client
	logger    *slog.Logger
}

// NewController returns a Controller that reads nodes from dbInstance and
// patches their Machines through k8sClient.
func NewController(cfg config.Config, logger *slog.Logger, dbInstance *dbwrapper.DB, k8sClient client.Client) *Controller {
	return &Controller{
		cfg:       cfg,
		db:        dbInstance,
		nodes:     dbInstance,
		k8sClient: k8sClient,
		logger:    logger,
	}
}

// SetupWithManager registers the controller, reconciling every node that is
// not being deleted.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	return dbreconcile.SetupController(
		mgr,
		"machine-annotations",
		c,
		dbwrapper.NewNodeSource(c.db, dbfilters.Node{Deleting: ptr.To(false)}),
	)
}

// Reconcile sets each owned annotation on the node's Machine that is missing
// or stale, in one patch. It skips a Machine whose host-id does not name the
// node, which includes Machines created before the controller wrote host-id.
func (c *Controller) Reconcile(ctx context.Context, key dbreconcile.Key) (reconcile.Result, error) {
	node, err := c.nodes.GetNode(ctx, key.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("get node %s: %w", key.ID, err)
	}

	machine := &clusterv1.Machine{}
	machineKey := types.NamespacedName{Namespace: c.cfg.ManagementNamespaceOrDefault(), Name: node.Name}
	if err := c.k8sClient.Get(ctx, machineKey, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("get machine %s: %w", machineKey, err)
	}
	if !machine.DeletionTimestamp.IsZero() || keys.MachineHostID.Value(machine.Annotations) != node.ID.String() {
		return reconcile.Result{}, nil
	}

	desired := shared.MachineAnnotations(node)
	patch := client.MergeFrom(machine.DeepCopy())
	var changed []string
	for _, owned := range ownedKeys() {
		want := desired[owned.String()]
		if got, ok := owned.Get(machine.Annotations); ok && got == want {
			continue
		}
		owned.DeleteLegacy(machine.Annotations)
		machine.Annotations[owned.String()] = want
		changed = append(changed, owned.String())
	}
	if len(changed) == 0 {
		return reconcile.Result{}, nil
	}
	if err := c.k8sClient.Patch(ctx, machine, patch); err != nil {
		return reconcile.Result{}, fmt.Errorf("set annotations %v on machine %s: %w", changed, machineKey, err)
	}
	c.logger.Info("Set controller-owned annotations on Machine", "nodeID", node.ID, "machine", machineKey, "annotations", changed)
	return reconcile.Result{}, nil
}

// ownedKeys returns the Machine annotations this controller keeps in step
// with the node record.
func ownedKeys() []keys.Key {
	return []keys.Key{keys.MachineAgentConnection, keys.MachineSpireAttestationMethod, keys.MachineSpiffeEnabled}
}
