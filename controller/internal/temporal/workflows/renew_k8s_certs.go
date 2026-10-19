// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	adaptercontroller "github.com/dsx-ai-factory/nvke/controller/internal/rollout/adapters/controller"
	rolloutkubevip "github.com/dsx-ai-factory/nvke/controller/internal/rollout/guards/kubevip"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
)

const (
	RenewK8sCertsWorkflowType   = "RenewK8sCerts"
	RenewK8sCertsV2WorkflowType = "RenewK8sCertsV2"

	certificateRenewalFailure   = "Kubernetes certificate renewal failed"
	certificateRenewalPreflight = "preflight"
	certificateRenewalRenewNode = "renew-node"

	certificateRenewalExpiryCheckChangeID = "certificate-renewal-check-before-drain"
	// certificateRenewalTypedAfterLegacyChangeID marks the typed dispatch that
	// follows a successful legacy run in one cycle. Histories recorded before it
	// returned the legacy result alone.
	certificateRenewalTypedAfterLegacyChangeID = "certificate-renewal-typed-after-legacy"
)

// RenewK8sCertsInput selects a threshold-based or forced cycle. V2 requires a
// positive threshold even when Force bypasses expiry selection; Force must not
// accompany ScheduledFor. The original workflow ignores the V2-only controls.
type RenewK8sCertsInput struct {
	ClusterID     uuid.UUID  `json:"clusterID"`
	ThresholdDays int        `json:"thresholdDays"`
	Force         bool       `json:"force"`
	ScheduledFor  *time.Time `json:"scheduledFor,omitempty"`
}

// RenewK8sCertsNodeResult records a node's completed contribution to the cycle.
// V2 appends it only after postflight succeeds; a missing entry does not prove
// that the node was never mutated.
type RenewK8sCertsNodeResult struct {
	NodeID       uuid.UUID                    `json:"nodeID"`
	NodeName     string                       `json:"nodeName"`
	Outcome      db.CertificateRenewalOutcome `json:"outcome"`
	Output       string                       `json:"output"`
	ErrorMessage string                       `json:"errorMessage"`
	ExitCode     int                          `json:"exitCode"`
}

// RenewK8sCertsResult aggregates V2 nodes that passed renewal and postflight.
// Outcome is renewed if any such node renewed, otherwise unchanged; callers must
// still check the workflow error because Results may cover only a partial cycle.
type RenewK8sCertsResult struct {
	Outcome db.CertificateRenewalOutcome `json:"outcome"`
	Results []RenewK8sCertsNodeResult    `json:"results"`
}

// RenewK8sCerts preserves the v1 typed-activity command sequence for existing
// Temporal histories. New scheduled and forced cycles use RenewK8sCertsV2.
func RenewK8sCerts(ctx workflow.Context, input RenewK8sCertsInput) (RenewK8sCertsResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("Renewing Kubernetes certs", "clusterID", input.ClusterID, "thresholdDays", input.ThresholdDays)

	var a *activities.Activities
	var controlPlaneNodes []db.Node
	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.ListControlPlaneNodes,
		input.ClusterID,
	).Get(ctx, &controlPlaneNodes); err != nil {
		return RenewK8sCertsResult{}, fmt.Errorf("failed to list control plane nodes: %w", err)
	}
	if len(controlPlaneNodes) == 0 {
		logger.Info("No control plane nodes found, skipping cert renewal", "clusterID", input.ClusterID)
		return RenewK8sCertsResult{}, nil
	}

	results := make([]RenewK8sCertsNodeResult, 0, len(controlPlaneNodes))
	for _, node := range controlPlaneNodes {
		var result activities.KubernetesCertificateRenewalResult
		workflowInfo := workflow.GetInfo(ctx)
		operationID := fmt.Sprintf(
			"k8s-cert-renewal/%s/%s/%s",
			workflowInfo.WorkflowExecution.ID,
			workflowInfo.WorkflowExecution.RunID,
			node.ID,
		)
		if err := workflow.ExecuteActivity(
			workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
				StartToCloseTimeout: 30 * time.Minute,
				HeartbeatTimeout:    defaultRunbookActivityHeartbeatTimeout(),
				RetryPolicy: &temporal.RetryPolicy{
					InitialInterval: 30 * time.Second,
					MaximumInterval: 2 * time.Minute,
					MaximumAttempts: 3,
				},
			}),
			a.RenewKubernetesCertificates,
			activities.KubernetesCertificateRenewalInput{
				NodeID:        node.ID,
				OperationID:   operationID,
				ThresholdDays: input.ThresholdDays,
			},
		).Get(ctx, &result); err != nil {
			return RenewK8sCertsResult{}, fmt.Errorf("failed to renew certs on node %s: %w", node.ID, err)
		}
		results = append(results, RenewK8sCertsNodeResult{
			NodeID:       node.ID,
			NodeName:     node.Name,
			Output:       result.Output,
			ErrorMessage: result.ErrorMessage,
			ExitCode:     result.ExitCode,
		})
	}

	return RenewK8sCertsResult{Results: results}, nil
}

// RenewK8sCertsV2 renews kubeadm-managed leaf certificates one control-plane
// node at a time. Forced invocations are operator-started and must use a unique
// Temporal workflow ID so their cycle cannot alias a scheduled invocation.
// With more than one control-plane node, each node's renewal runs between the
// kube-vip drain and restore guard used by the other apiserver-restarting
// rollouts, so worker kubelets pinned to that node fail over before its
// kube-apiserver restarts; a single control-plane node is renewed without the
// drain, because there is no other node to fail over to. Nonforced cycles check
// expiry first and skip both dispatch and drain when nothing is due. Restore runs on a disconnected
// context after success, failure or cancellation, and a failed drain or restore
// fails the cycle. Acceptance owns the cluster mutation lock; successful node
// results and health gates are required before advancing the due time. Failure
// cleanup observes the
// dispatched executor independently of cancellation and retains ownership when
// termination cannot be confirmed. The original workflow remains separate for
// existing V1 histories.
func RenewK8sCertsV2(ctx workflow.Context, input RenewK8sCertsInput) (RenewK8sCertsResult, error) {
	if input.ClusterID == uuid.Nil {
		return RenewK8sCertsResult{}, errors.New("clusterID is required")
	}
	if input.ThresholdDays <= 0 {
		return RenewK8sCertsResult{}, errors.New("thresholdDays must be positive")
	}
	if input.Force && input.ScheduledFor != nil {
		return RenewK8sCertsResult{}, errors.New("forced certificate renewal cannot have a scheduled due time")
	}

	cycleID := workflow.GetInfo(ctx).WorkflowExecution.ID
	logger := workflow.GetLogger(ctx)
	logger.Info(
		"Renewing Kubernetes certificates",
		"clusterID", input.ClusterID,
		"cycleID", cycleID,
		"thresholdDays", input.ThresholdDays,
		"forced", input.Force,
	)

	var a *activities.Activities
	var begin activities.BeginCertificateRenewalResult
	// Resolve acceptance even after cancellation: Begin may already have committed the mutation lock.
	acceptanceCtx, _ := workflow.NewDisconnectedContext(ctx)
	if err := workflow.ExecuteActivity(
		withDBOptions(acceptanceCtx),
		a.BeginCertificateRenewal,
		activities.BeginCertificateRenewalInput{
			ClusterID:    input.ClusterID,
			CycleID:      cycleID,
			Forced:       input.Force,
			ScheduledFor: input.ScheduledFor,
		},
	).Get(acceptanceCtx, &begin); err != nil {
		return RenewK8sCertsResult{}, fmt.Errorf("failed to begin certificate renewal: %w", err)
	}
	if begin.AlreadyCompleted {
		return RenewK8sCertsResult{Outcome: begin.Outcome}, nil
	}

	result, failure, err := renewK8sCerts(ctx, input, cycleID)
	if err != nil {
		cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
		if clearErr := failCertificateRenewal(cleanupCtx, input.ClusterID, cycleID, failure, err); clearErr != nil {
			logger.Error("Failed to persist certificate-renewal failure", "error", clearErr)
		}
		return result, fmt.Errorf("%s: %w", certificateRenewalFailure, err)
	}

	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.CompleteCertificateRenewal,
		activities.CompleteCertificateRenewalInput{
			ClusterID: input.ClusterID,
			CycleID:   cycleID,
			Outcome:   result.Outcome,
		},
	).Get(ctx, nil); err != nil {
		cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
		if clearErr := failCertificateRenewal(cleanupCtx, input.ClusterID, cycleID, certificateRenewalFailureDetails{phase: "complete"}, err); clearErr != nil {
			logger.Error("Failed to persist certificate-renewal completion failure", "error", clearErr)
		}
		return result, fmt.Errorf("failed to complete certificate renewal: %w", err)
	}
	return result, nil
}

// certificateRenewalFailureDetails separates diagnostics from the operation whose
// termination is uncertain. Healthy diagnostic postflight cannot clear that
// uncertainty; cleanup needs the original node, operation ID and executor.
type certificateRenewalFailureDetails struct {
	nodeID              *uuid.UUID
	phase               string
	unresolvedOperation *activities.ResolveKubernetesCertificateRenewalInput
}

// renewK8sCerts fixes node order and membership for the entire cycle. Each node's
// postflight is the gate to the next; on failure it stops the sequence and may
// gather diagnostics, but leaves ownership resolution to the caller.
func renewK8sCerts(
	ctx workflow.Context,
	input RenewK8sCertsInput,
	cycleID string,
) (RenewK8sCertsResult, certificateRenewalFailureDetails, error) {
	if err := ctx.Err(); err != nil {
		return RenewK8sCertsResult{}, certificateRenewalFailureDetails{phase: certificateRenewalPreflight}, err
	}

	var a *activities.Activities
	var controlPlaneNodes []db.Node
	if err := workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.ListControlPlaneNodes,
		input.ClusterID,
	).Get(ctx, &controlPlaneNodes); err != nil {
		return RenewK8sCertsResult{}, certificateRenewalFailureDetails{phase: "list-control-plane-nodes"}, fmt.Errorf("failed to list control-plane nodes: %w", err)
	}
	if len(controlPlaneNodes) == 0 {
		return RenewK8sCertsResult{}, certificateRenewalFailureDetails{phase: certificateRenewalPreflight}, errors.New("no control-plane nodes found")
	}
	slices.SortFunc(controlPlaneNodes, func(left, right db.Node) int {
		if byName := strings.Compare(left.Name, right.Name); byName != 0 {
			return byName
		}
		return strings.Compare(left.ID.String(), right.ID.String())
	})

	expectedNodeIDs := make([]uuid.UUID, 0, len(controlPlaneNodes))
	for _, node := range controlPlaneNodes {
		expectedNodeIDs = append(expectedNodeIDs, node.ID)
	}
	if err := validateCertificateRenewalHealth(ctx, input.ClusterID, expectedNodeIDs, nil); err != nil {
		return RenewK8sCertsResult{}, certificateRenewalFailureDetails{phase: certificateRenewalPreflight}, fmt.Errorf("certificate-renewal preflight failed: %w", err)
	}

	// The guard activities only need identities, so wrap the cycle's node
	// snapshot instead of reloading inventory; the agent port is never read here.
	rolloutCluster := adaptercontroller.NewCluster(db.Cluster{ID: input.ClusterID}, controlPlaneNodes, controlPlaneNodes, "")

	result := RenewK8sCertsResult{
		Outcome: db.CertificateRenewalOutcomeUnchanged,
		Results: make([]RenewK8sCertsNodeResult, 0, len(controlPlaneNodes)),
	}
	// Existing V2 histories retain their original guard/dispatch command sequence.
	checkExpiry := workflow.GetVersion(ctx, certificateRenewalExpiryCheckChangeID, workflow.DefaultVersion, 1) >= 1
	for _, node := range controlPlaneNodes {
		nodeID := node.ID
		nodeInput := activities.KubernetesCertificateRenewalInput{
			NodeID:              node.ID,
			OperationID:         certificateRenewalOperationID(cycleID, node.ID),
			ThresholdDays:       input.ThresholdDays,
			Force:               input.Force,
			ExpectedEtcdMembers: len(controlPlaneNodes),
		}
		renewalResult, unresolved, err := renewCertificatesOnNodeForCycle(ctx, rolloutCluster, node, nodeInput, checkExpiry)
		if err != nil {
			renewErr := fmt.Errorf("failed to renew certificates on node %s: %w", node.ID, err)
			diagnosticCtx, _ := workflow.NewDisconnectedContext(ctx)
			if healthErr := validateCertificateRenewalHealth(diagnosticCtx, input.ClusterID, expectedNodeIDs, &nodeID); healthErr != nil {
				workflow.GetLogger(ctx).Error(
					"Certificate-renewal diagnostic postflight failed",
					"nodeID", node.ID,
					"error", healthErr,
				)
				renewErr = fmt.Errorf("%w; diagnostic postflight failed: %w", renewErr, healthErr)
			}
			return result, certificateRenewalFailureDetails{nodeID: &nodeID, phase: certificateRenewalRenewNode, unresolvedOperation: unresolved}, renewErr
		}

		if err := validateCertificateRenewalHealth(ctx, input.ClusterID, expectedNodeIDs, &nodeID); err != nil {
			return result, certificateRenewalFailureDetails{nodeID: &nodeID, phase: "postflight"}, fmt.Errorf("certificate-renewal postflight failed for node %s: %w", node.ID, err)
		}
		outcome := renewalResult.Outcome
		if outcome == db.CertificateRenewalOutcomeRenewed {
			result.Outcome = db.CertificateRenewalOutcomeRenewed
		}
		result.Results = append(result.Results, RenewK8sCertsNodeResult{
			NodeID:       node.ID,
			NodeName:     node.Name,
			Outcome:      outcome,
			Output:       renewalResult.Output,
			ErrorMessage: renewalResult.ErrorMessage,
			ExitCode:     renewalResult.ExitCode,
		})
	}
	return result, certificateRenewalFailureDetails{}, nil
}

// renewCertificatesOnNodeForCycle selects only due nodes before any guard or
// mutating dispatch. The read-only result is recorded as unchanged; Force skips
// selection. Pending legacy-runbook intent goes straight to the legacy executor,
// the only one that can finish its restarts; when the agent also holds pending
// intent, the typed executor follows in the same cycle so neither restart waits
// for the next due time. A single control-plane node never drains, even when forced.
func renewCertificatesOnNodeForCycle(
	ctx workflow.Context,
	cluster rollout.Cluster,
	node db.Node,
	input activities.KubernetesCertificateRenewalInput,
	checkExpiry bool,
) (activities.KubernetesCertificateRenewalResult, *activities.ResolveKubernetesCertificateRenewalInput, error) {
	legacy, thenTyped := false, false
	if checkExpiry && !input.Force {
		var a *activities.Activities
		var check activities.KubernetesCertificateRenewalCheckResult
		if err := workflow.ExecuteActivity(certificateRenewalHealthOptions(ctx), a.CheckKubernetesCertificateRenewalRequired, input).Get(ctx, &check); err != nil {
			return activities.KubernetesCertificateRenewalResult{}, nil, fmt.Errorf("certificate expiry check failed: %w", err)
		}
		if !check.Required {
			return activities.KubernetesCertificateRenewalResult{Outcome: db.CertificateRenewalOutcomeUnchanged}, nil, nil
		}
		legacy, thenTyped = check.Legacy, check.Legacy && check.Typed
	}
	if input.ExpectedEtcdMembers == 1 {
		return renewCertificatesOnNodeWithExecutors(ctx, input, legacy, thenTyped)
	}
	return renewCertificatesOnNodeBehindKubeVipDrain(ctx, cluster, adaptercontroller.NewNode(node, ""), input, legacy, thenTyped)
}

// renewCertificatesOnNodeWithExecutors runs the selected executor and, when
// thenTyped is set after a legacy run, the typed executor next so pending agent
// intent also finishes. Either executor renewing makes the node renewed.
func renewCertificatesOnNodeWithExecutors(
	ctx workflow.Context,
	input activities.KubernetesCertificateRenewalInput,
	legacy bool,
	thenTyped bool,
) (activities.KubernetesCertificateRenewalResult, *activities.ResolveKubernetesCertificateRenewalInput, error) {
	result, unresolved, err := renewCertificatesOnNode(ctx, input, legacy)
	if err != nil || !legacy || !thenTyped {
		return result, unresolved, err
	}
	// Called only on this path so histories that never reach it are unaffected.
	if workflow.GetVersion(ctx, certificateRenewalTypedAfterLegacyChangeID, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return result, unresolved, nil
	}
	typedResult, unresolved, err := renewCertificatesOnNode(ctx, input, false)
	if err != nil {
		return typedResult, unresolved, err
	}
	if result.Outcome == db.CertificateRenewalOutcomeRenewed {
		typedResult.Outcome = db.CertificateRenewalOutcomeRenewed
	}
	return typedResult, nil, nil
}

// renewCertificatesOnNodeBehindKubeVipDrain brackets one node's renewal with the
// kube-vip drain/restore node guard. Drain is persisted before dispatch and
// restore always runs on a disconnected context, so a failed or canceled renewal
// cannot leave the node's VIP drained. A drain or restore failure is joined into
// the returned error; unresolved still describes only the renewal dispatch, so
// mutation-lock resolution is unaffected. Agents without a kube-vip
// configuration treat drain and restore as no-ops. An agent predating the drain
// RPCs fails closed, as in the other apiserver-restarting rollouts: no renewal
// is dispatched to a node whose VIP could not be drained.
func renewCertificatesOnNodeBehindKubeVipDrain(
	ctx workflow.Context,
	cluster rollout.Cluster,
	node rollout.Node,
	input activities.KubernetesCertificateRenewalInput,
	legacy bool,
	thenTyped bool,
) (activities.KubernetesCertificateRenewalResult, *activities.ResolveKubernetesCertificateRenewalInput, error) {
	var result activities.KubernetesCertificateRenewalResult
	var unresolved *activities.ResolveKubernetesCertificateRenewalInput
	if err := ctx.Err(); err != nil {
		return result, nil, err // Nothing was drained or dispatched.
	}
	err := rollouttemporal.RunWithNodeGuards(
		rollouttemporal.WithRolloutNodeAgentApplyOptions(ctx),
		cluster,
		node,
		[]rollout.NodeGuardRef{{Name: rolloutkubevip.GuardNameKubeVipDrainRestore}},
		func() error {
			var renewErr error
			result, unresolved, renewErr = renewCertificatesOnNodeWithExecutors(ctx, input, legacy, thenTyped)
			return renewErr
		},
	)
	return result, unresolved, err
}

// renewCertificatesOnNode records executor selection in workflow history before
// dispatching legacy work. Legacy selects that executor without a typed attempt.
// A canceled or failed activity can leave remote work running; a non-nil
// resolution input identifies what cleanup must observe, including dispatches
// whose acceptance is unknown.
func renewCertificatesOnNode(
	ctx workflow.Context,
	input activities.KubernetesCertificateRenewalInput,
	legacy bool,
) (activities.KubernetesCertificateRenewalResult, *activities.ResolveKubernetesCertificateRenewalInput, error) {
	var a *activities.Activities
	var result activities.KubernetesCertificateRenewalResult
	if err := ctx.Err(); err != nil {
		return result, nil, err
	}
	pending := &activities.ResolveKubernetesCertificateRenewalInput{NodeID: input.NodeID, OperationID: input.OperationID}
	var err error
	if !legacy {
		err = workflow.ExecuteActivity(certificateRenewalNodeOptions(ctx), a.RenewKubernetesCertificates, input).Get(ctx, &result)
	}
	var applicationErr *temporal.ApplicationError
	if errors.As(err, &applicationErr) && applicationErr.Type() == activities.CertificateRenewalNotDispatched {
		return result, nil, err
	}
	if legacy || errors.As(err, &applicationErr) && applicationErr.Type() == activities.CertificateRenewalLegacyRequired {
		if err := ctx.Err(); err != nil {
			return result, nil, err // Typed work was rejected and legacy work has not been dispatched.
		}
		pending.Legacy = true
		err = workflow.ExecuteActivity(certificateRenewalNodeOptions(ctx), a.RenewLegacyKubernetesCertificates, input).Get(ctx, &result)
		if errors.As(err, &applicationErr) && applicationErr.Type() == activities.CertificateRenewalNotDispatched {
			return result, nil, err // Admission refused the node before the runbook ran.
		}
	}
	if err != nil {
		return result, pending, err
	}
	if result.Outcome != db.CertificateRenewalOutcomeUnchanged && result.Outcome != db.CertificateRenewalOutcomeRenewed {
		return result, pending, fmt.Errorf("invalid renewal outcome %q from node %s", result.Outcome, input.NodeID)
	}
	return result, nil, nil
}

func certificateRenewalNodeOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 75 * time.Minute,
		HeartbeatTimeout:    defaultRunbookActivityHeartbeatTimeout(),
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 30 * time.Second,
			MaximumInterval: 2 * time.Minute,
			MaximumAttempts: 3,
		},
	})
}

func certificateRenewalHealthOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 10 * time.Second,
			MaximumInterval: 30 * time.Second,
			MaximumAttempts: 3,
		},
	})
}

func validateCertificateRenewalHealth(
	ctx workflow.Context,
	clusterID uuid.UUID,
	expectedNodeIDs []uuid.UUID,
	postflightNodeID *uuid.UUID,
) error {
	var a *activities.Activities
	return workflow.ExecuteActivity(
		certificateRenewalHealthOptions(ctx),
		a.ValidateCertificateRenewalHealth,
		activities.ValidateCertificateRenewalHealthInput{
			ClusterID:        clusterID,
			ExpectedNodeIDs:  expectedNodeIDs,
			PostflightNodeID: postflightNodeID,
		},
	).Get(ctx, nil)
}

// failCertificateRenewal uses bounded status-only resolution before deciding
// whether to release the mutation lock. Callers must supply a disconnected
// context so cancellation cannot skip cleanup; a resolution failure is persisted
// as retained ownership, not treated as proof that node work was canceled.
func failCertificateRenewal(
	ctx workflow.Context,
	clusterID uuid.UUID,
	cycleID string,
	failure certificateRenewalFailureDetails,
	workflowErr error,
) error {
	var a *activities.Activities
	retainMutationLock := false
	message := workflowErr.Error()
	if failure.unresolvedOperation != nil {
		resolutionCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			ScheduleToCloseTimeout: 5 * time.Minute,
			StartToCloseTimeout:    5 * time.Minute,
			HeartbeatTimeout:       defaultRunbookActivityHeartbeatTimeout(),
			RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 1},
		})
		if err := workflow.ExecuteActivity(resolutionCtx, a.ResolveKubernetesCertificateRenewal, *failure.unresolvedOperation).Get(ctx, nil); err != nil {
			retainMutationLock = true
			// Put retention first so the bounded database diagnostic cannot hide it.
			message = fmt.Sprintf("cluster mutation lock retained: termination of operation %s is unconfirmed (%v); %s",
				failure.unresolvedOperation.OperationID, err, message)
		}
	}
	return workflow.ExecuteActivity(
		withDBOptions(ctx),
		a.FailCertificateRenewal,
		activities.FailCertificateRenewalInput{
			ClusterID:          clusterID,
			CycleID:            cycleID,
			FailedNodeID:       failure.nodeID,
			Phase:              failure.phase,
			Message:            message,
			RetainMutationLock: retainMutationLock,
		},
	).Get(ctx, nil)
}

// certificateRenewalOperationID is stable across activity retries and runs of the
// same workflow ID. Hashing gives both executors a bounded, shell-safe identity;
// distinct intended cycles must therefore use distinct workflow IDs.
func certificateRenewalOperationID(cycleID string, nodeID uuid.UUID) string {
	digest := sha256.Sum256([]byte(cycleID + "/" + nodeID.String()))
	return fmt.Sprintf("k8s-cert-renewal-%x", digest)
}
