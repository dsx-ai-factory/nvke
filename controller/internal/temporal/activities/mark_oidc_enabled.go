// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/NVIDIA/nke/controller/internal/db"
)

// MarkOIDCEnabledInput identifies the cluster whose oidc_auth_enabled flag is
// latched to true once OIDC has been applied to the whole control plane. The
// cluster controller observes the row change and takes over the steady-state
// RBAC, kubeconfig, and trust-bundle reconcile.
type MarkOIDCEnabledInput struct {
	ClusterID uuid.UUID `json:"clusterID"`
}

// MarkOIDCEnabledResult is empty; the activity succeeds or errors.
type MarkOIDCEnabledResult struct{}

// MarkOIDCEnabled completes OIDC enablement: it latches oidc_auth_enabled to true
// and, in the same row-locked transaction (SELECT ... FOR UPDATE), releases the
// enablement lock and clears any prior UpdateFailed. Flipping the marker and
// releasing the lock in one write means a completed enablement can never be left
// both enabled and marked-failed. It is idempotent: with nothing left to change
// it is a genuine no-op (no write).
func (a *Activities) MarkOIDCEnabled(ctx context.Context, input MarkOIDCEnabledInput) (MarkOIDCEnabledResult, error) {
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, input.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		changed := false
		if !cluster.OIDCAuthEnabled {
			cluster.OIDCAuthEnabled = true
			changed = true
		}
		if clearOIDCUpdateInProgress(&cluster) {
			changed = true
		}
		if cluster.Conditions.UpdateFailed != nil {
			db.ClearUpdateFailed(&cluster)
			changed = true
		}
		if !changed {
			return nil
		}

		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("failed to mark oidc_auth_enabled for cluster %s: %w", input.ClusterID, err)
		}
		return nil
	})
	if err != nil {
		return MarkOIDCEnabledResult{}, err
	}
	return MarkOIDCEnabledResult{}, nil
}

// ClearOIDCUpdateInProgressInput identifies the cluster whose oidc_enable
// UpdateInProgress lock (taken by the UpdateCluster trigger) is to be released.
// A non-empty FailureMessage records the failure on the cluster; an empty one
// clears any prior UpdateFailed, marking a clean completion.
type ClearOIDCUpdateInProgressInput struct {
	ClusterID      uuid.UUID
	FailureMessage string
}

// ClearOIDCUpdateInProgressResult is empty; the activity succeeds or errors.
type ClearOIDCUpdateInProgressResult struct{}

// ClearOIDCUpdateInProgress releases the oidc_enable UpdateInProgress lock the
// UpdateCluster trigger took and either records a failure (FailureMessage set) or
// clears UpdateFailed on success. It only touches the lock when it is the OIDC
// lock, so a concurrent operation's lock is never cleared, and it is a no-op when
// no OIDC lock is present (e.g. a workflow started directly, without the API
// trigger, as the e2e does).
func (a *Activities) ClearOIDCUpdateInProgress(ctx context.Context, input ClearOIDCUpdateInProgressInput) (ClearOIDCUpdateInProgressResult, error) {
	err := a.db.RunInTx(ctx, func(ctx context.Context, tx db.Tx) error {
		cluster, err := tx.GetClusterForUpdate(ctx, input.ClusterID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return temporal.NewNonRetryableApplicationError("cluster not found in database", "db.ErrNotFound", err)
			}
			return fmt.Errorf("failed to get cluster: %w", err)
		}

		if !clearOIDCUpdateInProgress(&cluster) {
			return nil
		}
		if input.FailureMessage != "" {
			db.SetUpdateFailed(&cluster, input.FailureMessage)
		} else {
			db.ClearUpdateFailed(&cluster)
		}

		if _, err := tx.UpdateCluster(ctx, &cluster); err != nil {
			return fmt.Errorf("failed to clear oidc update progress: %w", err)
		}
		return nil
	})
	if err != nil {
		return ClearOIDCUpdateInProgressResult{}, err
	}
	return ClearOIDCUpdateInProgressResult{}, nil
}

// clearOIDCUpdateInProgress clears the in-flight lock only when it is the OIDC
// enablement lock, returning whether it changed the cluster.
func clearOIDCUpdateInProgress(cluster *db.Cluster) bool {
	if cluster.Conditions.UpdateInProgress == nil ||
		cluster.Conditions.UpdateInProgress.Type != db.OperationTypeOIDCEnable {
		return false
	}
	db.ClearUpdateInProgress(cluster)
	return true
}
