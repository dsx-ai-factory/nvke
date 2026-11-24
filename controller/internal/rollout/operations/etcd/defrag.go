// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
)

const (
	// OperationNameDefragmentEtcd is the registry key for the defragmentation
	// rollout.
	OperationNameDefragmentEtcd = "etcd-defrag.defragment"
)

const (
	// defaultDefragPollInterval paces GetEtcdDefragStatus. The agent restarts
	// etcd and waits for the member to come back, which takes seconds rather
	// than milliseconds, so polling faster only adds RPCs.
	defaultDefragPollInterval = 5 * time.Second
	// defaultDefragTimeout bounds one node. The agent parks kube-apiserver,
	// transfers leadership, restarts etcd and waits for the member to rejoin;
	// past this the node is not coming back on its own and the rollout should
	// stop rather than keep a control-plane node out of service.
	defaultDefragTimeout = 10 * time.Minute
)

// DefragOperation defragments one control-plane etcd member per node by asking
// the node agent to restart it, which makes etcd defragment during bootstrap.
//
// Almost all of the hard work is the agent's: it probes voting members for
// quorum, transfers leadership away, takes the node out of service and puts it
// back, and refuses outright if any of that is unsafe. This operation decides
// which members are worth restarting, drives the asynchronous call, and
// translates the agent's verdict into a rollout outcome.
//
// There is deliberately no RollbackHandler. A defragmentation is not reversible
// and leaves nothing to undo: the member either came back smaller, came back the
// same size, or never came back, and only the last needs a human.
type DefragOperation struct {
	nodeAgentClientDialer nodeAgentDefragClientDialer
	pollInterval          time.Duration
	timeout               time.Duration
}

var (
	_ rollout.Operation                 = (*DefragOperation)(nil)
	_ rollout.ApplyToNodePayloadBuilder = (*DefragOperation)(nil)
	_ rollout.SatisfactionChecker       = (*DefragOperation)(nil)

	// errDefragNodeTimeout is the cause attached to this operation's own
	// per-node deadline. A context derived with a timeout also reports
	// DeadlineExceeded when the parent's deadline fires, so the cause is what
	// distinguishes the node overrunning its budget from the caller running
	// out of one.
	errDefragNodeTimeout = errors.New("etcd defragmentation node timeout")

	// errDefragDialerUnconfigured separates a programming error from a node
	// that cannot be reached, which are opposite verdicts: the first provably
	// touched nothing.
	errDefragDialerUnconfigured = errors.New("node agent dialer is not configured")
)

// NewDefragOperation builds the defragmentation operation.
func NewDefragOperation(opts ...DefragOption) *DefragOperation {
	op := &DefragOperation{pollInterval: defaultDefragPollInterval, timeout: defaultDefragTimeout}
	for _, opt := range opts {
		opt(op)
	}
	return op
}

func (op *DefragOperation) Name() string { return OperationNameDefragmentEtcd }

// defragArtifact is the per-rollout payload. It carries the idempotency base
// rather than a finished operation ID because one rollout touches several
// nodes and each needs its own: reusing a single ID would make the second node
// receive the first node's completed operation and report success without ever
// restarting etcd.
type defragArtifact struct {
	OperationIDBase string `json:"operationIdBase"`
}

// BuildApplyToNodePayload mints the idempotency base once per rollout. The
// framework delivers the payload byte-identical to every ApplyToNode, including
// across a Temporal replay, which is what makes a retried call resume the
// in-flight defragmentation instead of restarting etcd a second time.
func (op *DefragOperation) BuildApplyToNodePayload(_ context.Context, cluster rollout.Cluster) (rollout.ApplyToNodePayload, error) {
	if cluster == nil {
		return nil, errors.New("cluster is nil")
	}
	return json.Marshal(defragArtifact{OperationIDBase: uuid.NewString()})
}

// IsSatisfied reports whether no control-plane member has enough reclaimable
// space to be worth restarting, which short-circuits the whole rollout before
// any node is touched.
//
// A member the agent would refuse counts as satisfied rather than as an error.
// Sitting below the bootstrap-defrag threshold is the normal steady state, not
// a fault, and a cluster made entirely of such members has nothing to do.
func (op *DefragOperation) IsSatisfied(ctx context.Context, cluster rollout.Cluster) (bool, error) {
	if cluster == nil {
		return false, errors.New("cluster is nil")
	}
	for _, node := range cluster.ControlPlaneNodes() {
		// The closure scopes the connection to this iteration; deferring in the
		// loop body would hold every node's open until the whole walk finished.
		worth, err := func() (bool, error) {
			client, cleanup, err := op.dial(ctx, node)
			if err != nil {
				return false, err
			}
			if cleanup != nil {
				defer cleanup()
			}
			worth, _, err := op.worthDefragmenting(ctx, client, node)
			return worth, err
		}()
		if err != nil {
			return false, err
		}
		if worth {
			return false, nil
		}
	}
	return true, nil
}

// ApplyToNode defragments this node's etcd member, resuming an attempt already
// in flight rather than starting a second one.
func (op *DefragOperation) ApplyToNode(ctx context.Context, node rollout.Node, payload rollout.ApplyToNodePayload) (rollout.Outcome, error) {
	if node == nil {
		return rollout.OutcomeFailedRestored, errors.New("node is nil")
	}
	if node.Role() != rollout.NodeRoleControlPlane {
		return rollout.OutcomeFailedRestored, fmt.Errorf("node %s has role %q; etcd defragmentation only supports control-plane nodes", node.Name(), node.Role())
	}
	artifact, err := parseDefragPayload(payload)
	if err != nil {
		return rollout.OutcomeFailedRestored, err
	}

	// The budget covers the dial and every agent call, not only the poll loop.
	// Nothing in the dialer contract imposes a deadline of its own, so a call
	// that hangs before the loop would otherwise run until the caller gave up
	// and never reach the fail-closed verdict the timeout exists to produce.
	ctx, cancel := context.WithTimeoutCause(ctx, op.timeout, errDefragNodeTimeout)
	defer cancel()

	client, cleanup, err := op.dial(ctx, node)
	if err != nil {
		// An unreachable agent is unknown node state. The dial precedes the
		// resolve, so nothing here can tell a first attempt from a retry that
		// has the member down already, and the agent listens independently of
		// etcd and kube-apiserver, so failing to reach it is not what a node
		// mid-defragmentation looks like. A misconfigured dialer is the one
		// case that provably emitted nothing.
		if errors.Is(err, errDefragDialerUnconfigured) {
			return rollout.OutcomeFailedRestored, err
		}
		return rollout.OutcomeFailedNotRestored, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Resolve before deciding. The operation ID is derived from a payload the
	// framework delivers byte-identical to every retry, so an existing record
	// under it means an earlier attempt already started this node and the
	// member may be down right now. Checking eligibility first would read that
	// as an unreachable member and report a node that is out of service as
	// restored.
	operationID := artifact.OperationIDBase + "/" + node.Name()
	resp, probeErr := client.GetEtcdDefragStatus(ctx, &nvkeproto.GetEtcdDefragStatusRequest{OperationId: operationID})
	switch {
	case probeErr == nil && isTerminalPhase(resp.GetOperation().GetPhase()):
		// The verdict is already in hand. Polling for it again would discard
		// it, and polls failing from there to the deadline would report
		// unknown state over an answer the agent has already given.
		return defragOutcome(ctx, resp, node)
	case probeErr == nil:
		slog.InfoContext(ctx, "resuming an etcd defragmentation already recorded by the agent", "node", node.Name(), "operationID", operationID)
		return op.awaitDefrag(ctx, client, node, operationID)
	case status.Code(probeErr) == codes.FailedPrecondition:
		// An agent restart left the record non-terminal with no worker behind
		// it. The agent put back whatever that attempt parked before it began
		// serving, so the node is already restored, and StartEtcdDefrag is the
		// only thing that clears the stale record. Falling through to start
		// again is the recovery; returning here would wedge the node, because
		// every retry would be answered the same way.
		slog.WarnContext(ctx, "an earlier etcd defragmentation was interrupted by an agent restart; starting again",
			"node", node.Name(), "operationID", operationID, "err", probeErr)
	case status.Code(probeErr) != codes.NotFound:
		return rollout.OutcomeFailedNotRestored, fmt.Errorf("read etcd defragmentation state for node %s: %w", node.Name(), probeErr)
	}

	// Ask before telling. The agent refuses an unhealthy member, a cluster too
	// small to lose one, and a member below its threshold through the single
	// OUTCOME_REFUSED, so a refusal alone cannot say whether the rollout should
	// shrug or stop. Reading the status first separates them: a member that is
	// simply not worth restarting never reaches StartEtcdDefrag, which leaves a
	// refusal to mean a genuine precondition failure.
	worth, reason, err := op.worthDefragmenting(ctx, client, node)
	if err != nil {
		return rollout.OutcomeFailedRestored, err
	}
	if !worth {
		slog.InfoContext(ctx, "skipping etcd defragmentation", "node", node.Name(), "reason", reason)
		return rollout.OutcomeUnchanged, nil
	}
	return op.startDefrag(ctx, client, node, operationID)
}

// startDefrag asks the agent to begin and follows the result, retrying the
// start once when the first call fails ambiguously.
//
// The retry is not a transport retry. StartEtcdDefrag never observes its RPC
// context, so a client-side failure does not stop the handler, and a status
// probe can be answered NotFound by an agent whose handler is still on its way
// to recording the operation. A second start cannot be: the agent serializes
// starts on a single mutex and returns the existing operation for an ID it has
// already published, so the retry blocks behind any in-flight handler and
// observes whatever that handler did.
func (op *DefragOperation) startDefrag(
	ctx context.Context,
	client nodeAgentDefragClient,
	node rollout.Node,
	operationID string,
) (rollout.Outcome, error) {
	_, err := client.StartEtcdDefrag(ctx, &nvkeproto.StartEtcdDefragRequest{OperationId: operationID})
	switch {
	case err == nil:
		return op.awaitDefrag(ctx, client, node, operationID)
	case agentDeclined(err):
		return rollout.OutcomeFailedRestored, fmt.Errorf("start etcd defragmentation on node %s: %w", node.Name(), err)
	}

	slog.WarnContext(ctx, "start etcd defragmentation failed ambiguously, retrying to establish whether the agent acted",
		"node", node.Name(), "err", err)

	_, retryErr := client.StartEtcdDefrag(ctx, &nvkeproto.StartEtcdDefragRequest{OperationId: operationID})
	switch {
	case retryErr == nil:
		return op.awaitDefrag(ctx, client, node, operationID)
	case agentDeclined(retryErr):
		return rollout.OutcomeFailedRestored, fmt.Errorf("start etcd defragmentation on node %s: %w", node.Name(), retryErr)
	default:
		return rollout.OutcomeFailedNotRestored, fmt.Errorf("start etcd defragmentation on node %s failed twice and its state could not be established (first attempt: %w): %w", node.Name(), err, retryErr)
	}
}

// agentDeclined reports whether err is a refusal the agent itself produced,
// which proves its handler ran and returned before taking the node out of
// service.
//
// gRPC-Go synthesizes Canceled, DeadlineExceeded, Unavailable and Internal on
// the client side, so those cannot distinguish a rejected request from one that
// never arrived. InvalidArgument and FailedPrecondition have no client-side
// source for a unary call: only the handler returns them.
func agentDeclined(err error) bool {
	code := status.Code(err)
	return code == codes.InvalidArgument || code == codes.FailedPrecondition
}

// awaitDefrag polls until the agent reports a terminal phase, then maps its
// verdict onto a rollout outcome. The caller owns the deadline.
func (op *DefragOperation) awaitDefrag(
	ctx context.Context,
	client nodeAgentDefragClient,
	node rollout.Node,
	operationID string,
) (rollout.Outcome, error) {
	ticker := time.NewTicker(op.pollInterval)
	defer ticker.Stop()

	for {
		resp, err := client.GetEtcdDefragStatus(ctx, &nvkeproto.GetEtcdDefragStatusRequest{OperationId: operationID})
		switch {
		case err != nil:
			slog.WarnContext(ctx, "etcd defragmentation status poll failed, retrying", "node", node.Name(), "err", err)
		// Only SUCCEEDED and FAILED are terminal. An unset phase means the
		// agent has not recorded this operation yet, which is indistinguishable
		// from a status field the agent did not populate at all; polling on is
		// correct for both, and the timeout is the backstop.
		case isTerminalPhase(resp.GetOperation().GetPhase()):
			return defragOutcome(ctx, resp, node)
		}

		select {
		case <-ctx.Done():
			// etcd may be down with kube-apiserver parked behind it, and this
			// operation can no longer tell. Cascading is the fail-closed read
			// whether the budget ran out or the caller gave up.
			if !errors.Is(context.Cause(ctx), errDefragNodeTimeout) {
				return rollout.OutcomeFailedNotRestored, fmt.Errorf("etcd defragmentation on node %s was interrupted: %w", node.Name(), context.Cause(ctx))
			}
			return rollout.OutcomeFailedNotRestored, fmt.Errorf("etcd defragmentation on node %s did not finish within %s: %w", node.Name(), op.timeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

// isTerminalPhase reports whether the agent has finished with an operation.
// UNSPECIFIED is deliberately not terminal: treating it as such would read a
// nil or not-yet-written status as a verdict.
func isTerminalPhase(phase nvkeproto.NodeAgentOperationPhase) bool {
	return phase == nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED ||
		phase == nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED
}

// defragOutcome translates the agent's outcome. The mapping is deliberately
// total: an unrecognized value is treated as unknown node state rather than
// quietly passing, because the alternative is reporting success for a member
// whose etcd may still be down.
func defragOutcome(ctx context.Context, resp *nvkeproto.GetEtcdDefragStatusResponse, node rollout.Node) (rollout.Outcome, error) {
	switch resp.GetOutcome() {
	case nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED:
		slog.InfoContext(ctx, "etcd defragmented",
			"node", node.Name(),
			"reclaimedBytes", resp.GetReclaimedBytes(),
			"dbSizeBytesBefore", resp.GetDbTotalSizeBytesBefore(),
			"dbSizeBytesAfter", resp.GetDbTotalSizeBytesAfter(),
		)
		return rollout.OutcomeApplied, nil

	case nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_NOT_RECLAIMED:
		// The member restarted and came back healthy having reclaimed nothing,
		// so it is mostly live data. Surfacing this as Unchanged rather than a
		// failure matters: it keeps the member counted toward the minimum
		// healthy replicas, and looping would not shrink it.
		slog.InfoContext(ctx, "etcd defragmentation reclaimed nothing", "node", node.Name(), "dbSizeBytes", resp.GetDbTotalSizeBytesAfter())
		return rollout.OutcomeUnchanged, nil

	case nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_REFUSED:
		// The pre-check already filtered the benign refusals, so reaching here
		// means the cluster changed underneath us: a member went unhealthy, or
		// too few voters answered to survive this one restarting.
		return rollout.OutcomeFailedRestored, fmt.Errorf("agent refused etcd defragmentation on node %s: %s", node.Name(), resp.GetErrorMessage())

	case nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_FAILED_RESTORED:
		return rollout.OutcomeFailedRestored, fmt.Errorf("etcd defragmentation on node %s failed and the node was restored: %s", node.Name(), resp.GetErrorMessage())

	case nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED:
		return rollout.OutcomeFailedNotRestored, fmt.Errorf("etcd defragmentation on node %s failed and the node is out of service: %s", node.Name(), resp.GetErrorMessage())

	case nvkeproto.GetEtcdDefragStatusResponse_OUTCOME_UNSPECIFIED:
		// Not a protocol violation: the agent leaves the outcome unset when a
		// run failed before reaching a typed verdict. That is precisely the
		// case where it cannot say whether the node was put back.
		return rollout.OutcomeFailedNotRestored, fmt.Errorf("agent reported no etcd defragmentation outcome for node %s: %s", node.Name(), resp.GetErrorMessage())

	default:
		return rollout.OutcomeFailedNotRestored, fmt.Errorf("agent reported unknown etcd defragmentation outcome %d for node %s", resp.GetOutcome(), node.Name())
	}
}

// worthDefragmenting reports whether restarting this member would reclaim
// anything, and why not when it would not. It answers from GetEtcdStatus alone
// and never restarts anything. The caller owns the connection, so one node
// costs one dial.
func (op *DefragOperation) worthDefragmenting(
	ctx context.Context,
	client nodeAgentDefragClient,
	node rollout.Node,
) (bool, string, error) {
	status, err := client.GetEtcdStatus(ctx, &nvkeproto.GetEtcdStatusRequest{})
	if err != nil {
		return false, "", fmt.Errorf("get etcd status from node %s: %w", node.Name(), err)
	}

	// Zero is the agent's way of saying this member was provisioned before the
	// threshold was configured. etcd treats the flag as disabled, so a restart
	// would be downtime that reclaims nothing however fragmented the file is.
	threshold := status.GetBootstrapDefragThresholdBytes()
	if threshold <= 0 {
		return false, "member has no bootstrap-defrag threshold configured", nil
	}
	if reclaimable := status.GetReclaimableBytes(); reclaimable < threshold {
		return false, fmt.Sprintf("reclaimable %d bytes is below the member's %d byte threshold", reclaimable, threshold), nil
	}
	return true, "", nil
}

func (op *DefragOperation) dial(ctx context.Context, node rollout.Node) (nodeAgentDefragClient, func(), error) {
	if op.nodeAgentClientDialer == nil {
		return nil, nil, errDefragDialerUnconfigured
	}
	client, cleanup, err := op.nodeAgentClientDialer(ctx, node)
	if err != nil {
		return nil, nil, fmt.Errorf("dial agent on node %s: %w", node.Name(), err)
	}
	if client == nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("dial agent on node %s returned nil client", node.Name())
	}
	return client, cleanup, nil
}

func parseDefragPayload(payload rollout.ApplyToNodePayload) (defragArtifact, error) {
	if len(payload) == 0 {
		return defragArtifact{}, errors.New("etcd defragmentation payload is empty")
	}
	artifact := defragArtifact{}
	if err := json.Unmarshal(payload, &artifact); err != nil {
		return defragArtifact{}, fmt.Errorf("parse etcd defragmentation payload: %w", err)
	}
	if artifact.OperationIDBase == "" {
		return defragArtifact{}, errors.New("etcd defragmentation payload is missing operationIdBase")
	}
	return artifact, nil
}
