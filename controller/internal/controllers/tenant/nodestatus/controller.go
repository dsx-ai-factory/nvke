// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodestatus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/tenant"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// Reconciler reconciles tenant Kubernetes Node status into CWE nodes.
type Reconciler struct {
	clusterID uuid.UUID
	db        *dbwrapper.DB
	client    ctrlclient.Client
	logger    *slog.Logger
}

// NewRegistrar returns a tenant-manager registrar for Kubernetes Node health.
func NewRegistrar(database *dbwrapper.DB, logger *slog.Logger) tenant.Registrar {
	if logger == nil {
		logger = slog.Default()
	}
	return func(clusterID uuid.UUID, manager ctrl.Manager) error {
		controllerName, controllerOptions := controllerRegistration(clusterID)
		reconciler := &Reconciler{
			clusterID: clusterID,
			db:        database,
			client:    manager.GetClient(),
			logger:    logger.With("clusterID", clusterID),
		}

		return ctrl.NewControllerManagedBy(manager).
			Named(controllerName).
			WithOptions(controllerOptions).
			For(&corev1.Node{}).
			WatchesRawSource(dbwrapper.NewMappedNodeSource(
				database,
				dbfilters.Node{
					ClusterID: &clusterID,
					Deleting:  ptr.To(false),
				},
				nodeRequestsForDBNode,
			)).
			Complete(reconciler)
	}
}

func controllerRegistration(clusterID uuid.UUID) (string, controller.Options) {
	return "tenant-node-health-" + clusterID.String(), controller.Options{
		SkipNameValidation: ptr.To(true),
	}
}

func nodeRequestsForDBNode(_ context.Context, node db.Node) ([]reconcile.Request, error) {
	if node.Name == "" {
		return nil, nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Name: node.Name,
		},
	}}, nil
}

func (r *Reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	nodes, err := r.db.ListNodes(ctx, db.NodeFilters{ClusterID: &r.clusterID, Name: &request.Name})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("listing CWE node %s for cluster %s: %w", request.Name, r.clusterID, err)
	}
	if len(nodes) == 0 {
		return reconcile.Result{}, nil
	}
	dbNode := nodes[0]

	kubeNode := &corev1.Node{}
	err = r.client.Get(ctx, types.NamespacedName{Name: request.Name}, kubeNode)
	if apierrors.IsNotFound(err) {
		kubeNode = nil
	} else if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting tenant Kubernetes Node %s for cluster %s: %w", request.Name, r.clusterID, err)
	}

	if err := r.updateKubernetesHealth(ctx, dbNode.ID, kubeNode); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *Reconciler) updateKubernetesHealth(ctx context.Context, nodeID uuid.UUID, kubeNode *corev1.Node) error {
	err := r.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		lockedNode, err := tx.GetNodeForUpdate(ctx, nodeID)
		if err != nil {
			return err
		}
		if lockedNode.Conditions.Deleting.Status == db.ConditionStatusTrue {
			return nil
		}

		previousConditions := lockedNode.Conditions
		applyKubernetesNodeHealth(&lockedNode, kubeNode)
		if lockedNode.Conditions.Equal(previousConditions) {
			r.logger.Debug("Kubernetes node health unchanged, skipping update",
				"nodeID", nodeID, "nodeName", lockedNode.Name)
			return nil
		}

		r.logger.Info("Kubernetes node health changed, updating node",
			"nodeID", nodeID,
			"nodeName", lockedNode.Name,
			"changes", describeConditionChanges(previousConditions, lockedNode.Conditions),
		)
		r.logger.Debug("Kubernetes node health change detail",
			"nodeID", nodeID,
			"nodeName", lockedNode.Name,
			"previousConditions", previousConditions,
			"newConditions", lockedNode.Conditions,
		)
		_, err = tx.UpdateNode(ctx, &lockedNode)
		return err
	})
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating Kubernetes health for node %s: %w", nodeID, err)
	}
	return nil
}
