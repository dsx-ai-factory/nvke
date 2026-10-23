// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	operationTypeEtcdDefrag = "etcd_defrag"

	// defaultMinVotingMembersForDefrag is the floor below which the agent will
	// not restart the local member. Three voters means the cluster survives
	// losing this one; at two the restart costs quorum, and at one it is a full
	// API outage. Callers may raise it but the default refuses the unsafe
	// topologies rather than making the operator opt out of them.
	defaultMinVotingMembersForDefrag = 3

	stepEtcdDefragRecovering     = "etcd_defrag_recovering_parked_manifest"
	stepEtcdDefragPreflight      = "etcd_defrag_preflight"
	stepEtcdDefragTransferLeader = "etcd_defrag_transferring_leadership"
	stepEtcdDefragStopping       = "etcd_defrag_stopping_etcd"
	stepEtcdDefragWaitingExit    = "etcd_defrag_waiting_for_etcd_exit"
	stepEtcdDefragRestarting     = "etcd_defrag_restarting_etcd"
	stepEtcdDefragWaitingStart   = "etcd_defrag_waiting_for_etcd_start"
	stepEtcdDefragWaitingHealthy = "etcd_defrag_waiting_for_etcd_health"
	stepEtcdDefragMeasuring      = "etcd_defrag_measuring"
)

// persistedEtcdDefragResult is the typed result carried through the generic
// operation framework, which stores only a string. Encoded on completion and
// decoded by GetEtcdDefragStatus.
type persistedEtcdDefragResult struct {
	Outcome      int32  `json:"outcome"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	SizeBefore   int64  `json:"sizeBefore"`
	SizeAfter    int64  `json:"sizeAfter"`
	Reclaimed    int64  `json:"reclaimed"`
}

// StartEtcdDefrag starts an asynchronous offline defragmentation of the local
// etcd member by restarting it, so etcd defragments during bootstrap. It never
// calls etcdctl defrag, which would block the member for the whole rewrite.
func (s *NodeAgentServer) StartEtcdDefrag(
	ctx context.Context,
	req *pb.StartEtcdDefragRequest,
) (*pb.StartEtcdDefragResponse, error) {
	defer logRPCSpan(ctx, "StartEtcdDefrag")()

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if !s.isControlPlaneRole() {
		role := ""
		if s.NodeStatus != nil {
			role = s.NodeStatus.Role
		}
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"start etcd defrag: control-plane node required, current role %q",
			role,
		)
	}
	if s.Persister == nil {
		return nil, status.Error(codes.FailedPrecondition, "status persister is not configured")
	}

	if err := s.resetInterruptedEtcdDefrag(); err != nil {
		return nil, err
	}

	op, err := s.startNodeAgentOperation(nodeAgentOperationSpec{
		ID:   strings.TrimSpace(req.GetOperationId()),
		Type: operationTypeEtcdDefrag,
	}, func(runCtx context.Context, progress nodeAgentOperationProgress) error {
		return s.runEtcdDefrag(runCtx, progress)
	})
	if err != nil {
		return nil, err
	}
	return &pb.StartEtcdDefragResponse{Operation: nodeAgentOperationToProto(op)}, nil
}

// resetInterruptedEtcdDefrag drops defragmentation records left non-terminal by
// an agent restart, so the Start that follows runs and recovers in band.
//
// Without it the in-band recovery is unreachable in the one case it exists for.
// A restart with etcd.yaml parked leaves a RUNNING record and no worker, and
// startNodeAgentOperation then rejects every retry: the same operation ID as
// inactive, a fresh one as already running, because
// runningNodeAgentOperationOfTypeLocked matches on the phase rather than on an
// active worker. runEtcdDefrag never executes, so ensureEtcdManifestActive never
// puts the parked manifest back and the member stays down.
//
// Every non-terminal record of this type is cleared, not just the requested ID,
// since a stale record under any ID is what blocks a fresh one. Terminal records
// stay, so a repeated Start after a completed attempt remains idempotent.
func (s *NodeAgentServer) resetInterruptedEtcdDefrag() error {
	s.mu.Lock()
	s.ensureNodeAgentOperationStateLocked()
	var stale []string
	for id, op := range s.NodeStatus.NodeAgentOperations {
		if op.Type != operationTypeEtcdDefrag || nodeAgentOperationTerminal(op.Phase) {
			continue
		}
		if _, active := s.activeOperations[id]; active {
			continue
		}
		stale = append(stale, id)
	}
	for _, id := range stale {
		delete(s.NodeStatus.NodeAgentOperations, id)
	}
	statusCopy := s.nodeStatusSnapshotLocked()
	s.mu.Unlock()

	if len(stale) == 0 {
		return nil
	}
	slog.Warn("dropping etcd defragmentation records left non-terminal by an agent restart", "operationIDs", stale)
	if s.Persister == nil {
		return nil
	}
	if err := s.Persister.Persist(&statusCopy); err != nil {
		return status.Errorf(codes.Internal, "persist recovered etcd defrag operation: %v", err)
	}
	return nil
}

// GetEtcdDefragStatus returns the persisted state of an asynchronous etcd
// defragmentation, including how many bytes the restart reclaimed.
func (s *NodeAgentServer) GetEtcdDefragStatus(
	ctx context.Context,
	req *pb.GetEtcdDefragStatusRequest,
) (*pb.GetEtcdDefragStatusResponse, error) {
	defer logRPCSpan(ctx, "GetEtcdDefragStatus")()

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	op, err := s.getNodeAgentOperation(nodeAgentOperationSpec{
		ID:   strings.TrimSpace(req.GetOperationId()),
		Type: operationTypeEtcdDefrag,
	})
	if err != nil {
		return nil, err
	}

	resp := &pb.GetEtcdDefragStatusResponse{Operation: nodeAgentOperationToProto(op)}
	if op.Result == "" {
		// Still running, or it failed before reaching a typed outcome. The
		// operation phase and error message carry what is known.
		resp.ErrorMessage = op.ErrorMessage
		return resp, nil
	}
	var result persistedEtcdDefragResult
	if err := json.Unmarshal([]byte(op.Result), &result); err != nil {
		return nil, status.Errorf(codes.Internal, "decode etcd defrag result: %v", err)
	}
	resp.Outcome = pb.GetEtcdDefragStatusResponse_Outcome(result.Outcome)
	resp.ErrorMessage = result.ErrorMessage
	resp.DbTotalSizeBytesBefore = result.SizeBefore
	resp.DbTotalSizeBytesAfter = result.SizeAfter
	resp.ReclaimedBytes = result.Reclaimed
	return resp, nil
}

// runEtcdDefrag performs the whole per-node sequence: preflight, leadership
// transfer, the manifest move that restarts etcd, and verification that the
// member came back.
//
// Note the asymmetry in how the failure families are reported, which follows
// from whether the node is left needing a human. A refusal returns nil so the
// operation succeeds with OUTCOME_REFUSED: the agent correctly declined and the
// node is untouched. A failure that etcd verifiably recovered from also returns
// nil, with OUTCOME_FAILED_RESTORED. Only a member that does not come back
// returns an error, so the operation lands in phase FAILED *and* carries
// OUTCOME_FAILED_NOT_RESTORED, because a caller polling only the phase must not
// mistake that for a clean stop.
//
// The context here comes from the operation framework, not from the RPC, so
// this sequence is already immune to the caller disconnecting partway through.
func (s *NodeAgentServer) runEtcdDefrag(
	ctx context.Context,
	progress nodeAgentOperationProgress,
) error {
	s.staticPodMutationMu.Lock()
	defer s.staticPodMutationMu.Unlock()

	runner := s.Runner
	if runner == nil {
		runner = &nvkeutils.ExecRunner{}
	}

	client, err := s.newEtcdMemberClient()
	if err != nil {
		return s.recordEtcdDefragRefusal(progress, fmt.Sprintf("connect to local kubeadm etcd: %v", err), 0)
	}
	defer client.Close()

	// Recover from an attempt that died with the manifest parked, before
	// preflight gets a chance to misread it. Without this a retry reads an
	// absent manifest, refuses, and reports OUTCOME_REFUSED - which states that
	// the node is untouched, when in fact etcd is down and this RPC is the only
	// thing that can put it back.
	if err := s.ensureEtcdManifestActive(ctx, runner, progress, client); err != nil {
		return s.recordEtcdDefragFailure(progress, err.Error(), 0)
	}

	progress.SetStep(stepEtcdDefragPreflight)
	sizeBefore, reachable, err := s.etcdDefragPreflight(ctx, client)
	if err != nil {
		return s.recordEtcdDefragRefusal(progress, err.Error(), sizeBefore)
	}

	progress.SetStep(stepEtcdDefragTransferLeader)
	if err := s.transferEtcdLeadershipAway(ctx, client, reachable); err != nil {
		// Leadership could not be moved, so a restart here would make the
		// cluster briefly unavailable for writes. Nothing has been touched yet,
		// so refusing is free and strictly safer than proceeding.
		return s.recordEtcdDefragRefusal(progress, fmt.Sprintf("transfer etcd leadership away from local member: %v", err), sizeBefore)
	}

	// Past this point etcd is taken down, so a failure is no longer a refusal.
	//
	// stopStaticPod moves the manifest aside *before* it waits for the container
	// to exit, so a timeout here leaves etcd.yaml parked in the backup directory
	// with etcd down and nothing scheduled to bring it back. Treating that as
	// terminal would turn an optional optimization into an outage that needs a
	// human, so the manifest goes back before anything is reported.
	if err := s.stopStaticPod(ctx, runner, progress, staticPodStopSpec{
		containerName: etcdContainerName,
		manifestName:  etcdManifestName,
		stoppingStep:  stepEtcdDefragStopping,
		waitingStep:   stepEtcdDefragWaitingExit,
		timeout:       s.etcdExitTimeoutOrDefault(),
	}); err != nil {
		return s.recoverEtcdAfterFailedStop(ctx, runner, progress, client, sizeBefore,
			fmt.Sprintf("stop etcd static pod: %v", err))
	}

	if err := s.restoreStaticPod(ctx, runner, progress, staticPodRestoreSpec{
		containerName: etcdContainerName,
		manifestName:  etcdManifestName,
		restoringStep: stepEtcdDefragRestarting,
		waitingStep:   stepEtcdDefragWaitingStart,
		timeout:       s.etcdHealthTimeoutOrDefault(),
	}); err != nil {
		return s.recordEtcdDefragFailure(progress, fmt.Sprintf("restore etcd static pod: %v", err), sizeBefore)
	}

	progress.SetStep(stepEtcdDefragWaitingHealthy)
	postStatus, err := s.waitForLocalEtcdHealthy(ctx, client)
	if err != nil {
		return s.recordEtcdDefragFailure(progress, fmt.Sprintf("etcd did not return healthy after restart: %v", err), sizeBefore)
	}

	progress.SetStep(stepEtcdDefragMeasuring)
	sizeAfter := postStatus.DbSize
	reclaimed := int64(0)
	if sizeBefore > sizeAfter {
		reclaimed = sizeBefore - sizeAfter
	}

	// A restart that crossed the threshold but reclaimed nothing means the
	// backend was live data, not free pages. That is a real answer, not a
	// failure, and the caller should stop retrying rather than loop.
	outcome := pb.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED
	if reclaimed == 0 {
		outcome = pb.GetEtcdDefragStatusResponse_OUTCOME_NOT_RECLAIMED
	}
	slog.InfoContext(ctx, "etcd defragmentation complete",
		"outcome", outcome.String(),
		"dbSizeBefore", sizeBefore,
		"dbSizeAfter", sizeAfter,
		"reclaimedBytes", reclaimed,
	)
	return recordEtcdDefragResult(progress, persistedEtcdDefragResult{
		Outcome:    int32(outcome),
		SizeBefore: sizeBefore,
		SizeAfter:  sizeAfter,
		Reclaimed:  reclaimed,
	})
}

// ensureEtcdManifestActive puts the manifest back when a previous attempt left
// it parked in the backup directory, so a retry is in-band recovery rather than
// a misleading refusal.
//
// The common case is that the manifest is where it belongs, which costs one
// stat and no crictl call. When it is parked, etcd is down, so this has to run
// before preflight: preflight would fail to read the manifest and report
// OUTCOME_REFUSED, telling the caller the node was untouched.
func (s *NodeAgentServer) ensureEtcdManifestActive(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	client etcdMemberClient,
) error {
	if _, err := os.Stat(s.etcdManifestPath()); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat etcd manifest: %w", err)
	}

	parkedPath := s.rootedPath(filepath.Join(defaultStaticPodBackupDir, etcdManifestName))
	if _, err := os.Stat(parkedPath); err != nil {
		// Neither active nor parked. The agent did not put it here and has
		// nothing to put back, so there is no recovery to attempt.
		return fmt.Errorf("etcd manifest is neither active at %s nor parked at %s", s.etcdManifestPath(), parkedPath)
	}

	slog.WarnContext(ctx, "etcd manifest was left parked by an earlier attempt; restoring it before preflight",
		"parked", parkedPath)
	if err := s.restoreStaticPod(ctx, runner, progress, staticPodRestoreSpec{
		containerName: etcdContainerName,
		manifestName:  etcdManifestName,
		restoringStep: stepEtcdDefragRecovering,
		waitingStep:   stepEtcdDefragWaitingStart,
		timeout:       s.etcdHealthTimeoutOrDefault(),
	}); err != nil {
		return fmt.Errorf("restore the parked etcd manifest: %w", err)
	}

	progress.SetStep(stepEtcdDefragWaitingHealthy)
	if _, err := s.waitForLocalEtcdHealthy(ctx, client); err != nil {
		return fmt.Errorf("etcd did not return healthy after restoring the parked manifest: %w", err)
	}
	return nil
}

// recoverEtcdAfterFailedStop puts the parked manifest back after the stop phase
// failed, then reports according to whether the member verifiably returned.
//
// The stop phase fails with the manifest already moved and etcd in an unknown
// state, so putting it back and watching is the only way to learn which. A
// member that returns healthy leaves the node in its pre-call state with no
// defragmentation performed, which is FAILED_RESTORED and lets a rollout stop
// cleanly. One that does not is FAILED_NOT_RESTORED and needs an operator.
func (s *NodeAgentServer) recoverEtcdAfterFailedStop(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	client etcdMemberClient,
	sizeBefore int64,
	failureMessage string,
) error {
	slog.ErrorContext(ctx, "etcd stop phase failed; putting the parked manifest back", "reason", failureMessage)

	if err := s.restoreStaticPod(ctx, runner, progress, staticPodRestoreSpec{
		containerName: etcdContainerName,
		manifestName:  etcdManifestName,
		restoringStep: stepEtcdDefragRestarting,
		waitingStep:   stepEtcdDefragWaitingStart,
		timeout:       s.etcdHealthTimeoutOrDefault(),
	}); err != nil {
		return s.recordEtcdDefragFailure(progress,
			fmt.Sprintf("%s; putting the manifest back also failed: %v", failureMessage, err), sizeBefore)
	}

	progress.SetStep(stepEtcdDefragWaitingHealthy)
	if _, err := s.waitForLocalEtcdHealthy(ctx, client); err != nil {
		return s.recordEtcdDefragFailure(progress,
			fmt.Sprintf("%s; etcd did not return healthy after the manifest was put back: %v", failureMessage, err), sizeBefore)
	}
	return recordEtcdDefragRestored(progress, failureMessage, sizeBefore)
}

// etcdDefragPreflight verifies every condition that must hold before the member
// is taken down. It returns the pre-restart backend size and the voting members
// that answered a probe, so the leadership transfer can pick a transferee that
// is known to be up without probing the cluster a second time. Every error it
// returns is a refusal: nothing here mutates anything, so the caller can report
// OUTCOME_REFUSED knowing the node is untouched.
func (s *NodeAgentServer) etcdDefragPreflight(
	ctx context.Context,
	client etcdMemberClient,
) (int64, []*etcdserverpb.Member, error) {
	_, manifestJSON, err := s.readEtcdManifest()
	if err != nil {
		return 0, nil, err
	}
	threshold, found, err := etcdBootstrapDefragThresholdBytes(manifestJSON)
	if err != nil {
		return 0, nil, err
	}
	if !found {
		return 0, nil, fmt.Errorf(
			"etcd manifest does not set --%s to a positive value, so a restart would not defragment at any size",
			etcdBootstrapDefragFlag,
		)
	}

	statusCtx, cancelStatus := s.etcdCallContext(ctx)
	localStatus, statusErr := client.Status(statusCtx, etcdLocalEndpoint)
	cancelStatus()
	if healthErr := s.localEtcdHealthError(ctx, client, localStatus, statusErr); healthErr != nil {
		return etcdStatusDbSize(localStatus), nil, fmt.Errorf("local etcd member is unhealthy: %w", healthErr)
	}
	sizeBefore := localStatus.DbSize

	memberListCtx, cancelMemberList := s.etcdCallContext(ctx)
	members, err := client.MemberList(memberListCtx)
	cancelMemberList()
	if err != nil {
		return sizeBefore, nil, fmt.Errorf("list etcd members: %w", err)
	}
	registeredVoters := votingMemberCount(members)
	reachable := s.reachableVotingMembers(ctx, client, members, localStatus.Header.MemberId)
	// Defragmentation is optional, so the floor is derived from the membership
	// alone and there is no caller override: nothing justifies defragmenting a
	// member at the cost of the cluster's quorum. The derived floor already
	// refuses the topologies where no value could preserve one, needing two
	// available voters of one registered and three of two.
	if floor := etcdRestartSafetyFloor(registeredVoters, defaultMinVotingMembersForDefrag); int32(len(reachable)) < floor {
		return sizeBefore, nil, fmt.Errorf(
			"refusing to restart etcd: %d of %d voting members are available, need %d",
			len(reachable), registeredVoters, floor,
		)
	}

	// The gate that makes the restart worth its cost. Reclaimable space below
	// the configured bootstrap threshold means etcd would start without
	// defragmenting, so the restart would be downtime for nothing.
	reclaimable := reclaimableBytes(localStatus.DbSize, localStatus.DbSizeInUse)
	if reclaimable < threshold {
		return sizeBefore, nil, fmt.Errorf(
			"reclaimable space %d bytes is below the configured bootstrap-defrag threshold %d bytes; a restart would not defragment",
			reclaimable, threshold,
		)
	}
	return sizeBefore, reachable, nil
}

// transferEtcdLeadershipAway moves leadership to another voting member when the
// local member is the leader, so the restart does not cost a leader election.
// It is a no-op when the local member is already a follower.
//
// The candidates are the members the preflight probed, not the raw membership.
// MoveLeader against a registered but unreachable member blocks until the call
// deadline and then refuses the whole defragmentation, on a cluster that had a
// healthy candidate the whole time.
func (s *NodeAgentServer) transferEtcdLeadershipAway(
	ctx context.Context,
	client etcdMemberClient,
	reachable []*etcdserverpb.Member,
) error {
	statusCtx, cancelStatus := s.etcdCallContext(ctx)
	localStatus, err := client.Status(statusCtx, etcdLocalEndpoint)
	cancelStatus()
	if err != nil {
		return fmt.Errorf("local etcd member status: %w", err)
	}
	if localStatus == nil || localStatus.Header == nil {
		return fmt.Errorf("local etcd status has no member ID")
	}
	localID := localStatus.Header.MemberId
	if localStatus.Leader != localID {
		return nil
	}

	transfereeID, ok := pickEtcdLeadershipTransferee(reachable, localID)
	if !ok {
		return fmt.Errorf("no other voting etcd member is available to take leadership")
	}
	localEndpoints := etcdMemberClientURLs(reachable, localID)
	if len(localEndpoints) == 0 {
		localEndpoints = []string{etcdLocalEndpoint}
	}
	transferCtx, cancelTransfer := s.etcdCallContext(ctx)
	defer cancelTransfer()
	if err := client.TransferLeadership(transferCtx, localEndpoints, transfereeID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "transferred etcd leadership before defragmentation restart",
		"from", fmt.Sprintf("%x", localID),
		"to", fmt.Sprintf("%x", transfereeID),
	)
	return nil
}

// pickEtcdLeadershipTransferee chooses a started, voting member other than the
// local one. Learners cannot be promoted to leader, and a member that has not
// started (no name yet) cannot serve, so both are skipped. The caller passes
// members that answered a probe, so liveness is not rechecked here.
func pickEtcdLeadershipTransferee(members []*etcdserverpb.Member, localID uint64) (uint64, bool) {
	for _, m := range members {
		if m.GetID() == localID || m.GetIsLearner() || m.GetName() == "" {
			continue
		}
		return m.GetID(), true
	}
	return 0, false
}

// etcdMemberClientURLs returns the advertised client URLs for a member ID.
func etcdMemberClientURLs(members []*etcdserverpb.Member, id uint64) []string {
	for _, m := range members {
		if m.GetID() == id {
			return m.GetClientURLs()
		}
	}
	return nil
}

// recordEtcdDefragRefusal persists a REFUSED outcome and completes the
// operation successfully: nothing was changed, which is the intended behaviour
// when a precondition does not hold.
func (s *NodeAgentServer) recordEtcdDefragRefusal(
	progress nodeAgentOperationProgress,
	reason string,
	sizeBefore int64,
) error {
	slog.Warn("refusing etcd defragmentation; etcd was not restarted", "reason", reason)
	return recordEtcdDefragResult(progress, persistedEtcdDefragResult{
		Outcome:      int32(pb.GetEtcdDefragStatusResponse_OUTCOME_REFUSED),
		ErrorMessage: reason,
		SizeBefore:   sizeBefore,
	})
}

// recordEtcdDefragRestored persists a FAILED_RESTORED outcome and completes the
// operation successfully. The attempt failed and nothing was defragmented, but
// the member is verifiably back on its original manifest, so this is a clean
// stop rather than a node needing intervention - the same reasoning that has
// PatchAPIServerManifest return FAILED_RESTORED with a nil error.
func recordEtcdDefragRestored(
	progress nodeAgentOperationProgress,
	reason string,
	sizeBefore int64,
) error {
	slog.Warn("etcd defragmentation failed but the member was put back and verified healthy", "reason", reason)
	return recordEtcdDefragResult(progress, persistedEtcdDefragResult{
		Outcome:      int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_RESTORED),
		ErrorMessage: reason,
		SizeBefore:   sizeBefore,
	})
}

// recordEtcdDefragFailure persists a FAILED_NOT_RESTORED outcome and returns an
// error so the operation itself is marked failed. etcd was taken down and did
// not verifiably come back, so this node needs intervention and a rollout must
// not advance past it.
func (s *NodeAgentServer) recordEtcdDefragFailure(
	progress nodeAgentOperationProgress,
	reason string,
	sizeBefore int64,
) error {
	slog.Error("etcd defragmentation failed and the member was not confirmed healthy", "reason", reason)
	if err := recordEtcdDefragResult(progress, persistedEtcdDefragResult{
		Outcome:      int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED),
		ErrorMessage: reason,
		SizeBefore:   sizeBefore,
	}); err != nil {
		return err
	}
	return fmt.Errorf("%s", reason)
}

func recordEtcdDefragResult(progress nodeAgentOperationProgress, result persistedEtcdDefragResult) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode etcd defrag result: %w", err)
	}
	progress.SetResult(string(encoded))
	return nil
}
