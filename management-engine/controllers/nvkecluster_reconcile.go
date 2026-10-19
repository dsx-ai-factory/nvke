// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cluster reconciliation observes existing resources and coordinates creation and recovery.

package controllers

import (
	"context"
	"errors"
	"net/http"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/dsx-ai-factory/nvke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/nvkecontroller"
)

var errCreateRejected = errors.New("cluster creation request was rejected")

func (r *NVKEClusterReconciler) reconcileCluster(ctx context.Context, cluster *internalv1.NVKECluster) (ctrl.Result, error) {
	observedCluster, err := r.ensureCluster(ctx, cluster)
	if err != nil {
		return r.lifecycleError(cluster, err)
	}
	setClusterStatus(cluster, observedCluster)
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

func (r *NVKEClusterReconciler) ensureCluster(ctx context.Context, cluster *internalv1.NVKECluster) (*api.Cluster, error) {
	// Recover existing work before resolving a spec that may have changed since creation.
	observedCluster, err := r.findCluster(ctx, cluster)
	if !errors.Is(err, nvkecontroller.ErrNotFound) {
		return observedCluster, err
	}
	state := cluster.Status.Provisioning
	newAttempt := state == nil
	if state != nil {
		// A deleted cluster must not be silently replaced by a new one at the same name.
		if state.ID != "" {
			return nil, err
		}
		if state.Generation != cluster.Generation {
			return nil, &InvalidSpecError{Message: "Changes to an existing creation request are not supported"}
		}
	}

	request, err := ResolveControllerRequest(cluster, r.Bindings)
	if err != nil {
		return nil, err
	}
	var checkpoint *internalv1.NVKECluster
	if newAttempt {
		state = &internalv1.ClusterProvisioningStatus{Endpoint: r.NVKEController.Endpoint(), Generation: cluster.Generation}
		// Checkpoint before POST: a restart can recover by name without claiming a newer spec was accepted.
		checkpoint = cluster.DeepCopy()
		checkpoint.Status.Provisioning = state
		if err := r.Status().Update(ctx, checkpoint); err != nil {
			return nil, err
		}
		cluster.Status.Provisioning = state
	}

	observedCluster, err = r.NVKEController.Create(ctx, request)
	if response, ok := errors.AsType[*nvkecontroller.HTTPError](err); ok && response.StatusCode == http.StatusBadRequest {
		// A duplicate-name response can follow a successful POST whose response was lost.
		observedCluster, err = r.findCluster(ctx, cluster)
		if errors.Is(err, nvkecontroller.ErrNotFound) {
			// A rejected request cannot cancel an earlier attempt whose outcome is still unknown.
			if newAttempt {
				// The deferred patch predates the checkpoint, so clear it explicitly with its resource version.
				checkpoint.Status.Provisioning = nil
				if err := r.Status().Update(ctx, checkpoint); err != nil {
					return nil, err
				}
				cluster.Status.Provisioning = nil
			}
			return nil, errCreateRejected
		}
	}
	if err != nil {
		return nil, err
	}
	cluster.Status.Provisioning.ID = observedCluster.ID.String()
	return observedCluster, nil
}

// Lookup also recovers a successful create whose response or status write was lost.
func (r *NVKEClusterReconciler) findCluster(ctx context.Context, cluster *internalv1.NVKECluster) (*api.Cluster, error) {
	state := cluster.Status.Provisioning
	if state != nil && state.Endpoint != r.NVKEController.Endpoint() {
		return nil, &InvalidSpecError{Message: "Cluster provisioning service has changed; restore the original configuration"}
	}
	var observedCluster *api.Cluster
	var err error
	if state != nil && state.ID != "" {
		observedCluster, err = r.NVKEController.Get(ctx, state.ID)
	} else {
		observedCluster, err = r.NVKEController.FindByName(ctx, clusterRemoteName(cluster))
	}
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, &InvalidSpecError{Message: "Cluster creation conflicts with an existing cluster"}
	}
	if observedCluster.Name != clusterRemoteName(cluster) {
		return nil, &InvalidSpecError{Message: "Cluster identity does not match the creation request"}
	}
	state.ID = observedCluster.ID.String()
	return observedCluster, nil
}

func clusterRemoteName(cluster *internalv1.NVKECluster) string { return "nke-" + string(cluster.UID) }
