// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	rolloutkubevip "github.com/dsx-ai-factory/nvke/controller/internal/rollout/guards/kubevip"
	rolloutoidc "github.com/dsx-ai-factory/nvke/controller/internal/rollout/operations/oidc"
	rolloutpreflights "github.com/dsx-ai-factory/nvke/controller/internal/rollout/preflights"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

// EnableOIDCInput is the brownfield OIDC migration input. ClusterID is the controller
// cluster UUID.
type EnableOIDCInput struct {
	ClusterID uuid.UUID `json:"clusterID"`
}

// EnableOIDC enables kube-apiserver OIDC authentication on an existing cluster's
// control plane and marks the cluster row so the cluster controller takes over
// the steady-state RBAC, kubeconfig, and trust-bundle reconcile. New clusters get
// OIDC from kubeadm init (the create path), not this workflow.
const (
	oidcFailureMessage = "OIDC enablement failed"
)

func EnableOIDC(ctx workflow.Context, input EnableOIDCInput) error {
	logger := workflow.GetLogger(ctx)

	if err := enableOIDC(ctx, input); err != nil {
		// On unrecoverable failure the rollout has already restored or cascade-rolled
		// back every touched node (RollbackHaltOnAnyFailure), so there is no partial
		// on-node state to unwind here and the marker is never flipped. Recovery is by
		// re-running the workflow: the operation and both write-back activities are
		// idempotent. Release the oidc_enable lock the UpdateCluster trigger took (on a
		// disconnected context so it still runs if the workflow is being canceled) and
		// record the failure, so UpdateCluster is not wedged at 409. A workflow started
		// directly (no trigger, no lock) clears nothing.
		if input.ClusterID != uuid.Nil {
			cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
			if clearErr := clearOIDCUpdateInProgress(cleanupCtx, input.ClusterID, oidcFailureMessage); clearErr != nil {
				logger.Error("failed to clear oidc update progress after workflow failure", "error", clearErr)
			}
		}
		return fmt.Errorf("failed to enable oidc: %w", err)
	}
	return nil
}

func enableOIDC(ctx workflow.Context, input EnableOIDCInput) error {
	if input.ClusterID == uuid.Nil {
		return errors.New("clusterID is required")
	}
	logger := workflow.GetLogger(ctx)

	cluster, err := rollouttemporal.LoadClusterForWorkflow(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to load cluster for oidc rollout: %w", err)
	}

	// 1. Roll the auth-config onto each control-plane node, one at a time, behind
	//    the rollout's safety floor and cascade-rollback (RollbackHaltOnAnyFailure):
	//    a partial control plane is never left behind. A FailedNotRestored or any
	//    other non-Completed outcome surfaces here as an error and HALTS before the
	//    write-back, so the marker is never flipped on a partial control plane.
	logger.Info("starting brownfield OIDC rollout", "clusterID", input.ClusterID.String())
	if err := executeOIDCRolloutPhase(ctx, cluster, withKubeVipNodeGuard()); err != nil {
		return err
	}

	// 2. Persist the apiserver settings into kube-system/kubeadm-config so a later
	//    kubeadm upgrade re-render does not strip OIDC. Idempotent.
	var a *activities.Activities
	if err := workflow.ExecuteActivity(
		withOIDCWriteBackOptions(ctx),
		a.MergeOIDCKubeadmConfig,
		activities.MergeOIDCKubeadmConfigInput{ClusterID: input.ClusterID},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to merge oidc into kubeadm-config: %w", err)
	}

	// 3. Complete: flip the marker and release the enablement lock in one atomic
	//    write. The cluster controller observes the row change and applies the RBAC
	//    and publishes the SPIRE-JWT kubeconfig + trust bundle. Idempotent: a genuine
	//    no-op if the flag is already set and the lock already released.
	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.MarkOIDCEnabled,
		activities.MarkOIDCEnabledInput{ClusterID: input.ClusterID},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to mark oidc enabled: %w", err)
	}

	return nil
}

// clearOIDCUpdateInProgress releases the oidc_enable UpdateInProgress lock via the
// ClearOIDCUpdateInProgress activity. An empty failureMessage marks a clean
// completion (clears UpdateFailed); a non-empty one records the failure.
func clearOIDCUpdateInProgress(ctx workflow.Context, clusterID uuid.UUID, failureMessage string) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.ClearOIDCUpdateInProgress, activities.ClearOIDCUpdateInProgressInput{
		ClusterID:      clusterID,
		FailureMessage: failureMessage,
	}).Get(ctx, nil)
}

func executeOIDCRolloutPhase(
	ctx workflow.Context,
	cluster rollout.Cluster,
	opts ...etcdRolloutPhaseOption,
) error {
	options := etcdRolloutPhaseOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	result, err := rollouttemporal.Execute(
		rollouttemporal.WithRolloutNodeAgentApplyOptions(ctx),
		cluster,
		rollout.Compile(rolloutoidc.NewEnableOIDCOperation()),
		oidcRolloutSpec(len(cluster.ControlPlaneNodes()), options),
	)
	if err != nil {
		return fmt.Errorf("%s rollout executor failed: %w", rolloutoidc.OperationName, err)
	}
	if err := result.Err(); err != nil {
		return fmt.Errorf("%s rollout failed: %w", rolloutoidc.OperationName, err)
	}
	return nil
}

// oidcRolloutSpec is serial, one auth-config at a time, cascade-rolling-back on
// ANY node failure (not only unrecoverable ones) so a half-migrated control plane
// behind the shared VIP is never left behind. The per-node Enable/Disable RPC
// capability is gated in the operation's Validate via gRPC reflection, so no
// agent version preflight is added.
func oidcRolloutSpec(controlPlaneCount int, options etcdRolloutPhaseOptions) rollout.RolloutSpec {
	spec := rollout.RolloutSpec{
		Strategy:           rollout.StrategySerial,
		RollbackPolicy:     rollout.RollbackHaltOnAnyFailure,
		MinHealthyReplicas: rollout.MinHealthyReplicasPolicy{MinHealthy: controlPlaneCount},
		ClusterPreflights:  rolloutpreflights.DefaultClusterPreflights(),
		NodePreflights: []rollout.NodePreflightRef{
			{Name: rolloutpreflights.PreflightNameNodeAgentReady},
		},
	}
	if options.withKubeVipNodeGuard {
		spec.NodeGuards = []rollout.NodeGuardRef{{Name: rolloutkubevip.GuardNameKubeVipDrainRestore}}
	}
	return spec
}

// withOIDCWriteBackOptions sizes the kubeadm-config write-back activity: it
// dials a control-plane agent for the tenant admin.conf and talks to the
// tenant apiserver, so it needs more than the fast DB budget.
func withOIDCWriteBackOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    10 * time.Second,
			MaximumInterval:    time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    3,
		},
	})
}
