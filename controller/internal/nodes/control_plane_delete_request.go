// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// MinFaultTolerantControlPlaneNodes is the smallest control plane that still
// tolerates losing a node. Removing a member from a three-node etcd cluster
// leaves two, where quorum is also two, so the next failure stops the cluster.
// A control-plane delete that would leave fewer than this is refused; growing
// the control plane first (replacement-first surge) is the supported way to
// rotate a node out of a three-node control plane.
const (
	MinFaultTolerantControlPlaneNodes = 3
)

var (
	// ErrDeleteAlreadyRequested is returned when a delete has already been
	// accepted for the node but its teardown has not started yet; callers should
	// treat the request as a no-op success.
	ErrDeleteAlreadyRequested = errors.New("node delete already requested")
	// ErrNoDeleteRequested is returned when a teardown is promoted for a node
	// that carries no recorded delete request.
	ErrNoDeleteRequested = errors.New("node has no recorded delete request")
)

// deleteRequestRecord is what a recorded control-plane delete stores on the node
// row. It is deliberately its own type rather than a TeardownNodeInput: the row
// outlives any particular workflow signature, and tying the two would make a
// change to the workflow's input a schema change. StartRequestedTeardown builds
// the workflow input from this when it promotes the request.
type deleteRequestRecord struct {
	Reason             string              `json:"reason,omitempty"`
	ForceUnreachable   bool                `json:"forceUnreachable,omitempty"`
	MachineHealthIssue *MachineHealthIssue `json:"machineHealthIssue,omitempty"`
}

// ControlPlaneQuorumFloorError reports a control-plane delete refused because
// too few control-plane nodes would survive it.
type ControlPlaneQuorumFloorError struct {
	NodeName  string
	Surviving int
	Minimum   int
}

func (e *ControlPlaneQuorumFloorError) Error() string {
	return fmt.Sprintf(
		"cannot delete control-plane node %q: only %d joined control-plane node(s) would remain, "+
			"below the minimum of %d needed for etcd to survive another failure. "+
			"Raise the control-plane desired count first so a replacement joins before this node is removed, "+
			"or delete the cluster instead",
		e.NodeName, e.Surviving, e.Minimum,
	)
}

// isHealthyControlPlaneNode reports whether a joined control-plane node is still
// serving: its Kubernetes components are up and its agent is answering. It
// mirrors the health the control-plane controller uses to decide whether a node
// can be remediated, so an operator delete and a controller replacement judge the
// same node the same way.
func isHealthyControlPlaneNode(node db.Node) bool {
	return node.Conditions.KubernetesReady.Status == db.ConditionStatusTrue &&
		node.Conditions.NodeAgentReady.Status == db.ConditionStatusTrue
}

// IsDeleteRequested reports whether a delete has been accepted for the node but
// its teardown has not started yet.
func IsDeleteRequested(node db.Node) bool {
	return node.Conditions.DeleteRequested.Status == db.ConditionStatusTrue
}

// IsDeleting reports whether the node's teardown is in flight.
func IsDeleting(node db.Node) bool {
	return node.Conditions.Deleting.Status == db.ConditionStatusTrue
}

// IsDeleteInFlight reports whether the node has an accepted delete that has not
// finished: either recorded and waiting to start, or already tearing down.
func IsDeleteInFlight(node db.Node) bool {
	return IsDeleteRequested(node) || IsDeleting(node)
}

// recordControlPlaneDeleteRequest accepts a control-plane node delete without
// starting the teardown: it enforces the quorum floor, then records the intent
// plus the teardown input the control-plane controller will replay when it
// promotes the request. The caller must already hold the cluster row lock taken
// by guardLiveNodeDelete, which is what serializes concurrent requests.
func recordControlPlaneDeleteRequest(ctx context.Context, tx db.Tx, node db.Node, options DeleteOptions) error {
	if IsDeleteRequested(node) {
		return ErrDeleteAlreadyRequested
	}

	// SkipQuorumFloor exempts controllers that replace the node; they run their
	// own etcd-quorum precondition. The floor itself exempts a node that has
	// already stopped reporting healthy, since it is not providing fault
	// tolerance and refusing to remove it preserves a three-node control plane in
	// name only. The teardown-time etcd guard remains the last word on live
	// membership.
	if !options.SkipQuorumFloor {
		if err := ensureControlPlaneQuorumFloor(ctx, tx, node); err != nil {
			return err
		}
	}

	// The floor waives a node that stopped reporting healthy, so the teardown has
	// to judge it the same way. Without this the accepted delete takes the healthy
	// path against a node whose kubelet is gone: the drain times out, the workflow
	// fails with no retry, and the row stays Deleting forever, which blocks every
	// later control-plane join, delete and remediation on the cluster.
	forceUnreachable := options.ForceUnreachable || !isHealthyControlPlaneNode(node)

	recorded, err := json.Marshal(deleteRequestRecord{
		Reason:             options.Reason,
		ForceUnreachable:   forceUnreachable,
		MachineHealthIssue: options.MachineHealthIssue,
	})
	if err != nil {
		return fmt.Errorf("failed to record control-plane delete request: %w", err)
	}

	node.Conditions.DeleteRequested = db.NewCondition(true, "Control-plane node delete accepted, waiting for the control-plane controller to start teardown")
	node.DeleteRequest = recorded

	if _, err := tx.UpdateNode(ctx, &node); err != nil {
		return fmt.Errorf("failed to update node: %w", err)
	}
	return nil
}

// ensureControlPlaneQuorumFloor refuses a control-plane delete that would leave
// fewer than MinFaultTolerantControlPlaneNodes joined control-plane nodes.
//
// It is deliberately independent of the teardown-time etcd dry-run rather than a
// replacement for it: this one refuses at request time, with an error the caller
// returns to the operator, instead of accepting the delete and failing a workflow
// minutes later. The dry-run still runs and remains the last word on the live
// etcd membership, which is why nothing here waits for the control plane to be
// healthy first -- an unhealthy node is one of the main reasons to delete one,
// and blocking on health would make the guard refuse exactly the deletes an
// operator most needs.
func ensureControlPlaneQuorumFloor(ctx context.Context, tx db.Tx, node db.Node) error {
	// A node that never joined is not an etcd member, so removing it cannot move
	// the cluster toward quorum loss. Applying the floor to it would block the
	// ordinary case of clearing a stuck pending node while the control plane is
	// still coming up.
	if node.Conditions.Joined.Status != db.ConditionStatusTrue {
		return nil
	}

	// Nor does the floor apply to a node that has stopped reporting healthy. Its
	// etcd member is already not answering, so the fault tolerance the floor
	// exists to preserve is gone whether or not the row stays; refusing here would
	// only keep a dead member in the cluster, and removing it is the first step of
	// replacing it. The floor still refuses a healthy node, which is the
	// accidental shrink it was built for.
	if !isHealthyControlPlaneNode(node) {
		return nil
	}

	surviving, err := countSurvivingJoinedControlPlane(ctx, tx, node.ClusterID, liveControlPlaneDeleteExclusion{
		ExcludeNodeID: &node.ID,
	})
	if err != nil {
		return err
	}
	if surviving >= MinFaultTolerantControlPlaneNodes {
		return nil
	}
	return &ControlPlaneQuorumFloorError{
		NodeName:  node.Name,
		Surviving: surviving,
		Minimum:   MinFaultTolerantControlPlaneNodes,
	}
}

// StartRequestedTeardown promotes a recorded control-plane delete request into an
// actual teardown: it flips the node from DeleteRequested to Deleting and queues
// TeardownNode with the input recorded when the request was accepted, so the
// operator's overrides survive the hand-off. Callers are responsible for allowing
// only one teardown in flight per cluster.
func StartRequestedTeardown(ctx context.Context, tx db.Tx, nodeID uuid.UUID) error {
	node, err := tx.GetNodeForUpdate(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}
	if IsDeleting(node) {
		return nil
	}
	if !IsDeleteRequested(node) {
		return ErrNoDeleteRequested
	}

	var recorded deleteRequestRecord
	if len(node.DeleteRequest) > 0 {
		if err := json.Unmarshal(node.DeleteRequest, &recorded); err != nil {
			return fmt.Errorf("failed to read recorded delete request for node %s: %w", node.ID, err)
		}
	}

	node.Conditions.DeleteRequested = db.NewCondition(false, "Teardown started")

	return StartTeardown(ctx, tx, node, TeardownNodeInput{
		Reason:             recorded.Reason,
		ForceUnreachable:   recorded.ForceUnreachable,
		MachineHealthIssue: recorded.MachineHealthIssue,
	}, "Node deletion in progress")
}

// RestartTeardownInput rebuilds the TeardownNode input for a fresh run of a
// teardown whose workflow closed without completing. It replays what the caller
// recorded and forces the unreachable path: the graceful path already had its
// run and failed, and a node's health conditions stop being refreshed once it is
// Deleting, so re-deriving the verdict gives the same answer and fails the same
// way forever. ForceUnreachable never waives etcd member removal, so the retry
// cannot leave a stale member behind.
func RestartTeardownInput(node db.Node) (TeardownNodeInput, error) {
	var recorded deleteRequestRecord
	if len(node.DeleteRequest) > 0 {
		if err := json.Unmarshal(node.DeleteRequest, &recorded); err != nil {
			return TeardownNodeInput{}, fmt.Errorf("failed to read recorded delete request for node %s: %w", node.ID, err)
		}
	}
	return TeardownNodeInput{
		NodeID:             node.ID,
		AllowCPNode:        true,
		Reason:             recorded.Reason,
		MachineHealthIssue: recorded.MachineHealthIssue,
		ForceUnreachable:   true,
	}, nil
}

// StartTeardown marks node as deleting and queues TeardownNode for it. It is the
// single place that pairs those two writes, so every caller queues the same
// typed input under the same workflow ID instead of hand-rolling the payload.
//
// input carries only the caller's intent: NodeID and AllowCPNode are set here,
// because the node being torn down is the one passed in and every caller of this
// is tearing down a control-plane node.
func StartTeardown(ctx context.Context, tx db.Tx, node db.Node, input TeardownNodeInput, deletingMessage string) error {
	input.NodeID = node.ID
	input.AllowCPNode = true

	// Keep the queued input on the row. The workflow's start event is the only
	// other copy of it, and a closed run cannot be replayed, so a restart reads
	// the intent back from here.
	recorded, err := json.Marshal(deleteRequestRecord{
		Reason:             input.Reason,
		ForceUnreachable:   input.ForceUnreachable,
		MachineHealthIssue: input.MachineHealthIssue,
	})
	if err != nil {
		return fmt.Errorf("failed to record teardown input for node %s: %w", node.ID, err)
	}
	node.DeleteRequest = recorded

	node.Conditions.Deleting = db.NewCondition(true, deletingMessage)
	if _, err := tx.UpdateNode(ctx, &node); err != nil {
		return fmt.Errorf("failed to update node: %w", err)
	}

	workflowID := constants.TeardownNodeWorkflowPrefix + node.ID.String()
	if _, err := tx.CreateWorkflowOutboxEntry(ctx, constants.TeardownNodeWorkflowType, workflowID, input); err != nil {
		return fmt.Errorf("failed to create workflow outbox entry: %w", err)
	}
	return nil
}
