// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package temporal is the production rollout executor: each step
// dispatches as a Temporal activity. Entry point: Execute.
package temporal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	temporalactivity "go.temporal.io/sdk/activity"
	sdktemporal "go.temporal.io/sdk/temporal"

	"github.com/NVIDIA/nke/controller/internal/rollout"
)

// heartbeatInterval is how often the long agent activities (ApplyToNode,
// RollbackNode) report progress. It stays well under the HeartbeatTimeout set by
// WithRolloutNodeAgentApplyActivityOptions (2m) so a live worker is never
// mistaken for a dead one.
const (
	heartbeatInterval = 30 * time.Second
)

// startHeartbeat records an activity heartbeat every heartbeatInterval until the
// returned stop func is called (defer it). It lets Temporal detect a crashed or
// lost worker within one HeartbeatTimeout window instead of waiting out the full
// StartToCloseTimeout, so the workflow retries the node on a fresh worker
// quickly. Node-agent apply/rollback is idempotent, so that retry is safe. When
// the activity has no HeartbeatTimeout configured the heartbeats are harmless.
func startHeartbeat(ctx context.Context, detail string) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				temporalactivity.RecordHeartbeat(ctx, detail)
			}
		}
	}()
	return func() { close(done) }
}

// Loader resolves a cluster UUID to a rollout.Cluster from inside an
// activity.
type Loader interface {
	LoadCluster(ctx context.Context, clusterID uuid.UUID) (rollout.Cluster, error)
}

// Operator-facing error codes carried in Temporal ApplicationError.Type.
const (
	errCodeOperationNotRegistered = "OperationNotRegistered"
	errCodePreflightNotRegistered = "PreflightNotRegistered"
	errCodeGuardNotRegistered     = "GuardNotRegistered"
	errCodeGuardConfigError       = "GuardConfigError"
	errCodeNodeNotInCluster       = "NodeNotInCluster"
	errCodeRolloutCleanupFailed   = "RolloutCleanupFailed"
	errCodeRolloutGateFailed      = "RolloutGateFailed"
)

// Domain sentinels for operator-fault resolution failures. The resolve /
// lookup / findNode helpers return these raw (SDK-free); the activity
// boundary classifies them via toTemporalActivityError. Keeping the
// sentinels out of the helpers lets each activity body show its own retry
// semantics without anyone having to read into resolve to find out.
var (
	errOperationNotRegistered = errors.New("operation not registered")
	errPreflightNotRegistered = errors.New("preflight not registered")
	errNodeNotInCluster       = errors.New("node not in cluster")
)

// nonRetryable wraps err as a Temporal non-retryable application
// error. Passes nil through.
func nonRetryable(err error, errCode string) error {
	if err == nil {
		return nil
	}
	return sdktemporal.NewNonRetryableApplicationError(err.Error(), errCode, err)
}

// nonRetryableCleanupError wraps cleanup-mutation errors (RollbackNode,
// NodeGuardPostApply, ClusterGuardPostRollout). Retry is HARMFUL: a
// retried teardown can act on a partially-restored node; a retried
// PostApply can double-close a service window or re-lift a silence.
func nonRetryableCleanupError(err error) error {
	return nonRetryable(err, errCodeRolloutCleanupFailed)
}

// nonRetryableGateError wraps deterministic-gate errors (Validate,
// CheckPostCondition, ClusterPreflight, NodePreflight). Retry is
// WASTEFUL: the gate re-probes the same state for the same answer.
func nonRetryableGateError(err error) error {
	return nonRetryable(err, errCodeRolloutGateFailed)
}

// toTemporalActivityError classifies a resolution/lookup error at the
// activity boundary: operator faults — an unregistered operation,
// preflight, or guard; an unknown node; a guard factory failure — map to
// non-retryable application errors with a stable operator-facing code.
// Anything else (e.g. a postgres LoadCluster failure) falls through
// unchanged and stays retryable under the activity's retry policy. nil
// passes through. The guard sentinels (rollout.ErrGuardConfig /
// rollout.ErrNotRegistered) come from the Registry; only guard resolution
// reaches them, since operation/preflight misses carry their own sentinel.
func toTemporalActivityError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errOperationNotRegistered):
		return nonRetryable(err, errCodeOperationNotRegistered)
	case errors.Is(err, errPreflightNotRegistered):
		return nonRetryable(err, errCodePreflightNotRegistered)
	case errors.Is(err, errNodeNotInCluster):
		return nonRetryable(err, errCodeNodeNotInCluster)
	case errors.Is(err, rollout.ErrGuardConfig):
		return nonRetryable(err, errCodeGuardConfigError)
	case errors.Is(err, rollout.ErrNotRegistered):
		return nonRetryable(err, errCodeGuardNotRegistered)
	default:
		return err
	}
}

// Registerer is the activity-registration subset of worker.Worker.
type Registerer interface {
	RegisterActivityWithOptions(a interface{}, options temporalactivity.RegisterOptions)
}

// Activities resolves the workflow's names+IDs into a concrete
// Operation+Cluster on the activity side via the injected Loader, and
// resolves operations/preflights/guards by name via the injected
// Registry. Both are supplied at construction so two workers never
// share registration state.
type Activities struct {
	loader   Loader
	registry *rollout.Registry
}

func NewActivities(loader Loader, registry *rollout.Registry) *Activities {
	return &Activities{loader: loader, registry: registry}
}

// Register registers every activity under the matching Activity* name.
func (a *Activities) Register(r Registerer) {
	r.RegisterActivityWithOptions(a.IsSatisfied, temporalactivity.RegisterOptions{Name: ActivityIsSatisfied})
	r.RegisterActivityWithOptions(a.Validate, temporalactivity.RegisterOptions{Name: ActivityValidate})
	r.RegisterActivityWithOptions(a.BuildApplyToNodePayload, temporalactivity.RegisterOptions{Name: ActivityBuildApplyToNodePayload})
	r.RegisterActivityWithOptions(a.ApplyToNode, temporalactivity.RegisterOptions{Name: ActivityApplyToNode})
	r.RegisterActivityWithOptions(a.RollbackNode, temporalactivity.RegisterOptions{Name: ActivityRollbackNode})
	r.RegisterActivityWithOptions(a.CheckPostCondition, temporalactivity.RegisterOptions{Name: ActivityCheckPostCond})
	r.RegisterActivityWithOptions(a.NodeGuardPreApply, temporalactivity.RegisterOptions{Name: ActivityNodeGuardPreApply})
	r.RegisterActivityWithOptions(a.NodeGuardCheckPreApply, temporalactivity.RegisterOptions{Name: ActivityNodeGuardCheckPreApply})
	r.RegisterActivityWithOptions(a.NodeGuardPostApply, temporalactivity.RegisterOptions{Name: ActivityNodeGuardPostApply})
	r.RegisterActivityWithOptions(a.NodeGuardCheckPostApply, temporalactivity.RegisterOptions{Name: ActivityNodeGuardCheckPostApply})
	r.RegisterActivityWithOptions(a.ClusterGuardPreRollout, temporalactivity.RegisterOptions{Name: ActivityClusterGuardPreRollout})
	r.RegisterActivityWithOptions(a.ClusterGuardPostRollout, temporalactivity.RegisterOptions{Name: ActivityClusterGuardPostRollout})
	r.RegisterActivityWithOptions(a.ClusterPreflight, temporalactivity.RegisterOptions{Name: ActivityClusterPreflight})
	r.RegisterActivityWithOptions(a.RunNodePreflight, temporalactivity.RegisterOptions{Name: ActivityNodePreflight})
}

// IsSatisfied packs business failures into resp.IsSatErr — see types.go.
func (a *Activities) IsSatisfied(ctx context.Context, req *IsSatisfiedRequest) (*IsSatisfiedResponse, error) {
	op, cluster, err := a.resolve(ctx, req.OperationName, req.ClusterID)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	satisfied, satErr := op.IsSatisfied(ctx, cluster)
	resp := &IsSatisfiedResponse{Satisfied: satisfied}
	if satErr != nil {
		resp.IsSatErr = fmt.Sprintf("is_satisfied: %v", satErr)
	}
	return resp, nil
}

func (a *Activities) Validate(ctx context.Context, req *ValidateRequest) error {
	op, cluster, err := a.resolve(ctx, req.OperationName, req.ClusterID)
	if err != nil {
		return toTemporalActivityError(err)
	}
	if err := op.Validate(ctx, cluster); err != nil {
		return nonRetryableGateError(fmt.Errorf("validate: %w", err))
	}
	return nil
}

func (a *Activities) BuildApplyToNodePayload(ctx context.Context, req *BuildApplyToNodePayloadRequest) (*BuildApplyToNodePayloadResponse, error) {
	op, cluster, err := a.resolve(ctx, req.OperationName, req.ClusterID)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	art, buildErr := op.BuildApplyToNodePayload(ctx, cluster)
	if buildErr != nil {
		return &BuildApplyToNodePayloadResponse{
			BuildErr: fmt.Sprintf("build_apply_to_node_payload: %v", buildErr),
		}, nil
	}
	return &BuildApplyToNodePayloadResponse{Payload: art}, nil
}

func (a *Activities) ApplyToNode(ctx context.Context, req *ApplyToNodeRequest) (*ApplyToNodeResponse, error) {
	op, cluster, err := a.resolve(ctx, req.OperationName, req.ClusterID)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	node, err := findNode(cluster, req.NodeID)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	// This RPC blocks for minutes on the agent's readiness wait; heartbeat
	// so a crashed worker is retried within HeartbeatTimeout, not StartToClose.
	stopHeartbeat := startHeartbeat(ctx, "apply-to-node in progress")
	defer stopHeartbeat()
	outcome, applyErr := op.ApplyToNode(ctx, node, req.Payload)
	resp := &ApplyToNodeResponse{Outcome: outcome}
	if applyErr != nil {
		resp.ApplyErr = applyErr.Error()
	}
	return resp, nil
}

func (a *Activities) RollbackNode(ctx context.Context, req *RollbackRequest) error {
	op, cluster, err := a.resolve(ctx, req.OperationName, req.ClusterID)
	if err != nil {
		return toTemporalActivityError(err)
	}
	node, err := findNode(cluster, req.NodeID)
	if err != nil {
		return toTemporalActivityError(err)
	}
	// Rollback dials the agent Disable RPC, which likewise blocks on the
	// readiness wait; heartbeat for the same fast-crash-recovery reason.
	stopHeartbeat := startHeartbeat(ctx, "rollback-node in progress")
	defer stopHeartbeat()
	if err := op.Rollback(ctx, node, req.Payload); err != nil {
		return nonRetryableCleanupError(fmt.Errorf("rollback: %w", err))
	}
	return nil
}

func (a *Activities) CheckPostCondition(ctx context.Context, req *CheckPostConditionRequest) error {
	op, cluster, err := a.resolve(ctx, req.OperationName, req.ClusterID)
	if err != nil {
		return toTemporalActivityError(err)
	}
	if err := op.CheckPostCondition(ctx, cluster); err != nil {
		return nonRetryableGateError(fmt.Errorf("check_post_condition: %w", err))
	}
	return nil
}

func (a *Activities) ClusterPreflight(ctx context.Context, req *ClusterPreflightRequest) error {
	fn, ok := a.registry.LookupClusterPreflight(req.PreflightName)
	if !ok {
		return toTemporalActivityError(
			fmt.Errorf("cluster preflight %q: %w", req.PreflightName, errPreflightNotRegistered),
		)
	}
	cluster, err := a.loadCluster(ctx, req.ClusterID)
	if err != nil {
		return toTemporalActivityError(err)
	}
	return nonRetryableGateError(fn(ctx, cluster, req.Params))
}

func (a *Activities) RunNodePreflight(ctx context.Context, req *NodePreflightRequest) error {
	fn, ok := a.registry.LookupNodePreflight(req.PreflightName)
	if !ok {
		return toTemporalActivityError(
			fmt.Errorf("node preflight %q: %w", req.PreflightName, errPreflightNotRegistered),
		)
	}
	cluster, err := a.loadCluster(ctx, req.ClusterID)
	if err != nil {
		return toTemporalActivityError(err)
	}
	node, err := findNode(cluster, req.NodeID)
	if err != nil {
		return toTemporalActivityError(err)
	}
	return nonRetryableGateError(fn(ctx, cluster, node, req.Params))
}

// resolve loads the operation and cluster, returning raw errors: an
// unregistered operation wraps errOperationNotRegistered, a load failure
// stays a plain (retryable) error. The caller classifies at the boundary
// via toTemporalActivityError.
func (a *Activities) resolve(
	ctx context.Context, opName string, clusterID uuid.UUID,
) (rollout.CompiledOperation, rollout.Cluster, error) {
	op, ok := a.registry.LookupOperation(opName)
	if !ok {
		return rollout.CompiledOperation{}, nil, fmt.Errorf("operation %q: %w", opName, errOperationNotRegistered)
	}
	cluster, err := a.loader.LoadCluster(ctx, clusterID)
	if err != nil {
		return rollout.CompiledOperation{}, nil, fmt.Errorf("load cluster %s: %w", clusterID, err)
	}
	return op, cluster, nil
}

// loadCluster is the cluster-only sibling of resolve. The error is a
// plain (retryable) error; classify via toTemporalActivityError.
func (a *Activities) loadCluster(ctx context.Context, clusterID uuid.UUID) (rollout.Cluster, error) {
	cluster, err := a.loader.LoadCluster(ctx, clusterID)
	if err != nil {
		return nil, fmt.Errorf("load cluster %s: %w", clusterID, err)
	}
	return cluster, nil
}

func (a *Activities) NodeGuardPreApply(ctx context.Context, req *NodeGuardPreApplyRequest) (*NodeGuardPreApplyResponse, error) {
	g, err := a.registry.ResolveNodeGuard(req.GuardName, req.Params)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	cluster, err := a.loadCluster(ctx, req.ClusterID)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	node, err := findNode(cluster, req.NodeID)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	state, preErr := g.PreApply(ctx, cluster, node)
	_, checkPreApply := g.(rollout.NodeGuardPreApplyChecker)
	_, checkPostApply := g.(rollout.NodeGuardPostApplyChecker)
	resp := &NodeGuardPreApplyResponse{
		State:                  state,
		CheckPreApplyRequired:  checkPreApply,
		CheckPostApplyRequired: checkPostApply,
	}
	if preErr != nil {
		resp.PreApplyErr = fmt.Sprintf("node guard %q PreApply: %v", req.GuardName, preErr)
	}
	return resp, nil
}

func (a *Activities) NodeGuardCheckPreApply(ctx context.Context, req *NodeGuardCheckPreApplyRequest) (*NodeGuardCheckPreApplyResponse, error) {
	if req.State.GuardName == "" {
		return nil, nonRetryableGateError(fmt.Errorf("node guard CheckPreApply: empty State.GuardName (guard %q must use rollout.StatelessGuardState or return a non-empty envelope)",
			req.GuardName))
	}
	if req.State.GuardName != req.GuardName {
		return nil, nonRetryableGateError(fmt.Errorf("node guard CheckPreApply: state belongs to guard %q but request targets %q",
			req.State.GuardName, req.GuardName))
	}
	g, err := a.registry.ResolveNodeGuard(req.GuardName, req.Params)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	checker, ok := g.(rollout.NodeGuardPreApplyChecker)
	if !ok {
		return &NodeGuardCheckPreApplyResponse{Done: true}, nil
	}
	cluster, err := a.loadCluster(ctx, req.ClusterID)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	node, err := findNode(cluster, req.NodeID)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	done, checkErr := checker.CheckPreApply(ctx, cluster, node, req.State)
	resp := &NodeGuardCheckPreApplyResponse{Done: done}
	if checkErr != nil {
		resp.CheckErr = fmt.Sprintf("node guard %q CheckPreApply: %v", req.GuardName, checkErr)
	}
	return resp, nil
}

// NodeGuardPostApply asserts State.GuardName matches the request so a
// mis-routed envelope blows up loudly rather than handing the wrong
// silence ID to the wrong guard. Stateless guards must use
// rollout.StatelessGuardState.
func (a *Activities) NodeGuardPostApply(ctx context.Context, req *NodeGuardPostApplyRequest) error {
	if req.State.GuardName == "" {
		return nonRetryableCleanupError(fmt.Errorf("node guard PostApply: empty State.GuardName (guard %q must use rollout.StatelessGuardState or return a non-empty envelope)",
			req.GuardName))
	}
	if req.State.GuardName != req.GuardName {
		return nonRetryableCleanupError(fmt.Errorf("node guard PostApply: state belongs to guard %q but request targets %q",
			req.State.GuardName, req.GuardName))
	}
	g, err := a.registry.ResolveNodeGuard(req.GuardName, req.Params)
	if err != nil {
		return toTemporalActivityError(err)
	}
	cluster, err := a.loadCluster(ctx, req.ClusterID)
	if err != nil {
		return toTemporalActivityError(err)
	}
	node, err := findNode(cluster, req.NodeID)
	if err != nil {
		return toTemporalActivityError(err)
	}
	if err := g.PostApply(ctx, cluster, node, req.Outcome, req.State); err != nil {
		return nonRetryableCleanupError(fmt.Errorf("node guard %q PostApply: %w", req.GuardName, err))
	}
	return nil
}

func (a *Activities) NodeGuardCheckPostApply(ctx context.Context, req *NodeGuardCheckPostApplyRequest) (*NodeGuardCheckPostApplyResponse, error) {
	if req.State.GuardName == "" {
		return nil, nonRetryableCleanupError(fmt.Errorf("node guard CheckPostApply: empty State.GuardName (guard %q must use rollout.StatelessGuardState or return a non-empty envelope)",
			req.GuardName))
	}
	if req.State.GuardName != req.GuardName {
		return nil, nonRetryableCleanupError(fmt.Errorf("node guard CheckPostApply: state belongs to guard %q but request targets %q",
			req.State.GuardName, req.GuardName))
	}
	g, err := a.registry.ResolveNodeGuard(req.GuardName, req.Params)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	checker, ok := g.(rollout.NodeGuardPostApplyChecker)
	if !ok {
		return &NodeGuardCheckPostApplyResponse{Done: true}, nil
	}
	cluster, err := a.loadCluster(ctx, req.ClusterID)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	node, err := findNode(cluster, req.NodeID)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	done, checkErr := checker.CheckPostApply(ctx, cluster, node, req.Outcome, req.State)
	resp := &NodeGuardCheckPostApplyResponse{Done: done}
	if checkErr != nil {
		resp.CheckErr = fmt.Sprintf("node guard %q CheckPostApply: %v", req.GuardName, checkErr)
	}
	return resp, nil
}

func (a *Activities) ClusterGuardPreRollout(ctx context.Context, req *ClusterGuardPreRolloutRequest) (*ClusterGuardPreRolloutResponse, error) {
	g, err := a.registry.ResolveClusterGuard(req.GuardName, req.Params)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	cluster, err := a.loadCluster(ctx, req.ClusterID)
	if err != nil {
		return nil, toTemporalActivityError(err)
	}
	state, preErr := g.PreRollout(ctx, cluster)
	resp := &ClusterGuardPreRolloutResponse{State: state}
	if preErr != nil {
		resp.PreRolloutErr = fmt.Sprintf("cluster guard %q PreRollout: %v", req.GuardName, preErr)
	}
	return resp, nil
}

// ClusterGuardPostRollout asserts State.GuardName matches the request;
// see NodeGuardPostApply for the contract rationale.
func (a *Activities) ClusterGuardPostRollout(ctx context.Context, req *ClusterGuardPostRolloutRequest) error {
	if req.State.GuardName == "" {
		return nonRetryableCleanupError(fmt.Errorf("cluster guard PostRollout: empty State.GuardName (guard %q must use rollout.StatelessGuardState or return a non-empty envelope)",
			req.GuardName))
	}
	if req.State.GuardName != req.GuardName {
		return nonRetryableCleanupError(fmt.Errorf("cluster guard PostRollout: state belongs to guard %q but request targets %q",
			req.State.GuardName, req.GuardName))
	}
	g, err := a.registry.ResolveClusterGuard(req.GuardName, req.Params)
	if err != nil {
		return toTemporalActivityError(err)
	}
	cluster, err := a.loadCluster(ctx, req.ClusterID)
	if err != nil {
		return toTemporalActivityError(err)
	}
	if err := g.PostRollout(ctx, cluster, req.Result, req.State); err != nil {
		return nonRetryableCleanupError(fmt.Errorf("cluster guard %q PostRollout: %w", req.GuardName, err))
	}
	return nil
}

// findNode returns the node with nodeID, or a raw error wrapping
// errNodeNotInCluster (classify via toTemporalActivityError).
func findNode(cluster rollout.Cluster, nodeID uuid.UUID) (rollout.Node, error) {
	for _, n := range cluster.Nodes() {
		if n.ID() == nodeID {
			return n, nil
		}
	}
	return nil, fmt.Errorf("node %s not found in cluster %s: %w", nodeID, cluster.ID(), errNodeNotInCluster)
}
