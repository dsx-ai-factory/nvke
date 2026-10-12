// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/workflow"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
)

const (
	defaultNodeGuardCheckPollInterval = 10 * time.Second
)

// appliedNodeRef records a node that succeeded ApplyToNode.
type appliedNodeRef struct {
	ID   uuid.UUID
	Name string
}

// openedClusterGuardRecord / openedNodeGuardRecord shuttle the
// GuardState envelope from each Pre* activity to its matching Post*.
type openedClusterGuardRecord struct {
	Ref   rollout.ClusterGuardRef
	State rollout.GuardState
}

type openedNodeGuardRecord struct {
	Ref                    rollout.NodeGuardRef
	State                  rollout.GuardState
	CheckPostApplyRequired bool
}

// phaseFailedResult builds a ResultFailed for a cluster-scoped phase.
func phaseFailedResult(phase rollout.Phase, name string, err error) rollout.Result {
	return rollout.Result{
		Status: rollout.ResultFailed,
		PhaseFailure: &rollout.PhaseFailure{
			Phase: phase,
			Name:  name,
			Err:   err.Error(),
		},
	}
}

// Execute runs the rollout from inside a workflow body. Sequence:
// SatisfactionChecker → Validator → ClusterPreflights →
// ClusterGuard.PreRollout → BuildApplyToNodePayload → per-node
// (NodePreflights → NodeGuard.PreApply → ApplyToNode → NodeGuard.PostApply)
// → CheckPostCondition → ClusterGuard.PostRollout, with cascade
// rollback on FailedNotRestored.
//
// Each cluster-scoped phase is a helper returning (Result, terminal?);
// the per-node loop lives in nodeLoop. Execute itself is just the linear
// spine that wires them together and owns the PostRollout finalizer.
func Execute(
	ctx workflow.Context,
	cluster rollout.Cluster,
	op rollout.CompiledOperation,
	spec rollout.RolloutSpec,
) (rollout.Result, error) {
	if err := spec.Validate(); err != nil {
		return rollout.Result{}, err
	}
	caps := op.Capabilities()
	opName := op.Name()
	cpNodes := cluster.ControlPlaneNodes()

	log := workflow.GetLogger(ctx)
	log.Info("Temporal rollout starting",
		"operation", opName,
		"cluster", cluster.Name(),
		"cpNodes", len(cpNodes),
		"clusterGuards", len(spec.ClusterGuards),
		"nodeGuards", len(spec.NodeGuards),
	)

	// Reject an infeasible min-healthy-replicas floor before any guard
	// opens or node is touched.
	if spec.MinHealthyReplicas.MinHealthy > 0 && spec.MinHealthyReplicas.MinHealthy > len(cpNodes) {
		return phaseFailedResult(
			rollout.PhaseMinHealthyReplicas, "",
			fmt.Errorf("infeasible minHealthyReplicas: MinHealthy=%d > control-plane count %d",
				spec.MinHealthyReplicas.MinHealthy, len(cpNodes)),
		), nil
	}

	// 1. SatisfactionChecker (may short-circuit to AlreadySatisfied).
	if r, done := runSatisfactionCheck(ctx, caps, opName, cluster); done {
		return r, nil
	}

	// 2. Validator.
	if r, failed := runValidator(ctx, caps, opName, cluster); failed {
		return r, nil
	}

	// 3. ClusterPreflights (declaration order).
	if r, failed := runClusterPreflights(ctx, cluster, spec.ClusterPreflights); failed {
		return r, nil
	}

	// 4. ClusterGuard.PreRollout (declaration order).
	openedClusterGuards, failResult := openClusterGuards(ctx, cluster, spec.ClusterGuards)
	if failResult != nil {
		runClusterGuardPostRollout(ctx, cluster, openedClusterGuards, *failResult)
		return *failResult, nil
	}

	// From here on, ClusterGuard.PostRollout is the deferred finalizer:
	// every terminal path assigns `result` and returns; the defer closure
	// captures the final `result` and fires on a disconnected context
	// regardless of which branch took us out.
	result := rollout.Result{}
	defer func() { runClusterGuardPostRollout(ctx, cluster, openedClusterGuards, result) }()

	// 5. BuildApplyToNodePayload.
	payload, buildFailure, failed := buildApplyToNodePayload(ctx, caps, opName, cluster)
	if failed {
		result = buildFailure
		return result, nil
	}

	// 6. Per-node loop. runNodes returns a Result whose Status is empty
	// only when every node applied cleanly; otherwise it is terminal.
	result = runNodes(ctx, op, caps, cluster, spec, payload, cpNodes)
	if result.Status != "" {
		return result, nil
	}

	// 7. PostConditionChecker (augments the existing result on failure).
	if runPostCondition(ctx, caps, opName, cluster, &result) {
		return result, nil
	}

	result.Status = rollout.ResultCompleted
	return result, nil
}

// runSatisfactionCheck runs the SatisfactionChecker. Returns (result,
// true) when the rollout is done — either short-circuited as
// AlreadySatisfied or aborted by a dispatch/business failure — and
// (zero, false) to continue.
func runSatisfactionCheck(
	ctx workflow.Context, caps rollout.Capabilities, opName string, cluster rollout.Cluster,
) (rollout.Result, bool) {
	if !caps.HasSatisfactionChecker {
		return rollout.Result{}, false
	}
	var resp IsSatisfiedResponse
	if err := workflow.ExecuteActivity(ctx, ActivityIsSatisfied,
		&IsSatisfiedRequest{OperationName: opName, ClusterID: cluster.ID()},
	).Get(ctx, &resp); err != nil {
		return phaseFailedResult(
			rollout.PhaseSatisfactionCheck, "",
			fmt.Errorf("is_satisfied dispatch: %w", err),
		), true
	}
	if resp.IsSatErr != "" {
		return phaseFailedResult(
			rollout.PhaseSatisfactionCheck, "",
			errors.New(resp.IsSatErr),
		), true
	}
	if resp.Satisfied {
		return rollout.Result{Status: rollout.ResultAlreadySatisfied}, true
	}
	return rollout.Result{}, false
}

// runValidator runs the operation-owned Validator. Returns (result,
// true) on failure (abort, no node touched), (zero, false) to continue.
func runValidator(
	ctx workflow.Context, caps rollout.Capabilities, opName string, cluster rollout.Cluster,
) (rollout.Result, bool) {
	if !caps.HasValidator {
		return rollout.Result{}, false
	}
	if err := workflow.ExecuteActivity(ctx, ActivityValidate,
		&ValidateRequest{OperationName: opName, ClusterID: cluster.ID()},
	).Get(ctx, nil); err != nil {
		return phaseFailedResult(
			rollout.PhaseValidate, "",
			fmt.Errorf("validate: %w", err),
		), true
	}
	return rollout.Result{}, false
}

// runClusterPreflights runs each ClusterPreflight in declaration order.
// Returns (result, true) on the first failure, (zero, false) if all pass.
func runClusterPreflights(
	ctx workflow.Context, cluster rollout.Cluster, refs []rollout.ClusterPreflightRef,
) (rollout.Result, bool) {
	for _, ref := range refs {
		if err := workflow.ExecuteActivity(ctx, ActivityClusterPreflight,
			&ClusterPreflightRequest{
				PreflightName: ref.Name,
				Params:        ref.Params,
				ClusterID:     cluster.ID(),
			},
		).Get(ctx, nil); err != nil {
			return phaseFailedResult(
				rollout.PhaseClusterPreflight, ref.Name,
				fmt.Errorf("cluster preflight %q: %w", ref.Name, err),
			), true
		}
	}
	return rollout.Result{}, false
}

// buildApplyToNodePayload runs the ApplyToNodePayloadBuilder once. Returns the built spec
// on success; on failure the third return is true and the second carries
// the terminal Result the caller must adopt.
func buildApplyToNodePayload(
	ctx workflow.Context, caps rollout.Capabilities, opName string, cluster rollout.Cluster,
) (rollout.ApplyToNodePayload, rollout.Result, bool) {
	if !caps.HasApplyToNodePayloadBuilder {
		return nil, rollout.Result{}, false
	}
	var resp BuildApplyToNodePayloadResponse
	if err := workflow.ExecuteActivity(ctx, ActivityBuildApplyToNodePayload,
		&BuildApplyToNodePayloadRequest{OperationName: opName, ClusterID: cluster.ID()},
	).Get(ctx, &resp); err != nil {
		return nil, phaseFailedResult(
			rollout.PhaseBuildApplyToNodePayload, "",
			fmt.Errorf("build_apply_to_node_payload dispatch: %w", err),
		), true
	}
	if resp.BuildErr != "" {
		return nil, phaseFailedResult(
			rollout.PhaseBuildApplyToNodePayload, "",
			errors.New(resp.BuildErr),
		), true
	}
	return resp.Payload, rollout.Result{}, false
}

// runPostCondition runs the PostConditionChecker. On failure it stamps
// the phase failure onto the existing result (preserving the per-node
// buckets) and returns true; failure does NOT cascade.
func runPostCondition(
	ctx workflow.Context, caps rollout.Capabilities, opName string,
	cluster rollout.Cluster, result *rollout.Result,
) bool {
	if !caps.HasPostConditionChecker {
		return false
	}
	if err := workflow.ExecuteActivity(ctx, ActivityCheckPostCond,
		&CheckPostConditionRequest{OperationName: opName, ClusterID: cluster.ID()},
	).Get(ctx, nil); err != nil {
		result.Status = rollout.ResultFailed
		result.PhaseFailure = &rollout.PhaseFailure{
			Phase: rollout.PhasePostCondition,
			Err:   err.Error(),
		}
		return true
	}
	return false
}

// nodeLoop carries the immutable rollout context plus the mutable state
// accumulated across the per-node loop (the assembling Result and the
// list of nodes that have successfully applied so far, for cascade).
type nodeLoop struct {
	ctx     workflow.Context
	op      rollout.CompiledOperation
	caps    rollout.Capabilities
	cluster rollout.Cluster
	spec    rollout.RolloutSpec
	payload rollout.ApplyToNodePayload

	result  rollout.Result
	applied []appliedNodeRef
}

// runNodes walks the per-node loop in declaration order. A terminal
// per-node failure stops the loop with Result.Status set (and may
// cascade); when every node applies cleanly the returned Status is empty
// so Execute proceeds to the post-condition.
func runNodes(
	ctx workflow.Context,
	op rollout.CompiledOperation,
	caps rollout.Capabilities,
	cluster rollout.Cluster,
	spec rollout.RolloutSpec,
	payload rollout.ApplyToNodePayload,
	cpNodes []rollout.Node,
) rollout.Result {
	l := &nodeLoop{
		ctx: ctx, op: op, caps: caps, cluster: cluster, spec: spec, payload: payload,
		applied: make([]appliedNodeRef, 0, len(cpNodes)),
	}
	for _, node := range cpNodes {
		if terminal := l.runNode(node); terminal {
			return l.result
		}
	}
	return l.result
}

// runNode runs one node through NodePreflights → NodeGuard.PreApply →
// ApplyToNode → NodeGuard.PostApply, then classifies the Outcome.
// Returns true when the rollout must stop on this node.
func (l *nodeLoop) runNode(node rollout.Node) bool {
	log := workflow.GetLogger(l.ctx)
	log.Info("Rollout: node start", "operation", l.op.Name(), "node", node.Name())

	// 6a. NodePreflights — node never touched on failure → SkippedPreflight
	// (distinct from FailedRestored, which implies an attempted apply).
	if name, err := l.runNodePreflights(node); err != nil {
		l.result.Nodes = append(l.result.Nodes, rollout.NodeResult{
			NodeID:   node.ID(),
			NodeName: node.Name(),
			Outcome:  rollout.OutcomeSkippedPreflight,
			Err:      fmt.Sprintf("node preflight %q: %v", name, err),
		})
		l.result.Status = rollout.ResultFailed
		return true
	}

	// 6b. NodeGuard.PreApply (declaration order).
	openedGuards, guardName, preApplyErr := l.openNodeGuards(node)
	if preApplyErr != nil {
		return l.handlePreApplyFailure(node, openedGuards, guardName, preApplyErr)
	}

	// 6c. ApplyToNode, then 6d. NodeGuard.PostApply (reverse, always runs).
	// effectiveOutcome is computed from the apply error before PostApply so
	// the guards see the apply verdict; a PostApply failure then joins into
	// applyErr and escalates the node to FailedNotRestored below.
	resp, applyErr := l.applyToNode(node)
	effectiveOutcome := resp.Outcome
	if applyErr != nil {
		effectiveOutcome = rollout.OutcomeFailedNotRestored
	}
	if postErr := runNodeGuardPostApply(l.ctx, l.cluster, node, openedGuards, effectiveOutcome); postErr != nil {
		applyErr = errors.Join(applyErr, postErr)
	}
	if applyErr != nil {
		return l.handleApplyFailure(node, resp, applyErr)
	}

	log.Info("Rollout: node complete",
		"operation", l.op.Name(), "node", node.Name(), "outcome", effectiveOutcome.String())
	return l.classifyOutcome(node, resp, effectiveOutcome)
}

// runNodePreflights runs each NodePreflight in declaration order,
// returning the name and error of the first failure.
func (l *nodeLoop) runNodePreflights(node rollout.Node) (string, error) {
	for _, ref := range l.spec.NodePreflights {
		if err := workflow.ExecuteActivity(l.ctx, ActivityNodePreflight,
			&NodePreflightRequest{
				PreflightName: ref.Name,
				Params:        ref.Params,
				ClusterID:     l.cluster.ID(),
				NodeID:        node.ID(),
			},
		).Get(l.ctx, nil); err != nil {
			return ref.Name, err
		}
	}
	return "", nil
}

// openNodeGuards dispatches each NodeGuard.PreApply in declaration order.
// err = activity dispatch failure or guard business failure; the opened
// slice records the State envelope (even on business failure) so PostApply
// can clean up any partial-state payload.
func (l *nodeLoop) openNodeGuards(node rollout.Node) ([]openedNodeGuardRecord, string, error) {
	opened := make([]openedNodeGuardRecord, 0, len(l.spec.NodeGuards))
	for _, gref := range l.spec.NodeGuards {
		var preResp NodeGuardPreApplyResponse
		if err := workflow.ExecuteActivity(l.ctx, ActivityNodeGuardPreApply,
			&NodeGuardPreApplyRequest{
				GuardName: gref.Name,
				Params:    gref.Params,
				ClusterID: l.cluster.ID(),
				NodeID:    node.ID(),
			},
		).Get(l.ctx, &preResp); err != nil {
			return opened, gref.Name, fmt.Errorf("dispatch: %w", err)
		}
		// Record State even on business failure so PostApply runs for
		// partial-state cleanup.
		opened = append(opened, openedNodeGuardRecord{
			Ref:                    gref,
			State:                  preResp.State,
			CheckPostApplyRequired: preResp.CheckPostApplyRequired,
		})
		if preResp.PreApplyErr != "" {
			return opened, gref.Name, errors.New(preResp.PreApplyErr)
		}
		if preResp.CheckPreApplyRequired {
			if err := waitNodeGuardCheckPreApply(l.ctx, l.cluster, node, gref, preResp.State); err != nil {
				return opened, gref.Name, err
			}
		}
	}
	return opened, "", nil
}

// applyToNode dispatches the ApplyToNode activity for one node.
func (l *nodeLoop) applyToNode(node rollout.Node) (ApplyToNodeResponse, error) {
	var resp ApplyToNodeResponse
	err := workflow.ExecuteActivity(l.ctx, ActivityApplyToNode,
		&ApplyToNodeRequest{
			OperationName: l.op.Name(),
			ClusterID:     l.cluster.ID(),
			NodeID:        node.ID(),
			Payload:       l.payload,
		},
	).Get(l.ctx, &resp)
	return resp, err
}

// handlePreApplyFailure unwinds the opened node guards after a PreApply
// failure. Unwind succeeded → FailedRestored, halt. Unwind failed →
// FailedNotRestored + cascade: leftover guard state may have touched disk.
func (l *nodeLoop) handlePreApplyFailure(
	node rollout.Node, openedGuards []openedNodeGuardRecord, guardName string, preApplyErr error,
) bool {
	nodeOutcome := rollout.OutcomeFailedRestored
	nodeErr := fmt.Errorf("node guard %q PreApply: %w", guardName, preApplyErr)
	if unwindErr := runNodeGuardPostApply(l.ctx, l.cluster, node, openedGuards, nodeOutcome); unwindErr != nil {
		nodeOutcome = rollout.OutcomeFailedNotRestored
		nodeErr = errors.Join(nodeErr, unwindErr)
	}
	l.result.Nodes = append(l.result.Nodes, rollout.NodeResult{
		NodeID:   node.ID(),
		NodeName: node.Name(),
		Outcome:  nodeOutcome,
		Err:      nodeErr.Error(),
	})
	if nodeOutcome == rollout.OutcomeFailedNotRestored {
		l.cascade(fmt.Sprintf("node guard unwind failed on node %s", node.Name()))
	} else {
		l.result.Status = rollout.ResultFailed
	}
	return true
}

// handleApplyFailure records a FailedNotRestored node after an apply or
// post-apply guard error, then cascades. Always terminal.
func (l *nodeLoop) handleApplyFailure(
	node rollout.Node, resp ApplyToNodeResponse, applyErr error,
) bool {
	failMsg := applyErr.Error()
	if resp.ApplyErr != "" {
		// ApplyToNode dispatched and the operation reported its own detail,
		// then PostApply failed and joined into applyErr. Surface both so
		// the operator detail isn't lost.
		failMsg = fmt.Sprintf("apply: %s; post-apply guard: %s", resp.ApplyErr, failMsg)
	}
	l.result.Nodes = append(l.result.Nodes, rollout.NodeResult{
		NodeID:   node.ID(),
		NodeName: node.Name(),
		Outcome:  rollout.OutcomeFailedNotRestored,
		Err:      failMsg,
	})
	l.cascade(fmt.Sprintf("apply activity error on node %s: %v", node.Name(), applyErr))
	return true
}

// classifyOutcome records the node by its authoritative Outcome and
// decides whether the rollout continues or terminates (with cascade where
// the Outcome/policy demands it). Err is left unset on success outcomes;
// the failure arms attach resp.ApplyErr where it's meaningful.
func (l *nodeLoop) classifyOutcome(
	node rollout.Node, resp ApplyToNodeResponse, outcome rollout.Outcome,
) bool {
	nodeResult := rollout.NodeResult{
		NodeID:   node.ID(),
		NodeName: node.Name(),
		Outcome:  outcome,
	}
	switch outcome {
	case rollout.OutcomeApplied:
		l.result.Nodes = append(l.result.Nodes, nodeResult)
		l.applied = append(l.applied, appliedNodeRef{ID: node.ID(), Name: node.Name()})
		return false
	case rollout.OutcomeUnchanged:
		l.result.Nodes = append(l.result.Nodes, nodeResult)
		return false
	case rollout.OutcomeFailedRestored:
		nodeResult.Err = resp.ApplyErr
		l.result.Nodes = append(l.result.Nodes, nodeResult)
		// CascadeOnUnrecoverable (default) halts without cascade on a clean
		// self-restore; HaltOnAnyFailure cascades.
		if l.spec.RollbackPolicy == rollout.RollbackHaltOnAnyFailure {
			l.cascade("halt-on-any-failure policy")
		} else {
			l.result.Status = rollout.ResultFailed
		}
		return true
	case rollout.OutcomeFailedNotRestored:
		nodeResult.Err = resp.ApplyErr
		l.result.Nodes = append(l.result.Nodes, nodeResult)
		l.cascade(fmt.Sprintf("node %s returned FailedNotRestored", node.Name()))
		return true
	case rollout.OutcomeUnspecified, rollout.OutcomeSkippedPreflight:
		// The operation forgot to set an Outcome (zero value) or a
		// post-preflight short-circuit leaked here.
		return l.handleUnexpectedOutcome(node, outcome)
	default:
		// A value outside the enum.
		return l.handleUnexpectedOutcome(node, outcome)
	}
}

// handleUnexpectedOutcome fail-closes a node whose Outcome is the zero
// value or otherwise not a recognized success/failure: silence can't
// masquerade as success. Records FailedNotRestored and cascades.
func (l *nodeLoop) handleUnexpectedOutcome(node rollout.Node, outcome rollout.Outcome) bool {
	l.result.Nodes = append(l.result.Nodes, rollout.NodeResult{
		NodeID:   node.ID(),
		NodeName: node.Name(),
		Outcome:  rollout.OutcomeFailedNotRestored,
		Err:      fmt.Sprintf("operation returned unexpected outcome %s (%d)", outcome, int(outcome)),
	})
	l.cascade(fmt.Sprintf("unexpected outcome %s on node %s", outcome, node.Name()))
	return true
}

// cascade runs the reverse-order cascade rollback over already-applied
// nodes and sets the terminal Status (RolledBack if it ran, else Failed).
func (l *nodeLoop) cascade(reason string) {
	rolledBack := cascadeRollback(l.ctx, l.op, l.caps.HasRollbackHandler, l.cluster, l.payload, l.applied, &l.result, reason)
	l.result.Status = statusForCascade(rolledBack)
}

// openClusterGuards dispatches each ClusterGuard.PreRollout in
// declaration order. On failure returns (opened-so-far, failResult);
// the caller MUST runClusterGuardPostRollout on the returned slice.
// The State envelope is recorded even for business failures so
// PostRollout can clean up any partial-state payload.
func openClusterGuards(
	ctx workflow.Context,
	cluster rollout.Cluster,
	refs []rollout.ClusterGuardRef,
) ([]openedClusterGuardRecord, *rollout.Result) {
	opened := make([]openedClusterGuardRecord, 0, len(refs))
	for _, ref := range refs {
		var resp ClusterGuardPreRolloutResponse
		if err := workflow.ExecuteActivity(ctx, ActivityClusterGuardPreRollout,
			&ClusterGuardPreRolloutRequest{
				GuardName: ref.Name,
				Params:    ref.Params,
				ClusterID: cluster.ID(),
			},
		).Get(ctx, &resp); err != nil {
			r := phaseFailedResult(
				rollout.PhaseClusterGuard, ref.Name,
				fmt.Errorf("cluster guard %q PreRollout dispatch: %w", ref.Name, err),
			)
			return opened, &r
		}
		opened = append(opened, openedClusterGuardRecord{Ref: ref, State: resp.State})
		if resp.PreRolloutErr != "" {
			r := phaseFailedResult(
				rollout.PhaseClusterGuard, ref.Name,
				errors.New(resp.PreRolloutErr),
			)
			return opened, &r
		}
	}
	return opened, nil
}

// runNodeGuardPostApply runs PostApply in reverse PreApply order for
// every opened guard, aggregating errors via errors.Join. Uses a
// disconnected context so a workflow cancel cannot strand an open
// service window.
func runNodeGuardPostApply(
	ctx workflow.Context,
	cluster rollout.Cluster,
	node rollout.Node,
	openedGuards []openedNodeGuardRecord,
	outcome rollout.Outcome,
) error {
	cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
	log := workflow.GetLogger(cleanupCtx)
	var errs []error
	for _, o := range slices.Backward(openedGuards) {
		if err := workflow.ExecuteActivity(cleanupCtx, ActivityNodeGuardPostApply,
			&NodeGuardPostApplyRequest{
				GuardName: o.Ref.Name,
				Params:    o.Ref.Params,
				ClusterID: cluster.ID(),
				NodeID:    node.ID(),
				Outcome:   outcome,
				State:     o.State,
			},
		).Get(cleanupCtx, nil); err != nil {
			log.Error("Node guard PostApply failed",
				"guard", o.Ref.Name, "node", node.Name(), "outcome", outcome.String(), "err", err)
			errs = append(errs, fmt.Errorf("node guard %q PostApply: %w", o.Ref.Name, err))
		}
		if o.CheckPostApplyRequired {
			if err := waitNodeGuardCheckPostApply(cleanupCtx, cluster, node, o.Ref, outcome, o.State); err != nil {
				log.Error("Node guard CheckPostApply failed",
					"guard", o.Ref.Name, "node", node.Name(), "outcome", outcome.String(), "err", err)
				errs = append(errs, fmt.Errorf("node guard %q CheckPostApply: %w", o.Ref.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func waitNodeGuardCheckPreApply(
	ctx workflow.Context,
	cluster rollout.Cluster,
	node rollout.Node,
	ref rollout.NodeGuardRef,
	state rollout.GuardState,
) error {
	for {
		var resp NodeGuardCheckPreApplyResponse
		if err := workflow.ExecuteActivity(ctx, ActivityNodeGuardCheckPreApply,
			&NodeGuardCheckPreApplyRequest{
				GuardName: ref.Name,
				Params:    ref.Params,
				ClusterID: cluster.ID(),
				NodeID:    node.ID(),
				State:     state,
			},
		).Get(ctx, &resp); err != nil {
			return fmt.Errorf("dispatch check pre-apply: %w", err)
		}
		if resp.CheckErr != "" {
			return errors.New(resp.CheckErr)
		}
		if resp.Done {
			return nil
		}
		if err := workflow.Sleep(ctx, defaultNodeGuardCheckPollInterval); err != nil {
			return err
		}
	}
}

func waitNodeGuardCheckPostApply(
	ctx workflow.Context,
	cluster rollout.Cluster,
	node rollout.Node,
	ref rollout.NodeGuardRef,
	outcome rollout.Outcome,
	state rollout.GuardState,
) error {
	for {
		var resp NodeGuardCheckPostApplyResponse
		if err := workflow.ExecuteActivity(ctx, ActivityNodeGuardCheckPostApply,
			&NodeGuardCheckPostApplyRequest{
				GuardName: ref.Name,
				Params:    ref.Params,
				ClusterID: cluster.ID(),
				NodeID:    node.ID(),
				Outcome:   outcome,
				State:     state,
			},
		).Get(ctx, &resp); err != nil {
			return fmt.Errorf("dispatch check post-apply: %w", err)
		}
		if resp.CheckErr != "" {
			return errors.New(resp.CheckErr)
		}
		if resp.Done {
			return nil
		}
		if err := workflow.Sleep(ctx, defaultNodeGuardCheckPollInterval); err != nil {
			return err
		}
	}
}

// runClusterGuardPostRollout runs PostRollout in reverse PreRollout
// order. Errors are swallowed per the ClusterGuard contract;
// disconnected context so a workflow cancel cannot abort cleanup.
// result is passed by value: this finalizer reads an immutable snapshot
// of the final Result and never mutates it.
func runClusterGuardPostRollout(
	ctx workflow.Context,
	cluster rollout.Cluster,
	openedGuards []openedClusterGuardRecord,
	result rollout.Result,
) {
	cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
	log := workflow.GetLogger(cleanupCtx)
	for _, o := range slices.Backward(openedGuards) {
		if err := workflow.ExecuteActivity(cleanupCtx, ActivityClusterGuardPostRollout,
			&ClusterGuardPostRolloutRequest{
				GuardName: o.Ref.Name,
				Params:    o.Ref.Params,
				ClusterID: cluster.ID(),
				Result:    result,
				State:     o.State,
			},
		).Get(cleanupCtx, nil); err != nil {
			log.Error("Cluster guard PostRollout failed",
				"guard", o.Ref.Name, "status", string(result.Status), "err", err)
		}
	}
}

// statusForCascade reports RolledBack only when every dispatched rollback
// succeeded.
func statusForCascade(rolledBack bool) rollout.ResultStatus {
	if rolledBack {
		return rollout.ResultRolledBack
	}
	return rollout.ResultFailed
}

// cascadeRollback runs RollbackNode in reverse apply order on a
// disconnected context. Individual errors land in Result.Nodes; the
// cascade always completes. Returns true only when every rollback succeeded.
func cascadeRollback(
	ctx workflow.Context,
	op rollout.CompiledOperation,
	hasRollback bool,
	cluster rollout.Cluster,
	payload rollout.ApplyToNodePayload,
	applied []appliedNodeRef,
	result *rollout.Result,
	reason string,
) bool {
	if !hasRollback || len(applied) == 0 {
		return false
	}
	cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
	log := workflow.GetLogger(cleanupCtx)
	log.Warn("Rollout: cascade rollback starting",
		"operation", op.Name(), "reason", reason, "nodeCount", len(applied))
	allSucceeded := true
	for _, a := range slices.Backward(applied) {
		if rerr := workflow.ExecuteActivity(cleanupCtx, ActivityRollbackNode,
			&RollbackRequest{
				OperationName: op.Name(),
				ClusterID:     cluster.ID(),
				NodeID:        a.ID,
				Payload:       payload,
			},
		).Get(cleanupCtx, nil); rerr != nil {
			allSucceeded = false
			log.Error("Rollout: rollback failed; continuing cascade",
				"operation", op.Name(), "node", a.Name, "err", rerr)
			result.Nodes = append(result.Nodes, rollout.NodeResult{
				NodeID:   a.ID,
				NodeName: a.Name,
				Outcome:  rollout.OutcomeFailedNotRestored,
				Err:      fmt.Sprintf("rollback: %v", rerr),
			})
		}
	}
	return allSucceeded
}
