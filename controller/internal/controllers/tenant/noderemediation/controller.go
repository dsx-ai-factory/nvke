// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderemediation

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	dbfilters "github.com/dsx-ai-factory/nke/controller/internal/controllers/dbreconcile/filters"
	"github.com/dsx-ai-factory/nke/controller/internal/controllers/tenant"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// Reconciler starts node remediation workflows from tenant Kubernetes Node labels.
type Reconciler struct {
	clusterID  uuid.UUID
	db         *dbwrapper.DB
	client     ctrlclient.Client
	remediator *nodeRemediator
}

// NewRegistrar returns a tenant-manager registrar for Kubernetes Node remediation.
func NewRegistrar(cfg config.Config, database *dbwrapper.DB, controllerTemporalClient workflowStarter, logger *slog.Logger) tenant.Registrar {
	if logger == nil {
		logger = slog.Default()
	}
	return func(clusterID uuid.UUID, manager ctrl.Manager) error {
		controllerName, controllerOptions := controllerRegistration(clusterID)
		reconciler := &Reconciler{
			clusterID:  clusterID,
			db:         database,
			client:     manager.GetClient(),
			remediator: newNodeRemediator(cfg, logger.With("controller", "tenant-node-remediation", "clusterID", clusterID), controllerTemporalClient),
		}

		return ctrl.NewControllerManagedBy(manager).
			Named(controllerName).
			WithOptions(controllerOptions).
			For(&corev1.Node{}, builder.WithPredicates(remediationNodePredicate())).
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

func remediationNodePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return nodeHasRemediationRequest(e.Object) },
		DeleteFunc: func(_ event.DeleteEvent) bool { return false },
		// Reconcile only when the requested remediation changes; workflow status
		// labels are written back to the Node and should not requeue this controller.
		UpdateFunc: func(e event.UpdateEvent) bool {
			return nodeRemediationRequestChanged(e.ObjectOld, e.ObjectNew) && nodeHasRemediationRequest(e.ObjectNew)
		},
		GenericFunc: func(e event.GenericEvent) bool { return nodeHasRemediationRequest(e.Object) },
	}
}

func nodeHasRemediationRequest(object ctrlclient.Object) bool {
	node, isNode := object.(*corev1.Node)
	if !isNode {
		return false
	}
	labels := node.GetLabels()
	if _, hasTerminateRequest := labels[constants.LabelKeyNodeLabelTerminate]; hasTerminateRequest {
		return true
	}
	if _, hasRebootRequest := labels[constants.LabelKeyNodeLabelReboot]; hasRebootRequest {
		return true
	}
	_, hasMachineHealthIssue := node.GetAnnotations()[constants.NodeAnnotationKeyMachineHealthIssue]
	return hasMachineHealthIssue
}

func nodeRemediationRequestChanged(oldObject, newObject ctrlclient.Object) bool {
	oldNode, ok := oldObject.(*corev1.Node)
	if !ok {
		return false
	}
	newNode, ok := newObject.(*corev1.Node)
	if !ok {
		return false
	}
	return oldNode.GetLabels()[constants.LabelKeyNodeLabelTerminate] != newNode.GetLabels()[constants.LabelKeyNodeLabelTerminate] ||
		oldNode.GetLabels()[constants.LabelKeyNodeLabelReboot] != newNode.GetLabels()[constants.LabelKeyNodeLabelReboot] ||
		oldNode.GetAnnotations()[constants.NodeAnnotationKeyMachineHealthIssue] != newNode.GetAnnotations()[constants.NodeAnnotationKeyMachineHealthIssue]
}

func controllerRegistration(clusterID uuid.UUID) (string, controller.Options) {
	return "tenant-node-remediation-" + clusterID.String(), controller.Options{
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
		return reconcile.Result{}, fmt.Errorf("listing controller node %s for cluster %s: %w", request.Name, r.clusterID, err)
	}
	if len(nodes) == 0 {
		return reconcile.Result{}, nil
	}
	dbNode := nodes[0]

	kubeNode := &corev1.Node{}
	err = r.client.Get(ctx, types.NamespacedName{Name: request.Name}, kubeNode)
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting tenant Kubernetes Node %s for cluster %s: %w", request.Name, r.clusterID, err)
	}

	if err := r.remediator.startWorkflowsIfNecessary(ctx, dbNode, kubeNode); err != nil {
		return reconcile.Result{}, fmt.Errorf("processing node remediation for node %s: %w", dbNode.ID, err)
	}

	return reconcile.Result{}, nil
}
