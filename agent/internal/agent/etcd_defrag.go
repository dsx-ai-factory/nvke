// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// API outage.
	//
	// There is no caller override, by design: defragmentation is optional, so
	// nothing justifies spending a cluster's quorum on it. The floor refuses
	// the unsafe topologies outright rather than making an operator opt out of
	// them. See etcdRestartSafetyFloor for how it combines with the registered
	// voter count.
	defaultMinVotingMembersForDefrag = 3

	stepEtcdDefragPreflight          = "etcd_defrag_preflight"
	stepEtcdDefragTransferLeader     = "etcd_defrag_transferring_leadership"
	stepEtcdDefragStoppingEtcd       = "etcd_defrag_stopping_etcd_container"
	stepEtcdDefragWaitingRestart     = "etcd_defrag_waiting_for_etcd_restart"
	stepEtcdDefragWaitingHealthy     = "etcd_defrag_waiting_for_etcd_health"
	stepEtcdDefragReturningToService = "etcd_defrag_returning_node_to_service"
	stepEtcdDefragMeasuring          = "etcd_defrag_measuring"
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
// an agent restart, so a fresh Start is not permanently rejected.
//
// Without it an interrupted attempt blocks every retry: the same operation ID
// is rejected as inactive, and a fresh one as already running, because
// runningNodeAgentOperationOfTypeLocked matches on the phase rather than on an
// active worker. The node would then be stuck until someone cleared the state
// file by hand.
//
// This runs after ReconcileInterruptedStaticPodParks has already put back
// whatever the interrupted attempt parked, and clearing the record here is what
// stops that recovery being re-triggered on a later startup.
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
// transfer, withdrawing the node from service, the etcd restart that
// defragments, and returning the node to service.
//
// The node is withdrawn first because restarting etcd makes this node's
// apiserver useless for the duration, and two separate paths reach it. The VIP
// is one. The kubernetes.default Service is the other, and it resolves to
// control-plane node IPs rather than the VIP, so withdrawing the VIP alone
// leaves kube-proxy load balancing in-cluster clients onto an apiserver whose
// only etcd (--etcd-servers=https://127.0.0.1:2379) is gone. Stopping the
// apiserver closes that path, and the endpoint reconciler prunes this node from
// the Service on its own once the apiserver stops renewing its masterlease.
//
// Note the asymmetry in how the failure families are reported, which follows
// from whether the node is left needing a human. A refusal returns nil so the
// operation succeeds with OUTCOME_REFUSED: the agent correctly declined and the
// node is untouched. A failure the agent verifiably undid also returns nil,
// with OUTCOME_FAILED_RESTORED. Only a node left out of service returns an
// error, so the operation lands in phase FAILED *and* carries
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

	// Past this point the node is being taken out of service, so a failure is
	// no longer a refusal: something has to put it back.
	if err := s.withdrawKubeVipLocked(ctx, runner, progress); err != nil {
		return s.returnEtcdDefragNodeToServiceAfterFailure(ctx, runner, progress, client, sizeBefore,
			fmt.Sprintf("withdraw this node from the control-plane VIP: %v", err))
	}

	if err := s.stopKubeAPIServerLocked(ctx, runner, progress); err != nil {
		return s.returnEtcdDefragNodeToServiceAfterFailure(ctx, runner, progress, client, sizeBefore,
			fmt.Sprintf("stop kube-apiserver: %v", err))
	}

	// The manifest stays in place and kubelet owns the restart, so there is no
	// parked etcd.yaml to put back and no window where a crashed agent leaves
	// the member down with nothing scheduled to return it.
	//
	// startTimeout is the static-pod budget rather than the etcd health budget:
	// this waits only for kubelet to run a replacement container, which is a
	// scheduling event on the order of kubelet's restart backoff, not the
	// backend rewrite. The rewrite is what the health wait below covers, and
	// the two must not share a budget - a ten-minute wait for a container that
	// kubelet is never going to start is ten minutes of this node sitting out
	// of service before anyone is told.
	if err := s.restartStaticPodContainer(ctx, runner, progress, staticPodContainerRestartSpec{
		containerName: etcdContainerName,
		stoppingStep:  stepEtcdDefragStoppingEtcd,
		waitingStep:   stepEtcdDefragWaitingRestart,
		stopTimeout:   s.etcdExitTimeoutOrDefault(),
		startTimeout:  s.staticPodStartTimeoutOrDefault(),
	}); err != nil {
		return s.returnEtcdDefragNodeToServiceAfterFailure(ctx, runner, progress, client, sizeBefore,
			fmt.Sprintf("restart the etcd container: %v", err))
	}

	progress.SetStep(stepEtcdDefragWaitingHealthy)
	postStatus, err := s.waitForLocalEtcdHealthy(ctx, client)
	if err != nil {
		// Deliberately leaves the node withdrawn. kube-apiserver cannot serve
		// without its local etcd and kube-vip would advertise a node that
		// cannot answer, so returning either one here would route traffic to a
		// broken member instead of leaving the other two to carry the cluster.
		// The operation ends terminally failed, which is also what keeps the
		// startup reconciler from undoing this decision on the next restart.
		message := fmt.Sprintf(
			"etcd did not return healthy after restart: %v; this node is left out of service with %s and %s parked in %s",
			err, defaultKubeAPIServerManifestName, defaultKubeVipManifestName, defaultStaticPodBackupDir,
		)
		return s.recordEtcdDefragFailure(progress, message, sizeBefore)
	}

	progress.SetStep(stepEtcdDefragReturningToService)
	if err := s.returnEtcdDefragNodeToService(ctx, runner, progress); err != nil {
		return s.recordEtcdDefragFailure(progress, fmt.Sprintf(
			"etcd was defragmented but this node could not be returned to service: %v", err,
		), sizeBefore)
	}

	progress.SetStep(stepEtcdDefragMeasuring)
	sizeAfter := postStatus.DbSize
	reclaimed := int64(0)
	if sizeBefore > sizeAfter {
		reclaimed = sizeBefore - sizeAfter
	}

	// A restart that crossed the threshold but came back no smaller is a real
	// answer rather than a failure, so the caller should stop retrying rather
	// than loop. Usually it means the backend was live data and not free pages.
	// It also covers a backend that grew between the two measurements, which is
	// why the subtraction above is clamped: writes landing during the restart
	// can outrun what the defragmentation freed.
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

// returnEtcdDefragNodeToServiceAfterFailure undoes the withdrawal after a
// failure between taking the node out of service and confirming etcd came back,
// then reports according to whether the node is verifiably serving again.
//
// etcd health is checked first because it decides whether returning the node is
// the right thing to do at all. A healthy member means the node can serve, so
// kube-apiserver and then kube-vip go back and the attempt is FAILED_RESTORED:
// nothing was defragmented, but the node is as it was and a rollout can stop
// cleanly. An unhealthy member means putting either one back would advertise a
// node that cannot answer, so the withdrawal stands and the attempt is
// FAILED_NOT_RESTORED, which needs an operator.
//
// Order matters on the way back: the apiserver cannot republish the masterlease
// that returns this node to the kubernetes.default Service until etcd is
// serving, and kube-vip must not advertise the node before the apiserver is
// ready.
func (s *NodeAgentServer) returnEtcdDefragNodeToServiceAfterFailure(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
	client etcdMemberClient,
	sizeBefore int64,
	failureMessage string,
) error {
	slog.ErrorContext(ctx, "etcd defragmentation failed after the node was withdrawn; returning it to service",
		"reason", failureMessage)

	progress.SetStep(stepEtcdDefragWaitingHealthy)
	if _, err := s.waitForLocalEtcdHealthy(ctx, client); err != nil {
		return s.recordEtcdDefragFailure(progress, fmt.Sprintf(
			"%s; etcd is also not healthy: %v; this node is left out of service with %s and %s parked in %s",
			failureMessage, err, defaultKubeAPIServerManifestName, defaultKubeVipManifestName,
			defaultStaticPodBackupDir,
		), sizeBefore)
	}

	progress.SetStep(stepEtcdDefragReturningToService)
	if err := s.returnEtcdDefragNodeToService(ctx, runner, progress); err != nil {
		return s.recordEtcdDefragFailure(progress,
			fmt.Sprintf("%s; %v", failureMessage, err), sizeBefore)
	}
	return recordEtcdDefragRestored(progress, failureMessage, sizeBefore)
}

// returnEtcdDefragNodeToService puts kube-apiserver and then kube-vip back.
// Callers must already hold staticPodMutationMu and must have confirmed the
// local etcd member healthy first - neither pod can do its job without it.
//
// Both restores are attempted even when the first one fails, and the errors are
// collected rather than returned at the first. They are independent renames,
// and a failure here is usually something both share - an unreachable container
// runtime fails each start wait in turn - so stopping at kube-apiserver would
// leave kube-vip parked for a reason that had nothing to do with kube-vip. A
// manifest put back has a chance of recovering on its own; one left in the
// backup directory does not.
func (s *NodeAgentServer) returnEtcdDefragNodeToService(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	progress nodeAgentOperationProgress,
) error {
	var restoreErrs []error
	if err := s.restoreKubeAPIServerLocked(ctx, runner, progress); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restoring kube-apiserver failed: %w", err))
	}
	if err := s.restoreKubeVipLocked(ctx, runner, progress); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("rejoining the control-plane VIP failed: %w", err))
	}
	return errors.Join(restoreErrs...)
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
