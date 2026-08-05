// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package workflows

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	rolloutkubevip "github.com/NVIDIA/nke/controller/internal/rollout/guards/kubevip"
	rolloutetcd "github.com/NVIDIA/nke/controller/internal/rollout/operations/etcd"
	rolloutpreflights "github.com/NVIDIA/nke/controller/internal/rollout/preflights"
	rollouttemporal "github.com/NVIDIA/nke/controller/internal/rollout/temporal"
	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

const (
	etcdEncryptionFailureMessage = "Etcd encryption enablement failed"

	etcdEncryptionConfigRunbook       = "kubeadm-config-api-encryption"
	etcdEncryptionValidateOnlyEnv     = "KUBEADM_CONFIG_API_ENCRYPTION_VALIDATE_ONLY"
	minEtcdEncryptionNodeAgentVersion = "v0.1.26"

	etcdRewriteInitialPollInterval = 30 * time.Second
	etcdRewriteSlowPollInterval    = time.Minute
	etcdRewriteSlowPollAfter       = 10
	etcdRewriteTimeout             = 6 * time.Hour
)

type EnableEtcdEncryptionInput struct {
	ClusterID uuid.UUID `json:"clusterID"`
}

func EnableEtcdEncryption(ctx workflow.Context, input EnableEtcdEncryptionInput) error {
	logger := workflow.GetLogger(ctx)

	if err := enableEtcdEncryption(ctx, input); err != nil {
		// On unrecoverable failure we do NOT unwind partially-applied node state.
		// Neither did SWE: although its rollout set RollbackCascadeOnUnrecoverable,
		// the etcd EncryptionOperation implemented no RollbackHandler, so that cascade
		// was inert and SWE also left partial state in place on failure. Recovery on
		// both sides is by re-running the workflow. EnsurePendingEtcdEncryptionKey
		// reuses the existing key, Initialize and Promote converge when replayed, a
		// new workflow run uses a fresh rewrite ID and re-establishes the completion
		// labels needed by Remove, and the kubeadm-config runbook is idempotent. The
		// API clears UpdateFailed before starting the next attempt, so a retry is
		// always permitted. Here we only clear the in-progress condition (on a
		// disconnected context so it still runs if the workflow is being canceled)
		// and record the failure.
		if input.ClusterID != uuid.Nil {
			cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
			if clearErr := clearEtcdEncryptionUpdateInProgress(cleanupCtx, input.ClusterID, etcdEncryptionFailureMessage); clearErr != nil {
				logger.Error("failed to clear etcd encryption update progress after workflow failure", "error", clearErr)
			}
		}
		return fmt.Errorf("failed to enable etcd encryption: %w", err)
	}

	return nil
}

func enableEtcdEncryption(ctx workflow.Context, input EnableEtcdEncryptionInput) error {
	if input.ClusterID == uuid.Nil {
		return errors.New("clusterID is required")
	}

	var a *activities.Activities
	var plan activities.PrepareEtcdEncryptionResult
	if err := workflow.ExecuteActivity(
		withEtcdEncryptionOptions(ctx),
		a.PrepareEtcdEncryption,
		activities.PrepareEtcdEncryptionInput{ClusterID: input.ClusterID},
	).Get(ctx, &plan); err != nil {
		return fmt.Errorf("failed to prepare etcd encryption: %w", err)
	}
	if plan.AlreadyEncrypted {
		return completeEtcdEncryption(ctx, input.ClusterID)
	}

	if err := validateEtcdEncryptionHealth(ctx, input.ClusterID, plan.NodeIDs); err != nil {
		return err
	}
	if err := executeEtcdEncryptionRunbook(
		ctx,
		plan.ExecutorNodeID,
		etcdEncryptionConfigRunbook,
		map[string]string{etcdEncryptionValidateOnlyEnv: "true"},
	); err != nil {
		return err
	}

	cluster, err := rollouttemporal.LoadClusterForWorkflow(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to load cluster for etcd encryption rollout: %w", err)
	}

	if err := workflow.ExecuteActivity(
		withEtcdEncryptionOptions(ctx),
		a.EnsurePendingEtcdEncryptionKey,
		activities.EtcdEncryptionClusterInput{ClusterID: input.ClusterID},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to ensure pending etcd encryption key: %w", err)
	}

	if err := executeEtcdEncryptionRolloutPhase(
		ctx,
		cluster,
		rolloutetcd.NewInitializeOperation(
			rolloutetcd.WithObjectsToEncrypt(rolloutetcd.DefaultObjectsToEncrypt()),
		),
		withKubeVipNodeGuard(),
	); err != nil {
		return err
	}
	if err := executeEtcdEncryptionRolloutPhase(
		ctx,
		cluster,
		rolloutetcd.NewPromoteProvidersOperation(),
	); err != nil {
		return err
	}

	if err := rewriteEtcdEncryptedObjects(ctx, plan.ExecutorNodeID); err != nil {
		return err
	}

	if err := executeEtcdEncryptionRolloutPhase(
		ctx,
		cluster,
		rolloutetcd.NewRemovePlainTextSecondaryOperation(),
	); err != nil {
		return err
	}

	if err := executeEtcdEncryptionRunbook(ctx, plan.ExecutorNodeID, etcdEncryptionConfigRunbook, nil); err != nil {
		return err
	}
	if err := workflow.ExecuteActivity(
		withEtcdEncryptionOptions(ctx),
		a.FinalizePendingEtcdEncryptionKey,
		activities.EtcdEncryptionClusterInput{ClusterID: input.ClusterID},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to finalize pending etcd encryption key: %w", err)
	}

	if err := completeEtcdEncryption(ctx, input.ClusterID); err != nil {
		return fmt.Errorf("failed to mark etcd encryption complete: %w", err)
	}

	return nil
}

func executeEtcdEncryptionRolloutPhase(
	ctx workflow.Context,
	cluster rollout.Cluster,
	operation rollout.Operation,
	opts ...etcdRolloutPhaseOption,
) error {
	options := etcdRolloutPhaseOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	result, err := rollouttemporal.Execute(
		rollouttemporal.WithRolloutNodeAgentApplyOptions(ctx),
		cluster,
		rollout.Compile(operation),
		etcdEncryptionRolloutSpec(len(cluster.ControlPlaneNodes()), options),
	)
	if err != nil {
		return fmt.Errorf("%s rollout executor failed: %w", operation.Name(), err)
	}
	if err := result.Err(); err != nil {
		return fmt.Errorf("%s rollout failed: %w", operation.Name(), err)
	}
	return nil
}

type etcdRolloutPhaseOptions struct {
	withKubeVipNodeGuard bool
}

type etcdRolloutPhaseOption func(*etcdRolloutPhaseOptions)

func withKubeVipNodeGuard() etcdRolloutPhaseOption {
	return func(options *etcdRolloutPhaseOptions) {
		options.withKubeVipNodeGuard = true
	}
}

func etcdEncryptionRolloutSpec(controlPlaneCount int, options etcdRolloutPhaseOptions) rollout.RolloutSpec {
	spec := rollout.RolloutSpec{
		Strategy:           rollout.StrategySerial,
		MinHealthyReplicas: rollout.MinHealthyReplicasPolicy{MinHealthy: controlPlaneCount},
		RollbackPolicy:     rollout.RollbackCascadeOnUnrecoverable,
		ClusterPreflights:  rolloutpreflights.DefaultClusterPreflights(),
		NodePreflights: []rollout.NodePreflightRef{
			{Name: rolloutpreflights.PreflightNameNodeAgentReady},
			{
				Name: rolloutpreflights.PreflightNameNodeAgentVersion,
				Params: map[string]string{
					rolloutpreflights.ParamMinVersion: minEtcdEncryptionNodeAgentVersion,
				},
			},
		},
	}
	if options.withKubeVipNodeGuard {
		spec.NodeGuards = []rollout.NodeGuardRef{{Name: rolloutkubevip.GuardNameKubeVipDrainRestore}}
	}
	return spec
}

func validateEtcdEncryptionHealth(ctx workflow.Context, clusterID uuid.UUID, nodeIDs []uuid.UUID) error {
	var a *activities.Activities
	if err := workflow.ExecuteActivity(
		withEtcdEncryptionHealthOptions(ctx),
		a.ValidateEtcdEncryptionHealth,
		activities.ValidateEtcdEncryptionHealthInput{ClusterID: clusterID, ExpectedNodeIDs: nodeIDs},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("control-plane health preflight failed: %w", err)
	}
	return nil
}

func executeEtcdEncryptionRunbook(
	ctx workflow.Context,
	nodeID uuid.UUID,
	runbookName string,
	runbookEnv map[string]string,
) error {
	var a *activities.Activities
	if err := workflow.ExecuteActivity(
		withEtcdEncryptionRunbookOptions(ctx),
		a.ExecuteHostRunbook,
		activities.ExecuteHostRunbookInput{HostID: nodeID, RunbookName: runbookName, RunbookEnv: runbookEnv},
	).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to execute %s runbook: %w", runbookName, err)
	}
	return nil
}

func rewriteEtcdEncryptedObjects(ctx workflow.Context, nodeID uuid.UUID) error {
	return rewriteEtcdEncryptedObjectsWithTimeout(ctx, nodeID, etcdRewriteTimeout)
}

func rewriteEtcdEncryptedObjectsWithTimeout(ctx workflow.Context, nodeID uuid.UUID, timeout time.Duration) error {
	if timeout <= 0 {
		return errors.New("encrypted-object rewrite timeout must be positive")
	}
	var a *activities.Activities
	execution := workflow.GetInfo(ctx).WorkflowExecution
	requestedOperationID := fmt.Sprintf("encrypt-etcd-%s-%s", execution.ID, execution.RunID)
	input := activities.EtcdEncryptionRewriteInput{NodeID: nodeID, OperationID: requestedOperationID}
	deadline := workflow.Now(ctx).Add(timeout)

	var operation activities.EtcdEncryptionRewriteOperation
	if err := workflow.ExecuteActivity(
		withEtcdEncryptionOptions(ctx),
		a.StartEtcdEncryptionRewrite,
		input,
	).Get(ctx, &operation); err != nil {
		return fmt.Errorf("failed to start encrypted-object rewrite: %w", err)
	}
	if operation.OperationID == "" {
		return errors.New("encrypted-object rewrite returned an empty operation ID")
	}
	// Start may resume an operation left running by an earlier workflow run.
	input.OperationID = operation.OperationID
	if done, err := validateEtcdEncryptionRewriteOperation(input.OperationID, operation); done || err != nil {
		return err
	}

	for polls := 0; ; polls++ {
		interval := etcdRewriteInitialPollInterval
		if polls >= etcdRewriteSlowPollAfter {
			interval = etcdRewriteSlowPollInterval
		}
		remaining := deadline.Sub(workflow.Now(ctx))
		if remaining <= 0 {
			return fmt.Errorf("encrypted-object rewrite %s did not complete within %s", input.OperationID, timeout)
		}
		if interval > remaining {
			interval = remaining
		}
		if err := workflow.Sleep(ctx, interval); err != nil {
			return err
		}
		if !workflow.Now(ctx).Before(deadline) {
			return fmt.Errorf("encrypted-object rewrite %s did not complete within %s", input.OperationID, timeout)
		}
		if err := workflow.ExecuteActivity(
			withEtcdEncryptionOptions(ctx),
			a.GetEtcdEncryptionRewriteStatus,
			input,
		).Get(ctx, &operation); err != nil {
			return fmt.Errorf("failed to poll encrypted-object rewrite: %w", err)
		}
		if done, err := validateEtcdEncryptionRewriteOperation(input.OperationID, operation); done || err != nil {
			return err
		}
	}
}

func validateEtcdEncryptionRewriteOperation(
	operationID string,
	operation activities.EtcdEncryptionRewriteOperation,
) (bool, error) {
	if operation.OperationID != operationID {
		return false, fmt.Errorf("encrypted-object rewrite returned operation ID %q, expected %q", operation.OperationID, operationID)
	}
	switch operation.Phase {
	case activities.EtcdEncryptionOperationPhaseRunning,
		activities.EtcdEncryptionOperationPhaseUnspecified:
		return false, nil
	case activities.EtcdEncryptionOperationPhaseSucceeded:
		return true, nil
	case activities.EtcdEncryptionOperationPhaseFailed:
		return false, fmt.Errorf("encrypted-object rewrite failed: %s", operation.ErrorMessage)
	default:
		return false, fmt.Errorf("encrypted-object rewrite returned unknown phase %q", operation.Phase)
	}
}

func withEtcdEncryptionOptions(ctx workflow.Context) workflow.Context {
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

// withEtcdEncryptionRunbookOptions matches withEtcdEncryptionOptions but adds a
// HeartbeatTimeout so a hung ExecuteHostRunbook activity (which heartbeats
// internally) is detected promptly rather than only at StartToCloseTimeout. This
// mirrors the runbook activity options used elsewhere (see UpdateKubeadmConfigMap).
func withEtcdEncryptionRunbookOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:    defaultRunbookActivityHeartbeatTimeout(),
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    10 * time.Second,
			MaximumInterval:    time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    3,
		},
	})
}

func withEtcdEncryptionHealthOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    2 * time.Minute,
		ScheduleToCloseTimeout: 30 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    10 * time.Second,
			MaximumInterval:    time.Minute,
			BackoffCoefficient: 2,
			MaximumAttempts:    0,
		},
	})
}

func completeEtcdEncryption(ctx workflow.Context, clusterID uuid.UUID) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.CompleteEtcdEncryption, activities.CompleteEtcdEncryptionInput{
		ClusterID: clusterID,
	}).Get(ctx, nil)
}

func clearEtcdEncryptionUpdateInProgress(ctx workflow.Context, clusterID uuid.UUID, failureMessage string) error {
	var a *activities.Activities

	return workflow.ExecuteActivity(withDBOptions(ctx), a.ClearEtcdEncryptionUpdateInProgress, activities.ClearEtcdEncryptionUpdateInProgressInput{
		ClusterID:      clusterID,
		FailureMessage: failureMessage,
	}).Get(ctx, nil)
}
