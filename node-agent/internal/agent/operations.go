package agent

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/NVIDIA/nke/node-agent/pkg/agent/proto"
)

type nodeAgentOperationSpec struct {
	ID   string
	Type string
}

type nodeAgentOperationProgress interface {
	SetStep(step string)
}

type nodeAgentOperationFunc func(context.Context, nodeAgentOperationProgress) error

type nodeAgentOperationProgressImpl struct {
	server      *NodeAgentServer
	operationID string
}

func (p nodeAgentOperationProgressImpl) SetStep(step string) {
	if err := p.server.setNodeAgentOperationStep(p.operationID, step); err != nil {
		slog.Error("failed to persist node-agent operation step",
			"operationID", p.operationID,
			"step", step,
			"err", err)
	}
}

// startNodeAgentOperation and getNodeAgentOperation provide a small framework
// for node-local async operations.
//
// Semantics:
//   - Start requires a stable caller-provided operation ID and operation type.
//   - Starting the same active operation ID/type is idempotent and returns the
//     current persisted operation.
//   - Starting an already terminal operation ID/type returns the terminal state;
//     it does not run the operation again.
//   - Starting a different non-terminal operation of the same type is rejected.
//   - A persisted non-terminal operation with no active in-process worker is
//     rejected. This usually means node-agent restarted during the operation.
//
// Consumer pitfall: decide the restart contract before wiring a workflow. If the
// operation is safe to resume after node-agent restart, the operation function
// must be written to recover from persisted state before using this helper. If
// restart should be loud, call Start once and then poll GetStatus; stale
// non-terminal state will surface as FailedPrecondition instead of silently
// resuming or waiting forever.
func (n *NodeAgentServer) startNodeAgentOperation(
	spec nodeAgentOperationSpec,
	fn nodeAgentOperationFunc,
) (NodeAgentOperation, error) {
	spec.ID = strings.TrimSpace(spec.ID)
	spec.Type = strings.TrimSpace(spec.Type)
	if spec.ID == "" {
		return NodeAgentOperation{}, status.Error(codes.InvalidArgument, "operation_id is required")
	}
	if spec.Type == "" {
		return NodeAgentOperation{}, status.Error(codes.InvalidArgument, "operation type is required")
	}
	if fn == nil {
		return NodeAgentOperation{}, status.Error(codes.Internal, "operation function is nil")
	}

	n.operationStartMu.Lock()
	defer n.operationStartMu.Unlock()

	op, statusCopy, shouldStart, err := n.beginNodeAgentOperation(spec)
	if err != nil {
		return NodeAgentOperation{}, err
	}
	if statusCopy != nil {
		if err := n.persistNodeAgentOperationSnapshot(*statusCopy, op); err != nil {
			return NodeAgentOperation{}, status.Errorf(codes.Internal, "persist node-agent operation: %v", err)
		}
	}
	if shouldStart {
		var published bool
		op, published, err = n.publishNodeAgentOperationStart(spec, op)
		if err != nil {
			return NodeAgentOperation{}, err
		}
		if !published {
			return op, nil
		}
		go n.runNodeAgentOperation(spec.ID, fn)
	}
	return op, nil
}

func (n *NodeAgentServer) beginNodeAgentOperation(spec nodeAgentOperationSpec) (NodeAgentOperation, *NodeStatus, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.ensureNodeAgentOperationStateLocked()

	if existing, ok := n.NodeStatus.NodeAgentOperations[spec.ID]; ok {
		if existing.Type != spec.Type {
			return NodeAgentOperation{}, nil, false, status.Errorf(codes.FailedPrecondition,
				"operation %s already exists with type %s", spec.ID, existing.Type)
		}
		if _, active := n.activeOperations[spec.ID]; active || nodeAgentOperationTerminal(existing.Phase) {
			return existing, nil, false, nil
		}
		return NodeAgentOperation{}, nil, false, inactiveNodeAgentOperationError(existing)
	}

	if running, ok := n.runningNodeAgentOperationOfTypeLocked(spec.Type); ok {
		return NodeAgentOperation{}, nil, false, status.Errorf(codes.FailedPrecondition,
			"operation %s of type %s is already %s", running.ID, running.Type, running.Phase)
	}

	op := NodeAgentOperation{
		ID:    spec.ID,
		Type:  spec.Type,
		Phase: NodeAgentOperationPhaseRunning,
		Step:  "queued",
	}
	statusCopy := n.nodeStatusSnapshotLocked()
	if statusCopy.NodeAgentOperations == nil {
		statusCopy.NodeAgentOperations = map[string]NodeAgentOperation{}
	}
	statusCopy.NodeAgentOperations[spec.ID] = op
	return op, &statusCopy, true, nil
}

func (n *NodeAgentServer) publishNodeAgentOperationStart(
	spec nodeAgentOperationSpec,
	op NodeAgentOperation,
) (NodeAgentOperation, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.ensureNodeAgentOperationStateLocked()

	if existing, ok := n.NodeStatus.NodeAgentOperations[spec.ID]; ok {
		if existing.Type != spec.Type {
			return NodeAgentOperation{}, false, status.Errorf(codes.FailedPrecondition,
				"operation %s already exists with type %s", spec.ID, existing.Type)
		}
		if _, active := n.activeOperations[spec.ID]; active || nodeAgentOperationTerminal(existing.Phase) {
			return existing, false, nil
		}
		return NodeAgentOperation{}, false, inactiveNodeAgentOperationError(existing)
	}

	if running, ok := n.runningNodeAgentOperationOfTypeLocked(spec.Type); ok {
		return NodeAgentOperation{}, false, status.Errorf(codes.FailedPrecondition,
			"operation %s of type %s is already %s", running.ID, running.Type, running.Phase)
	}

	n.NodeStatus.NodeAgentOperations[spec.ID] = op
	n.activeOperations[spec.ID] = struct{}{}
	return op, true, nil
}

func (n *NodeAgentServer) getNodeAgentOperation(spec nodeAgentOperationSpec) (NodeAgentOperation, error) {
	spec.ID = strings.TrimSpace(spec.ID)
	spec.Type = strings.TrimSpace(spec.Type)
	if spec.ID == "" {
		return NodeAgentOperation{}, status.Error(codes.InvalidArgument, "operation_id is required")
	}
	if spec.Type == "" {
		return NodeAgentOperation{}, status.Error(codes.InvalidArgument, "operation type is required")
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	n.ensureNodeAgentOperationStateLocked()
	op, ok := n.NodeStatus.NodeAgentOperations[spec.ID]
	if !ok || op.Type != spec.Type {
		return NodeAgentOperation{}, status.Errorf(codes.NotFound, "operation %s of type %s not found", spec.ID, spec.Type)
	}
	_, active := n.activeOperations[spec.ID]
	if !nodeAgentOperationTerminal(op.Phase) && !active {
		return NodeAgentOperation{}, inactiveNodeAgentOperationError(op)
	}
	return op, nil
}

func (n *NodeAgentServer) runNodeAgentOperation(operationID string, fn nodeAgentOperationFunc) {
	progress := nodeAgentOperationProgressImpl{
		server:      n,
		operationID: operationID,
	}
	var err error
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in node-agent operation",
				"operationID", operationID,
				"panic", r,
				"stack", string(debug.Stack()))
			err = fmt.Errorf("panic in %s: %v", operationID, r)
		}
		n.finishNodeAgentOperation(operationID, err)
	}()
	err = fn(context.Background(), progress)
}

func (n *NodeAgentServer) setNodeAgentOperationStep(operationID, step string) error {
	n.mu.Lock()
	n.ensureNodeAgentOperationStateLocked()
	op, ok := n.NodeStatus.NodeAgentOperations[operationID]
	if !ok {
		n.mu.Unlock()
		return fmt.Errorf("operation %s not found", operationID)
	}
	op.Step = strings.TrimSpace(step)
	n.NodeStatus.NodeAgentOperations[operationID] = op
	statusCopy := n.nodeStatusSnapshotLocked()
	n.mu.Unlock()

	return n.persistNodeAgentOperationSnapshot(statusCopy, op)
}

func (n *NodeAgentServer) finishNodeAgentOperation(operationID string, operationErr error) {
	n.mu.Lock()
	n.ensureNodeAgentOperationStateLocked()

	op, ok := n.NodeStatus.NodeAgentOperations[operationID]
	if !ok {
		delete(n.activeOperations, operationID)
		n.mu.Unlock()
		return
	}
	if operationErr != nil {
		op.Phase = NodeAgentOperationPhaseFailed
		op.ErrorMessage = operationErr.Error()
		if strings.TrimSpace(op.Step) == "" || op.Step == "queued" {
			op.Step = "failed"
		}
	} else {
		op.Phase = NodeAgentOperationPhaseSucceeded
		op.Step = "succeeded"
		op.ErrorMessage = ""
	}
	n.NodeStatus.NodeAgentOperations[operationID] = op
	delete(n.activeOperations, operationID)
	statusCopy := n.nodeStatusSnapshotLocked()
	n.mu.Unlock()

	if err := n.persistNodeAgentOperationSnapshot(statusCopy, op); err != nil {
		slog.Error("failed to persist completed node-agent operation",
			"operationID", operationID,
			"phase", op.Phase,
			"err", err)
	}
}

func (n *NodeAgentServer) runningNodeAgentOperationOfTypeLocked(operationType string) (NodeAgentOperation, bool) {
	for id, op := range n.NodeStatus.NodeAgentOperations {
		if op.Type != operationType || nodeAgentOperationTerminal(op.Phase) {
			continue
		}
		if _, active := n.activeOperations[id]; active || op.Phase == NodeAgentOperationPhaseRunning {
			return op, true
		}
	}
	return NodeAgentOperation{}, false
}

func (n *NodeAgentServer) ensureNodeAgentOperationStateLocked() {
	if n.NodeStatus == nil {
		n.NodeStatus = &NodeStatus{}
	}
	if n.NodeStatus.NodeAgentOperations == nil {
		n.NodeStatus.NodeAgentOperations = map[string]NodeAgentOperation{}
	}
	if n.activeOperations == nil {
		n.activeOperations = map[string]struct{}{}
	}
}

func (n *NodeAgentServer) nodeStatusSnapshotLocked() NodeStatus {
	return cloneNodeStatus(n.NodeStatus)
}

func (n *NodeAgentServer) persistNodeAgentOperationSnapshot(statusCopy NodeStatus, op NodeAgentOperation) error {
	if n.Persister == nil {
		return nil
	}
	if err := n.Persister.Persist(&statusCopy); err != nil {
		return fmt.Errorf("persist operation %s (%s): %w", op.ID, op.Type, err)
	}
	return nil
}

func nodeAgentOperationTerminal(phase NodeAgentOperationPhase) bool {
	return phase == NodeAgentOperationPhaseSucceeded || phase == NodeAgentOperationPhaseFailed
}

func inactiveNodeAgentOperationError(op NodeAgentOperation) error {
	return status.Errorf(codes.FailedPrecondition,
		"operation %s (%s) is non-terminal in persisted state, but no worker is active; node-agent may have restarted during the operation",
		op.ID, op.Type)
}

func nodeAgentOperationToProto(op NodeAgentOperation) *pb.NodeAgentOperationStatus {
	return &pb.NodeAgentOperationStatus{
		OperationId:  op.ID,
		Phase:        nodeAgentOperationPhaseToProto(op.Phase),
		Step:         op.Step,
		ErrorMessage: op.ErrorMessage,
	}
}

func nodeAgentOperationPhaseToProto(phase NodeAgentOperationPhase) pb.NodeAgentOperationPhase {
	switch phase {
	case NodeAgentOperationPhaseRunning:
		return pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING
	case NodeAgentOperationPhaseSucceeded:
		return pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED
	case NodeAgentOperationPhaseFailed:
		return pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED
	default:
		return pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_UNSPECIFIED
	}
}
